package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
)

// Portable identity, catalogue, authentication, and inference correctness is
// authoritative in control-plane/deployed-execution-contracts. These helpers
// retain only the minimal product seam required for Forge's real-GPU serving
// smoke after substrate reconciliation.
type catalogEntry struct {
	ModelID    string `json:"model_id"`
	Available  bool   `json:"available"`
	BackendURL string `json:"backend_url"`
}

var keyRe = regexp.MustCompile(`API key \(([^)]+)\): (\S+)`)

func mustFindKey(t *testing.T, logs, scope string) string {
	t.Helper()
	for _, match := range keyRe.FindAllStringSubmatch(logs, -1) {
		if match[1] == scope {
			return match[2]
		}
	}
	redacted := keyRe.ReplaceAllString(logs, "API key ($1): <redacted>")
	t.Fatalf("bootstrap key %q not found in logs:\n%s", scope, redacted)
	return ""
}

func keyPrefix(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

func createAPIKey(t *testing.T, client *http.Client, url, adminKey, identityID, scope string) string {
	t.Helper()
	body := fmt.Sprintf(`{"identityID":%q,"name":"e2e-%s","scope":%q}`, identityID, scope, scope)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create api key: status %d\n%s", resp.StatusCode, respBody)
	}
	var result struct {
		FullKey string `json:"fullKey"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil || result.FullKey == "" {
		t.Fatalf("api-key response missing fullKey: %s", respBody)
	}
	return result.FullKey
}

func getSecretKey(t *testing.T, cluster *remotecluster.Cluster, namespace, name, key string) string {
	t.Helper()
	encoded := strings.TrimSpace(cluster.Kubectl(t, "get", "secret", name, "-n", namespace,
		"-o", fmt.Sprintf("jsonpath={.data.%s}", key)))
	if encoded == "" {
		t.Fatalf("secret %s/%s has no key %q", namespace, name, key)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode secret %s/%s[%s]: %v", namespace, name, key, err)
	}
	return string(decoded)
}

func snapshotCatalog(client *http.Client, baseURL, adminKey string) ([]catalogEntry, int, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/admin/v1/snapshot", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Admin-Key", adminKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var snapshot struct {
		Catalog []catalogEntry `json:"catalog"`
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return nil, resp.StatusCode, err
	}
	return snapshot.Catalog, resp.StatusCode, nil
}

func chatCompletionsStatus(t *testing.T, client *http.Client, baseURL, key, model string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post chat completions: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}
