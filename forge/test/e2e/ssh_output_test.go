package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
	"golang.org/x/crypto/ssh"
)

const structuredSSHTimeout = 15 * time.Minute

type runtimeImageCommand func(context.Context, string) (string, error)

// sshOutputSeparated leaves successful stdout byte-for-byte intact. Cancellation
// closes this observation's client, including a blocked session-open or RPC, so
// a hung command cannot outlive the caller's readiness deadline.
func sshOutputSeparated(ctx context.Context, client *ssh.Client, command string) (stdout, stderr string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", "", err
	}
	defer session.Close()
	var out, diagnostic bytes.Buffer
	session.Stdout = &out
	session.Stderr = &diagnostic
	err = session.Run(command)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return out.String(), diagnostic.String(), err
}

// structuredSSHCommand retains warnings in the test log through the shared
// redactor, never in parser input. Existing unstructured sshOutput callers keep
// their combined-output diagnostics. A failed command remains failed.
func structuredSSHCommand(t *testing.T, client *ssh.Client) runtimeImageCommand {
	t.Helper()
	redactor := redact.New(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	return func(ctx context.Context, command string) (string, error) {
		stdout, stderr, err := sshOutputSeparated(ctx, client, command)
		stderr = redactor.String(stderr)
		const maxStderr = 16 << 10
		if len(stderr) > maxStderr {
			stderr = stderr[:maxStderr] + "\n[stderr truncated]"
		}
		if stderr != "" {
			t.Logf("structured SSH stderr: %s", stderr)
		}
		if err != nil {
			return stdout, fmt.Errorf("structured SSH command: %w\nstderr:\n%s", err, stderr)
		}
		return stdout, nil
	}
}

func sshStdout(t *testing.T, client *ssh.Client, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), structuredSSHTimeout)
	defer cancel()
	return structuredSSHCommand(t, client)(ctx, command)
}
