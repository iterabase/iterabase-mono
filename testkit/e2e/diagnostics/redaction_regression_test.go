package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
)

func TestKindRuntimeReviewPersistence(t *testing.T) {
	t.Parallel()
	headers := http.Header{
		"Authorization": {"Basic AUTH_SECRET"},
		"Cookie":        {"session=COOKIE_ONE", "other=COOKIE_TWO"},
	}
	raw, err := json.Marshal(headers)
	if err != nil {
		t.Fatal(err)
	}
	escaped, err := json.Marshal(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(headers, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]string{
		"url-query-apostrophe":    "https://cdn01.quay.io/blob?filename=it's-a-blob&X-Amz-Signature=SIGNED_SECRET#FRAGMENT_SECRET",
		"url-password-apostrophe": "https://user:prefix'PASSWORD_SECRET@cdn01.quay.io/blob?X-Amz-Signature=SIGNED_SECRET#FRAGMENT_SECRET",
		"headers-json-array":      string(raw),
		"headers-logfmt-json":     "level=error msg=" + string(escaped),
		"headers-multiline":       string(pretty),
		"headers-unicode-key":     `{"\u0041uthorization":["Basic AUTH_SECRET"],"\u0043ookie":["session=COOKIE_ONE","other=COOKIE_TWO"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := &kindFixture{run: func(_ context.Context, command process.Command) (process.Result, error) {
				if command.Name == "docker" && command.Args[2] == "journalctl" {
					return process.Result{Output: payload, Stdout: payload, Stderr: payload, ExitCode: 7}, errors.New(payload)
				}
				return kindFixtureResult(command), nil
			}}
			directory := t.TempDir()
			err := (KindRuntimeCollector{Executor: fixture, ClusterName: "charts", OutputDir: directory}).Collect(context.Background())
			if err == nil {
				t.Fatal("synthetic collection error was hidden")
			}
			report := readKindReport(t, directory)
			if report.CollectionComplete || len(report.Sources) != 3 || report.Sources[1].State != "unavailable" || report.Sources[2].State != "collected" {
				t.Fatalf("error or later observation lost: %+v", report)
			}
			outputs := []string{err.Error()}
			if err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if err == nil {
					outputs = append(outputs, string(data))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, output := range outputs {
				for _, secret := range []string{"SIGNED_SECRET", "FRAGMENT_SECRET", "PASSWORD_SECRET", "AUTH_SECRET", "COOKIE_ONE", "COOKIE_TWO"} {
					if strings.Contains(output, secret) {
						t.Errorf("synthetic credential %q survived journal/error/index persistence: %s", secret, output)
					}
				}
			}
		})
	}
}
