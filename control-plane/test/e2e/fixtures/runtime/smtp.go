package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The SMTP sink is the deployed E2E mail relay (HOR-582). The control plane
// sends authentication email only over verified TLS, so the sink offers
// STARTTLS with a test-CA leaf mounted at /tls and keeps every accepted message
// in memory for the scenario to read through GET /messages. It is never part of
// a product install.
const (
	smtpSinkAddr    = ":2525"
	smtpSinkCert    = "/tls/tls.crt"
	smtpSinkKey     = "/tls/tls.key"
	smtpMaxMessage  = 1 << 20
	smtpIdleTimeout = 2 * time.Minute
)

type sinkMessage struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Data string   `json:"data"`
}

type mailbox struct {
	mu       sync.Mutex
	messages []sinkMessage
}

func (box *mailbox) add(message sinkMessage) {
	box.mu.Lock()
	defer box.mu.Unlock()
	box.messages = append(box.messages, message)
}

func (box *mailbox) list() []sinkMessage {
	box.mu.Lock()
	defer box.mu.Unlock()
	return append([]sinkMessage(nil), box.messages...)
}

func runSMTPSink() {
	certificate, err := tls.LoadX509KeyPair(smtpSinkCert, smtpSinkKey)
	if err != nil {
		log.Fatalf("load SMTP sink certificate: %v", err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	box := &mailbox{}

	listener, err := net.Listen("tcp", smtpSinkAddr) //nolint:gosec // in-cluster E2E sink reached through its Service
	if err != nil {
		log.Fatalf("listen SMTP sink: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("accept SMTP connection: %v", err)
				continue
			}
			go serveSMTP(conn, tlsConfig, box)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(box.list())
	})
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("E2E SMTP sink listening on %s (mailbox on %s)", smtpSinkAddr, server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// serveSMTP implements the bounded ESMTP subset Go's net/smtp client uses:
// EHLO, STARTTLS, MAIL, RCPT, DATA, RSET, NOOP, and QUIT. A message is accepted
// only after STARTTLS, mirroring the verified-TLS-only product contract.
//
//nolint:gocyclo // One explicit SMTP command loop.
func serveSMTP(conn net.Conn, tlsConfig *tls.Config, box *mailbox) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	reply := func(line string) bool {
		_ = conn.SetDeadline(time.Now().Add(smtpIdleTimeout))
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}
	if !reply("220 e2e-smtp-sink ESMTP") {
		return
	}
	secure := false
	var current sinkMessage
	for {
		_ = conn.SetDeadline(time.Now().Add(smtpIdleTimeout))
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(command, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			if secure {
				reply("250-e2e-smtp-sink")
				reply("250 8BITMIME")
			} else {
				reply("250-e2e-smtp-sink")
				reply("250 STARTTLS")
			}
		case "STARTTLS":
			if secure {
				reply("503 TLS already active")
				continue
			}
			if !reply("220 ready to start TLS") {
				return
			}
			tlsConn := tls.Server(conn, tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				log.Printf("SMTP STARTTLS handshake: %v", err)
				return
			}
			conn = tlsConn
			reader = bufio.NewReader(conn)
			writer = bufio.NewWriter(conn)
			secure = true
			current = sinkMessage{}
		case "MAIL":
			if !secure {
				reply("530 must issue STARTTLS first")
				continue
			}
			current = sinkMessage{From: smtpAddress(command)}
			reply("250 ok")
		case "RCPT":
			if !secure {
				reply("530 must issue STARTTLS first")
				continue
			}
			current.To = append(current.To, smtpAddress(command))
			reply("250 ok")
		case "DATA":
			if !secure || len(current.To) == 0 {
				reply("503 need RCPT first")
				continue
			}
			if !reply("354 end with <CRLF>.<CRLF>") {
				return
			}
			data, err := readSMTPData(reader)
			if err != nil {
				reply("552 message rejected")
				return
			}
			current.Data = data
			box.add(current)
			current = sinkMessage{}
			reply("250 accepted")
		case "RSET":
			current = sinkMessage{}
			reply("250 ok")
		case "NOOP":
			reply("250 ok")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 command not implemented")
		}
	}
}

func smtpAddress(command string) string {
	_, value, _ := strings.Cut(command, ":")
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "<")
	if end := strings.Index(value, ">"); end >= 0 {
		value = value[:end]
	}
	return value
}

func readSMTPData(reader *bufio.Reader) (string, error) {
	var builder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		if line == ".\r\n" || line == ".\n" {
			return builder.String(), nil
		}
		line = strings.TrimPrefix(line, ".")
		if builder.Len()+len(line) > smtpMaxMessage {
			return "", errors.New("message too large")
		}
		builder.WriteString(line)
	}
}
