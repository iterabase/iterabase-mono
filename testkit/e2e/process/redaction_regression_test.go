package process

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are intentionally unregistered synthetic credentials: the diagnostic
// structural policy itself must protect them, before process evidence is saved.
func TestDiagnosticReviewPersistence(t *testing.T) {
	t.Parallel()
	headers := http.Header{
		"Authorization":       {"Basic AUTH_SECRET"},
		"Proxy-Authorization": {"Basic PROXY_SECRET"},
		"Cookie":              {"session=COOKIE_ONE", "other=COOKIE_TWO"},
		"Set-Cookie":          {"session=SET_COOKIE_ONE", "other=SET_COOKIE_TWO"},
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
	cases := map[string]string{
		"url-query-apostrophe":    "https://cdn01.quay.io/blob?filename=it's-a-blob&X-Amz-Signature=SIGNED_SECRET#FRAGMENT_SECRET",
		"url-password-apostrophe": "https://user:prefix'PASSWORD_SECRET@cdn01.quay.io/blob?X-Amz-Signature=SIGNED_SECRET#FRAGMENT_SECRET",
		"headers-json-array":      string(raw),
		"headers-logfmt-json":     "level=error msg=" + string(escaped),
		"headers-multiline":       string(pretty),
		"headers-unicode-key":     `{"\u0041uthorization":["Basic AUTH_SECRET"],"\u0043ookie":["session=COOKIE_ONE","other=COOKIE_TWO"]}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			result, err := (Runner{OutputDir: directory}).Run(context.Background(), Command{
				Name: "sh", Args: []string{"-c", `printf '%s' "$DIAGNOSTIC_FIXTURE"; printf '%s' "$DIAGNOSTIC_FIXTURE" >&2; exit 7`},
				Env: map[string]string{"DIAGNOSTIC_FIXTURE": payload}, Timeout: 2 * time.Second,
				MaxOutputBytes: 4096, RedactURLs: true, OutputName: "diagnostic.log",
			})
			if err == nil || result.ExitCode != 7 || result.OutputTruncated {
				t.Fatalf("unexpected process outcome: %+v err=%v", result, err)
			}
			data, err := os.ReadFile(filepath.Join(directory, "diagnostic.log"))
			if err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{result.Output, result.Stdout, result.Stderr, string(data)} {
				for _, secret := range []string{"SIGNED_SECRET", "FRAGMENT_SECRET", "PASSWORD_SECRET", "AUTH_SECRET", "PROXY_SECRET", "COOKIE_ONE", "COOKIE_TWO", "SET_COOKIE_ONE", "SET_COOKIE_TWO"} {
					if strings.Contains(output, secret) {
						t.Errorf("synthetic credential %q survived diagnostic persistence: %s", secret, output)
					}
				}
				if strings.HasPrefix(name, "url-") && !strings.Contains(output, "cdn01.quay.io") {
					t.Errorf("URL sanitization lost the safe endpoint origin: %q", output)
				}
			}
		})
	}
}
