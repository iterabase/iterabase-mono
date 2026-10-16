package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiagnosticStringDropsSignedURLsBeforeStructuralRedaction(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, input := range []string{
		"GET https://user:pass@cdn01.quay.io/blobs/" + digest + "?token=one&X-Amz-Signature=two#three",
		`{"url":"https:\/\/user:pass@cdn01.quay.io\/blobs\/` + digest + `?X-Amz-Credential=one&Signature=two#three"}`,
		`url=https\u003a\u002f\u002fuser:pass@cdn01.quay.io/blobs/` + digest + `?unknown-key=one&another=two#three`,
		"GET HTTPS://user:pass@cdn01.quay.io/blobs/" + digest + "?unknown-key=one",
	} {
		output := New().DiagnosticString(input)
		if !strings.Contains(output, "cdn01.quay.io") || !strings.Contains(output, digest) {
			t.Fatalf("lost public host/digest from %q: %q", input, output)
		}
		for _, secret := range []string{"user", "pass", "one", "two", "three", "Signature", "Credential", "unknown-key"} {
			if strings.Contains(output, secret) {
				t.Fatalf("secret %q in %q", secret, output)
			}
		}
		if New().DiagnosticString(output) != output {
			t.Fatalf("diagnostic redaction is not idempotent: %q", output)
		}
	}
}

func TestDiagnosticStringRedactsWholeAuthorizationAndCookieHeaders(t *testing.T) {
	t.Parallel()
	input := "Authorization: Basic YWJjZGVm\nCookie: session=alpha; other=bravo\nProxy-Authorization=Bearer charlie\nSet-Cookie: session=delta; secure\npassword=echo\nregistered-value\n"
	output := New("registered-value").DiagnosticString(input)
	for _, secret := range []string{"YWJjZGVm", "alpha", "bravo", "charlie", "delta", "echo", "registered-value"} {
		if strings.Contains(output, secret) {
			t.Fatalf("secret %q retained in %q", secret, output)
		}
	}
}

func TestDiagnosticHeaderClassificationIsBoundedAndFailClosed(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		`{"Authorization":["Basic AUTH_SECRET"],"Cookie":["session=COOKIE_ONE","other=COOKIE_TWO"]}`,
		`msg="{\"Authorization\":[\"Basic AUTH_SECRET\"],\"Cookie\":[\"session=COOKIE_ONE\",\"other=COOKIE_TWO\"]}"`,
		`{"\u0041uthorization":["Basic AUTH_SECRET"]}`,
		"{\n  \"Cookie\": [\n    \"session=COOKIE_ONE\",\n    \"other=COOKIE_TWO\"\n  ]\n}",
	} {
		output := New().DiagnosticString(value)
		if output != "<credential-header diagnostic payload withheld>" || New().DiagnosticString(output) != output {
			t.Fatalf("ambiguous header payload was not withheld/idempotent: %q", output)
		}
	}
	// Do not keep decoding attacker-controlled nested log records indefinitely
	// or assume that an unclassified, still-encoded payload is safe to retain.
	encoded := `{"Authorization":["Basic AUTH_SECRET"]}`
	for range 9 {
		data, err := json.Marshal(encoded)
		if err != nil {
			t.Fatal(err)
		}
		encoded = string(data)
	}
	if output := New().DiagnosticString(encoded); output != "<credential-header diagnostic payload withheld>" {
		t.Fatalf("excessively nested record was not withheld: %q", output)
	}
	benign := `msg="unrelated\\value" status=failed`
	if output := New().DiagnosticString(benign); output != benign {
		t.Fatalf("classification copy rewrote unrelated evidence: %q", output)
	}
}

func TestDiagnosticURLPathAndMalformedURLFailClosed(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"https://cdn01.quay.io/secret-path?signature=signed-value#fragment",
		"https://user:%invalid@cdn01.quay.io/path?signature=signed-value",
	} {
		output := New().DiagnosticString(input)
		for _, secret := range []string{"secret-path", "signature", "signed-value", "user", "%invalid", "fragment"} {
			if strings.Contains(output, secret) {
				t.Fatalf("unsafe malformed/path output: %q", output)
			}
		}
	}
	// Custom executors may have already structurally redacted a URL. Do not
	// stop at the inserted marker/quotes and leave signed material behind it.
	masked := `https://user:<redacted>@cdn01.quay.io/path?unknown-key=one&token="<redacted>"&Signature=two`
	output := New().DiagnosticString(masked)
	for _, secret := range []string{"user", "unknown-key", "one", "Signature", "two"} {
		if strings.Contains(output, secret) {
			t.Fatalf("already-masked URL leaked suffix: %q", output)
		}
	}
	input := "https://example.test/path?public=value"
	if New().String(input) != input {
		t.Fatal("ordinary redaction changed protocol URL values")
	}
}
