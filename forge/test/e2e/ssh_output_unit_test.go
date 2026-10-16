package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// An actual loopback SSH server exercises channel stdout/stderr separation and
// exit status, not a mock that has already separated the streams for the code.
func sshOutputTestClient(t *testing.T, handle func(string, ssh.Channel, <-chan struct{}) uint32) *ssh.Client {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var workers sync.WaitGroup
	workers.Go(func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		server, channels, requests, err := ssh.NewServerConn(connection, config)
		if err != nil {
			_ = connection.Close()
			return
		}
		defer server.Close()
		workers.Go(func() { ssh.DiscardRequests(requests) })
		for request := range channels {
			channel, requests, err := request.Accept()
			if err != nil {
				continue
			}
			workers.Go(func() {
				defer channel.Close()
				for request := range requests {
					if request.Type != "exec" {
						_ = request.Reply(false, nil)
						continue
					}
					var payload struct{ Command string }
					if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
						_ = request.Reply(false, nil)
						return
					}
					_ = request.Reply(true, nil)
					status := handle(payload.Command, channel, done)
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
					return
				}
			})
		}
	})
	// This key policy is restricted to the private, ephemeral loopback server.
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User: "fixture", HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: 5 * time.Second,
	})
	if err != nil {
		close(done)
		_ = listener.Close()
		workers.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(done)
		_ = client.Close()
		_ = listener.Close()
		workers.Wait()
	})
	return client
}

func TestSSHStructuredOutputIgnoresStderrWarningButRetainsIt(t *testing.T) {
	config := "sha256:" + strings.Repeat("a", 64)
	payload := fmt.Sprintf(`{"status":{"id":%q,"repoTags":["docker.io/iterabase-e2e/model:exact-head"]}}`, config)
	warning := "sudo: unable to resolve host fixture: Temporary failure in name resolution\n"
	client := sshOutputTestClient(t, func(_ string, channel ssh.Channel, _ <-chan struct{}) uint32 {
		_, _ = io.WriteString(channel.Stderr(), warning)
		_, _ = io.WriteString(channel, payload)
		return 0
	})
	stdout, stderr, err := sshOutputSeparated(t.Context(), client, "inspect")
	if err != nil || stdout != payload || stderr != warning {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	stdout, err = sshStdout(t, client, "inspect")
	if err != nil || stdout != payload {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if _, _, err := importedRuntimeImageConfig([]byte(stdout), config); err != nil {
		t.Fatalf("stderr warning broke identity verification: %v", err)
	}
}

func TestSSHStructuredOutputPreservesNonzeroExitAndRedactsStderr(t *testing.T) {
	t.Setenv("GH_TOKEN", "fixture-private-token")
	client := sshOutputTestClient(t, func(_ string, channel ssh.Channel, _ <-chan struct{}) uint32 {
		_, _ = io.WriteString(channel, "exact stdout")
		_, _ = io.WriteString(channel.Stderr(), "warning fixture-private-token")
		return 42
	})
	stdout, stderr, err := sshOutputSeparated(t.Context(), client, "fail")
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 42 || stdout != "exact stdout" || !strings.Contains(stderr, "warning") {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	stdout, err = sshStdout(t, client, "fail")
	if err == nil || !strings.Contains(err.Error(), "warning") || strings.Contains(err.Error(), "fixture-private-token") || stdout != "exact stdout" {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
}

func TestSSHStructuredOutputCancellationClosesHungCommand(t *testing.T) {
	started := make(chan struct{})
	client := sshOutputTestClient(t, func(_ string, _ ssh.Channel, done <-chan struct{}) uint32 {
		close(started)
		<-done // Simulate a remote command that ignores cancellation/signals.
		return 0
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := sshOutputSeparated(ctx, client, "hang")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("SSH command never started")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSH command outlived cancellation")
	}
}

func TestSSHStructuredOutputPreCanceledDoesNotOpenSession(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := sshOutputSeparated(ctx, nil, "must not run")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled command returned %v", err)
	}
}
