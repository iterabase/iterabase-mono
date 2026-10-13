package identity

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// SMTP transport modes. Both require verified TLS; the architecture forbids
// cleartext authentication email.
const (
	SMTPModeStartTLS = "starttls"
	SMTPModeTLS      = "tls"
)

// SMTPConfig is the operator-owned transactional authentication-email
// configuration. Secrets arrive from Kubernetes Secrets, never from customer
// surfaces.
type SMTPConfig struct {
	Host     string
	Port     int
	Mode     string
	From     string
	Username string
	Password string
	Timeout  time.Duration
	// RootCAs, when set, replaces the system roots for verifying a private
	// relay's certificate (LoadSMTPRootCAs). Verification is never disabled.
	RootCAs *x509.CertPool
}

// LoadSMTPRootCAs returns the system roots plus a PEM bundle, for an SMTP relay
// whose certificate is issued by a private CA. An empty path returns nil (the
// system roots alone).
func LoadSMTPRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path) //nolint:gosec // operator-configured CA bundle path
	if err != nil {
		return nil, fmt.Errorf("read smtp ca file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("smtp ca file contains no PEM certificates")
	}
	return pool, nil
}

// ValidateSMTPConfig rejects an unusable or cleartext configuration.
func ValidateSMTPConfig(cfg SMTPConfig) error {
	if strings.TrimSpace(cfg.Host) == "" {
		return errors.New("smtp host is required")
	}
	if strings.ContainsAny(cfg.Host, " \t\r\n/") {
		return errors.New("smtp host is invalid")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return errors.New("smtp port must be 1-65535")
	}
	if cfg.Mode != SMTPModeStartTLS && cfg.Mode != SMTPModeTLS {
		return fmt.Errorf("smtp mode must be %q or %q", SMTPModeStartTLS, SMTPModeTLS)
	}
	if err := validateMailAddress(cfg.From, "smtp from"); err != nil {
		return err
	}
	if cfg.Username == "" && cfg.Password != "" {
		return errors.New("smtp password requires a username")
	}
	if cfg.Username != "" && cfg.Password == "" {
		return errors.New("smtp username requires a password")
	}
	return nil
}

func validateMailAddress(address, field string) error {
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("%s address is required", field)
	}
	if strings.ContainsAny(address, "\r\n") {
		return fmt.Errorf("%s address contains a line break", field)
	}
	if !strings.Contains(address, "@") {
		return fmt.Errorf("%s address is invalid", field)
	}
	return nil
}

// SMTPSender delivers authentication email over verified TLS.
type SMTPSender struct {
	cfg SMTPConfig
}

// NewSMTPSender validates and builds a sender.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if err := ValidateSMTPConfig(cfg); err != nil {
		return nil, err
	}
	return &SMTPSender{cfg: cfg}, nil
}

// Send reports accepted, retryable, or ambiguous delivery. No error is
// returned: ambiguity is a first-class outcome.
//
//nolint:gocyclo // explicit SMTP phase handling; ambiguity boundaries are the point.
func (s *SMTPSender) Send(ctx context.Context, message AuthEmailMessage) AuthEmailSendResult {
	if err := validateMailAddress(message.To, "recipient"); err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "invalid_recipient"}
	}
	timeout := s.cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	address := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: timeout}
	if s.cfg.Mode == SMTPModeTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, tlsConfig(s.cfg.Host, s.cfg.RootCAs))
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "connect"}
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "client"}
	}
	defer func() { _ = client.Close() }()

	if s.cfg.Mode == SMTPModeStartTLS {
		if err := client.StartTLS(tlsConfig(s.cfg.Host, s.cfg.RootCAs)); err != nil {
			return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "starttls"}
		}
	}
	if s.cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "auth"}
		}
	}
	if err := client.Mail(s.cfg.From); err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "mail_from"}
	}
	if err := client.Rcpt(message.To); err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "rcpt_to"}
	}
	writer, err := client.Data()
	if err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeRetryable, ErrorClass: "data"}
	}
	payload := buildMIME(s.cfg.From, message)
	if _, err := writer.Write(payload); err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeUnknown, ErrorClass: "write"}
	}
	if err := writer.Close(); err != nil {
		return AuthEmailSendResult{Outcome: SendOutcomeUnknown, ErrorClass: "close"}
	}
	// The server accepted the message once the data writer closed cleanly. A
	// QUIT failure does not change acceptance.
	_ = client.Quit()
	return AuthEmailSendResult{Outcome: SendOutcomeAccepted}
}

func tlsConfig(host string, roots *x509.CertPool) *tls.Config {
	return &tls.Config{ //nolint:gosec // ServerName set from configured host; verification stays enabled
		ServerName: host,
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}
}

func buildMIME(from string, message AuthEmailMessage) []byte {
	boundary := "iterabase-" + randomToken()
	var builder strings.Builder
	builder.WriteString("From: " + from + "\r\n")
	builder.WriteString("To: " + message.To + "\r\n")
	builder.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", message.Subject) + "\r\n")
	builder.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	builder.WriteString("Message-ID: <" + randomToken() + "@iterabase>\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	builder.WriteString("--" + boundary + "\r\n")
	builder.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	builder.WriteString(strings.ReplaceAll(message.Text, "\n", "\r\n"))
	builder.WriteString("\r\n\r\n--" + boundary + "\r\n")
	builder.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	builder.WriteString(strings.ReplaceAll(message.HTML, "\n", "\r\n"))
	builder.WriteString("\r\n\r\n--" + boundary + "--\r\n")
	return []byte(builder.String())
}

func randomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}
