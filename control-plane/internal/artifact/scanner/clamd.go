// Package scanner is the isolated scanner adapter (DES-HOR-460-07,
// DES-HOR-461-01). It runs in a pod with no database, object-store,
// Kubernetes, or customer credential and no network egress. It accepts one
// bounded stream from the exact coordinator identity over mTLS, asks the
// pod-local clamd for a malware verdict, then applies the project-owned
// admission policy (type detection, structural validation, email expansion).
package scanner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Clamd talks to the pod-local clamd over loopback TCP. clamd is never a
// Service and never sees TLS keys.
type Clamd struct {
	Addr    string
	Timeout time.Duration
}

const instreamChunk = 64 << 10

// ErrClamd reports an engine failure: never a verdict.
var ErrClamd = errors.New("scanner: clamd error")

func (c Clamd) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial: %v", ErrClamd, err)
	}
	deadline := time.Now().Add(c.timeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	return conn, nil
}

func (c Clamd) timeout() time.Duration {
	if c.Timeout == 0 {
		return 110 * time.Second
	}
	return c.Timeout
}

func (c Clamd) command(ctx context.Context, cmd string) (string, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("z" + cmd + "\x00")); err != nil {
		return "", fmt.Errorf("%w: %v", ErrClamd, err)
	}
	return readReply(conn)
}

func readReply(conn net.Conn) (string, error) {
	reply, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%w: read: %v", ErrClamd, err)
	}
	return strings.TrimRight(reply, "\x00\n"), nil
}

// Ping proves the engine answers.
func (c Clamd) Ping(ctx context.Context) error {
	reply, err := c.command(ctx, "PING")
	if err != nil {
		return err
	}
	if reply != "PONG" {
		return fmt.Errorf("%w: unexpected ping reply", ErrClamd)
	}
	return nil
}

// Version returns the engine version string, e.g. "ClamAV 1.4.6/27450/...".
func (c Clamd) Version(ctx context.Context) (string, error) {
	return c.command(ctx, "VERSION")
}

// Reload asks clamd to adopt the current signature directory.
func (c Clamd) Reload(ctx context.Context) error {
	reply, err := c.command(ctx, "RELOAD")
	if err != nil {
		return err
	}
	if reply != "RELOADING" {
		return fmt.Errorf("%w: unexpected reload reply", ErrClamd)
	}
	return nil
}

// Scan streams bytes through INSTREAM. It returns the signature name when
// malware is found. Any engine error (including size limits) is ErrClamd so
// the caller reports a retryable failure rather than a verdict.
func (c Clamd) Scan(ctx context.Context, data []byte) (infected bool, signature string, err error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return false, "", fmt.Errorf("%w: %v", ErrClamd, err)
	}
	var size [4]byte
	for off := 0; off < len(data); off += instreamChunk {
		end := min(off+instreamChunk, len(data))
		binary.BigEndian.PutUint32(size[:], uint32(end-off)) //nolint:gosec // end-off <= instreamChunk (64 KiB).
		if _, err := conn.Write(size[:]); err != nil {
			return false, "", fmt.Errorf("%w: %v", ErrClamd, err)
		}
		if _, err := conn.Write(data[off:end]); err != nil {
			return false, "", fmt.Errorf("%w: %v", ErrClamd, err)
		}
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return false, "", fmt.Errorf("%w: %v", ErrClamd, err)
	}
	reply, err := readReply(conn)
	if err != nil {
		return false, "", err
	}
	return parseScanReply(reply)
}

// parseScanReply interprets "stream: OK", "stream: <name> FOUND", or an
// error reply.
func parseScanReply(reply string) (bool, string, error) {
	body, ok := strings.CutPrefix(reply, "stream: ")
	if !ok {
		return false, "", fmt.Errorf("%w: unexpected reply", ErrClamd)
	}
	switch {
	case body == "OK":
		return false, "", nil
	case strings.HasSuffix(body, " FOUND"):
		name := strings.TrimSuffix(body, " FOUND")
		if len(name) > 128 {
			name = name[:128]
		}
		return true, sanitizeSignature(name), nil
	default:
		return false, "", fmt.Errorf("%w: engine reported an error", ErrClamd)
	}
}

func sanitizeSignature(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// selfTestSample builds the industry-standard EICAR test string at runtime so
// the adapter binary itself never contains a contiguous detectable sample.
func selfTestSample() []byte {
	parts := []string{`X5O!P%@AP[4\PZX54(P^)7CC)7}$`, `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`}
	return []byte(parts[0] + parts[1])
}
