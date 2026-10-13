package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

func sinkTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp-sink"},
		DNSNames: []string{"smtp-sink"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}, roots
}

func TestSMTPSinkAcceptsOnlyAfterVerifiedSTARTTLS(t *testing.T) {
	serverTLS, roots := sinkTLS(t)
	box := &mailbox{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSMTP(conn, serverTLS, box)
		}
	}()

	// Without STARTTLS the sink refuses the envelope.
	plain, err := smtp.Dial(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.Mail("auth@example.com"); err == nil {
		t.Fatal("sink accepted MAIL before STARTTLS")
	}
	_ = plain.Close()

	client, err := smtp.Dial(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartTLS(&tls.Config{ServerName: "smtp-sink", RootCAs: roots, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatalf("verified STARTTLS: %v", err)
	}
	if err := client.Mail("auth@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("admin@example.com"); err != nil {
		t.Fatal(err)
	}
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("Subject: Setup\r\n\r\nOpen https://app.example.com/auth/setup?token=abc\r\n.leading dot\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	_ = client.Quit()

	messages := box.list()
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	message := messages[0]
	if message.From != "auth@example.com" || len(message.To) != 1 || message.To[0] != "admin@example.com" {
		t.Fatalf("envelope = %+v", message)
	}
	if !strings.Contains(message.Data, "/auth/setup?token=abc") || !strings.Contains(message.Data, ".leading dot") {
		t.Fatalf("data = %q", message.Data)
	}
}
