package ssmsession

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// SSHDocument is the SSM document that forwards a stream to the instance's
// local sshd.
const SSHDocument = "AWS-StartSSHSession"

// SessionAPI is the subset of the SSM client Dial uses.
type SessionAPI interface {
	StartSession(ctx context.Context, in *ssm.StartSessionInput, optFns ...func(*ssm.Options)) (*ssm.StartSessionOutput, error)
	TerminateSession(ctx context.Context, in *ssm.TerminateSessionInput, optFns ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error)
}

// Dialer opens SSH streams to hosted instances.
type Dialer struct {
	API    SessionAPI
	Region string
	Plugin string // path from EnsurePlugin
}

// Dial starts an SSH session to instanceID and returns its stream. The session
// is recorded in CloudTrail as StartSession/TerminateSession (DES-HOR-603-07).
// Closing the connection stops the plugin and terminates the session.
func (d Dialer) Dial(ctx context.Context, instanceID string) (net.Conn, error) {
	params := map[string][]string{"portNumber": {"22"}}
	out, err := d.API.StartSession(ctx, &ssm.StartSessionInput{
		Target:       aws.String(instanceID),
		DocumentName: aws.String(SSHDocument),
		Parameters:   params,
	})
	if err != nil {
		return nil, fmt.Errorf("ssm start session to %s: %w", instanceID, err)
	}
	session, err := json.Marshal(map[string]string{
		"SessionId":  aws.ToString(out.SessionId),
		"TokenValue": aws.ToString(out.TokenValue),
		"StreamUrl":  aws.ToString(out.StreamUrl),
	})
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(map[string]any{"Target": instanceID, "DocumentName": SSHDocument, "Parameters": params})
	if err != nil {
		return nil, err
	}
	// The argument order is the plugin's contract with the AWS CLI: session,
	// region, operation, profile, request parameters, endpoint. The session
	// token authorizes the stream; the plugin needs no AWS credentials.
	cmd := exec.Command(d.Plugin, //nolint:gosec // the reviewed, checksum-verified plugin Forge installed; arguments are JSON it built
		string(session), d.Region, "StartSession", "", string(request), "https://ssm."+d.Region+".amazonaws.com")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &conn{cmd: cmd, in: stdin, out: stdout, api: d.API, sessionID: aws.ToString(out.SessionId), instanceID: instanceID}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		c.terminate()
		return nil, fmt.Errorf("start session-manager-plugin: %w", err)
	}
	return c, nil
}

// conn adapts the plugin's stdio to net.Conn for golang.org/x/crypto/ssh.
// Deadlines are not supported by pipes; the SSH client's own timeouts and
// context cancellation bound every operation.
type conn struct {
	cmd        *exec.Cmd
	in         io.WriteCloser
	out        io.ReadCloser
	stderr     syncBuffer
	api        SessionAPI
	sessionID  string
	instanceID string
	closeOnce  sync.Once
	waitOnce   sync.Once
}

// wait reaps the plugin once, after which its stderr is complete.
func (c *conn) wait() {
	c.waitOnce.Do(func() { _ = c.cmd.Wait() })
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.out.Read(b)
	if err == io.EOF {
		// The plugin's stderr is copied until it exits; wait so a failure
		// reason is not lost to a bare EOF.
		c.wait()
		if detail := c.stderr.String(); detail != "" {
			return n, fmt.Errorf("ssm session to %s ended: %s", c.instanceID, detail)
		}
	}
	return n, err
}

func (c *conn) Write(b []byte) (int, error) { return c.in.Write(b) }

func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.in.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		c.wait()
		c.terminate()
	})
	return nil
}

// terminate ends the SSM session so it does not linger until its idle timeout.
func (c *conn) terminate() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = c.api.TerminateSession(ctx, &ssm.TerminateSessionInput{SessionId: aws.String(c.sessionID)})
}

func (c *conn) LocalAddr() net.Addr              { return addr("forge") }
func (c *conn) RemoteAddr() net.Addr             { return addr(c.instanceID) }
func (c *conn) SetDeadline(time.Time) error      { return nil }
func (c *conn) SetReadDeadline(time.Time) error  { return nil }
func (c *conn) SetWriteDeadline(time.Time) error { return nil }

type addr string

func (a addr) Network() string { return "ssm" }
func (a addr) String() string  { return string(a) }

// syncBuffer collects the plugin's stderr for diagnostics; exec writes it from
// its own goroutine while Read may inspect it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 64<<10 {
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.TrimSpace(b.buf.Bytes()))
}
