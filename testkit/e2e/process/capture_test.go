package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

func TestCaptureLimitWithholdsBothStreamsAndPartialSecrets(t *testing.T) {
	t.Parallel()
	for _, script := range []string{
		`printf '12345678'; printf 'boundary-secret' >&2; exit 7`,
		`printf 'boundary-secret'; exit 7`,
	} {
		directory := t.TempDir()
		result, err := (Runner{Redactor: redact.New("boundary-secret"), OutputDir: directory}).Run(context.Background(), Command{
			Name: "sh", Args: []string{"-c", script}, Timeout: time.Second,
			MaxOutputBytes: 8, OutputName: "capture.log",
		})
		if !errors.Is(err, ErrOutputLimit) || !result.OutputTruncated || result.ExitCode != 7 {
			t.Fatalf("overflow result=%+v err=%v", result, err)
		}
		if result.Output != "" || result.Stdout != "" || result.Stderr != "" {
			t.Fatalf("overflow leaked a payload prefix: %+v", result)
		}
		data, readErr := os.ReadFile(filepath.Join(directory, "capture.log"))
		if readErr != nil || len(data) != 0 {
			t.Fatalf("persisted overflow payload=%q err=%v", data, readErr)
		}
	}
}

func TestCaptureLimitCountsCombinedChannelsAndAcceptsExactBound(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{7, 8} {
		result, err := (Runner{}).Run(context.Background(), Command{
			Name: "sh", Args: []string{"-c", `printf '1234'; printf '5678' >&2`},
			Timeout: time.Second, MaxOutputBytes: limit,
		})
		if limit == 7 {
			if !errors.Is(err, ErrOutputLimit) || !result.OutputTruncated {
				t.Fatalf("combined overflow result=%+v err=%v", result, err)
			}
		} else if err != nil || result.OutputTruncated || result.Stdout != "1234" || result.Stderr != "5678" {
			t.Fatalf("exact-bound result=%+v err=%v", result, err)
		}
	}
}

func TestCaptureBoundsRedactionExpansionAndCancellation(t *testing.T) {
	t.Parallel()
	result, err := (Runner{Redactor: redact.New("x")}).Run(context.Background(), Command{
		Name: "sh", Args: []string{"-c", `printf 'x'`}, Timeout: time.Second, MaxOutputBytes: 2,
	})
	if !errors.Is(err, ErrOutputLimit) || !result.OutputTruncated || result.Output != "" {
		t.Fatalf("expanded result=%+v err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (Runner{}).Run(ctx, Command{Name: "sh", Args: []string{"-c", "exit 0"}, Timeout: time.Second, MaxOutputBytes: 8})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestDiagnosticURLPolicyPrecedesRedactionAndPersistence(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	input := "https://user:password@cdn01.quay.io/blob?token=first&X-Amz-Signature=second#fragment"
	result, err := (Runner{OutputDir: directory}).Run(context.Background(), Command{
		Name: "sh", Args: []string{"-c", `printf '%s' "$PROBE_URL"; printf '%s' "$PROBE_URL" >&2`},
		Env: map[string]string{"PROBE_URL": input}, Timeout: time.Second,
		MaxOutputBytes: 1024, RedactURLs: true, OutputName: "safe.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "safe.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{result.Output, result.Stdout, result.Stderr, string(data)} {
		if !strings.Contains(value, "cdn01.quay.io") {
			t.Fatalf("lost safe origin: %q", value)
		}
		for _, secret := range []string{"user", "password", "first", "second", "fragment", "X-Amz"} {
			if strings.Contains(value, secret) {
				t.Fatalf("unsafe diagnostic capture %q", value)
			}
		}
	}
	// Opt-in only: ordinary protocol stdout retains its URL/query byte values.
	plain := "https://example.test/api?public=value"
	defaultResult, err := (Runner{}).Run(context.Background(), Command{
		Name: "sh", Args: []string{"-c", `printf '%s' "$PROBE_URL"`},
		Env: map[string]string{"PROBE_URL": plain}, Timeout: time.Second,
	})
	if err != nil || defaultResult.Stdout != plain || defaultResult.OutputTruncated {
		t.Fatalf("default capture changed: %+v err=%v", defaultResult, err)
	}
}

func TestCaptureRejectsNegativeLimit(t *testing.T) {
	t.Parallel()
	if _, err := (Runner{}).Run(context.Background(), Command{Name: "true", Timeout: time.Second, MaxOutputBytes: -1}); err == nil {
		t.Fatal("negative capture limit accepted")
	}
}
