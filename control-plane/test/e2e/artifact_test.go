package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// artifactStatus is the customer status projection (HOR-461).
type artifactStatus struct {
	UploadID string `json:"uploadId"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
	Artifact *struct {
		ArtifactID string `json:"artifactId"`
		MIMEType   string `json:"mimeType"`
		SizeBytes  int64  `json:"sizeBytes"`
		Digest     string `json:"digest"`
	} `json:"artifact"`
}

var artifactBytes = []byte("case_id,result\n478,accepted\n")

// e2eArtifactValues enables the deployed quarantine pipeline offline: the
// FreshClam updater publishes only a test database (no internet egress or
// official CDN in CI) that detects the EICAR string, which is also the
// scanner adapter's readiness self-test.
func e2eArtifactValues() map[string]any {
	return map[string]any{
		"enabled": true,
		"processing": map[string]any{
			"enabled": true,
			// The test database is tiny; the 1.5 GiB production request is
			// sized for the official signatures and would crowd a Kind node.
			"scanner": map[string]any{"clamdResources": map[string]any{
				"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
				"limits":   map[string]any{"memory": "1Gi"},
			}},
			"signatures": map[string]any{
				"official": false,
				"customDatabases": map[string]any{
					"iterabase-e2e.hdb": "44d88612fea8a8f36de82e1278abb02f:68:Eicar-Test-Signature\n",
				},
			},
		},
	}
}

func setupArtifactJourneyStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.ensurePrincipals(t)
	state.applyWorkFixture(t)
}

// uploadArtifact streams one file into quarantine and returns its handle.
func (state *deployedState) uploadArtifact(t *testing.T, key, name, mimeType, idempotencyKey string, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	status, body := state.request(t, http.MethodPost, "/v1/artifacts", key, content, map[string]string{
		"Content-Type": mimeType, "X-Artifact-SHA256": "sha256:" + hex.EncodeToString(sum[:]),
		"X-Artifact-Filename": name, "Idempotency-Key": idempotencyKey,
	})
	requireStatus(t, status, http.StatusAccepted, body)
	assertCustomerSafeJSON(t, body)
	var handle artifactStatus
	mustDecode(t, body, &handle)
	if handle.UploadID == "" || handle.State != "checking_security" || handle.Artifact != nil {
		t.Fatalf("upload must return a provisional handle, not a usable ref: %s", safeResponse(body))
	}
	return handle.UploadID
}

// waitArtifactState polls the status route until the predicate holds.
func (state *deployedState) waitArtifactState(t *testing.T, key, uploadID string, done func(artifactStatus) bool) artifactStatus {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		status, body := state.request(t, http.MethodGet, "/v1/artifacts/"+uploadID+"/status", key, nil, nil)
		requireStatus(t, status, http.StatusOK, body)
		var st artifactStatus
		mustDecode(t, body, &st)
		if done(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("artifact %s did not reach the expected state: %s", uploadID, safeResponse(body))
		}
		time.Sleep(2 * time.Second)
	}
}

// uploadClearedArtifact admits a file and waits for the deployed scanner to
// clear it, returning the canonical artifact id.
func (state *deployedState) uploadClearedArtifact(t *testing.T, key, name, mimeType, idempotencyKey string, content []byte) string {
	t.Helper()
	uploadID := state.uploadArtifact(t, key, name, mimeType, idempotencyKey, content)
	cleared := state.waitArtifactState(t, key, uploadID, func(s artifactStatus) bool {
		if s.State == "rejected" || s.State == "security_check_failed_retryable" {
			t.Fatalf("safe artifact was not cleared: state=%s reason=%s", s.State, s.Reason)
		}
		return s.Artifact != nil
	})
	return cleared.Artifact.ArtifactID
}

// eicar builds the standard antivirus test string at runtime so no source
// file contains a contiguous detectable sample.
func eicar() []byte {
	return []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$` + `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)
}

func uploadPublishAndLinkArtifactStage(t *testing.T, state *deployedState) {
	t.Helper()
	digestBytes := sha256.Sum256(artifactBytes)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	// The Admin reporting/deletion credential does not hold artifacts.upload.
	status, body := state.request(t, http.MethodPost, "/v1/artifacts", state.adminKey, artifactBytes, map[string]string{
		"Content-Type": "text/csv", "X-Artifact-Filename": "accepted.csv", "Idempotency-Key": "hor-461-admin",
	})
	requireStatus(t, status, http.StatusForbidden, body)

	// SCN-005: unusable until cleared by the deployed ClamAV scanner and
	// admission policy, then shared and linkable.
	uploadID := state.uploadArtifact(t, state.workKey, "accepted.csv", "text/csv", "hor-461-artifact-1", artifactBytes)
	start := workStartBody("Case 478 — linked artifact")
	start["artifactRefs"] = []any{map[string]any{"artifactId": uploadID, "role": "source"}}
	status, body = state.requestJSONWithHeaders(t, http.MethodPost, "/v1/work-items", state.workKey, start,
		map[string]string{"Idempotency-Key": "hor-461-artifact-link-early"})
	if status == http.StatusCreated {
		t.Fatalf("a provisional handle must never link to work: %s", safeResponse(body))
	}
	cleared := state.waitArtifactState(t, state.workKey, uploadID, func(s artifactStatus) bool { return s.Artifact != nil })
	if cleared.Artifact.Digest != digest || cleared.Artifact.SizeBytes != int64(len(artifactBytes)) || cleared.Artifact.MIMEType != "text/csv" {
		t.Fatalf("cleared ref mismatch: %+v", cleared.Artifact)
	}
	state.artifactID = cleared.Artifact.ArtifactID

	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	status, body = state.request(t, http.MethodPost, "/v1/artifacts", state.workKey, []byte("rejected"), map[string]string{
		"Content-Type": "text/plain", "X-Artifact-SHA256": wrongDigest, "X-Artifact-Filename": "x.txt", "Idempotency-Key": "hor-461-bad-digest",
	})
	requireStatus(t, status, http.StatusBadRequest, body)

	start["artifactRefs"] = []any{map[string]any{
		"artifactId": state.artifactID, "role": "source", "metadata": map[string]any{"name": "accepted.csv", "label": "Accepted result"},
	}}
	status, body = state.requestJSONWithHeaders(t, http.MethodPost, "/v1/work-items", state.workKey, start,
		map[string]string{"Idempotency-Key": "hor-478-artifact-link-1"})
	requireStatus(t, status, http.StatusCreated, body)
	var item workItemResponse
	mustDecode(t, body, &item)
	state.workItemID = item.ID

	status, body = state.request(t, http.MethodGet, "/v1/work-items/"+state.workItemID+"/artifacts", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	assertCustomerSafeJSON(t, body, privateSourceMarker)
	var links []struct {
		ArtifactID string          `json:"artifactId"`
		Role       string          `json:"role"`
		Metadata   json.RawMessage `json:"metadata"`
		Digest     *string         `json:"digest"`
		State      string          `json:"state"`
	}
	mustDecode(t, body, &links)
	if len(links) != 1 || links[0].ArtifactID != state.artifactID || links[0].Role != "source" || links[0].Digest == nil || *links[0].Digest != digest {
		t.Fatalf("work artifact link mismatch: %s", safeResponse(body))
	}

	assertArtifactDownload(t, state, digest)
	status, body = state.request(t, http.MethodHead, "/v1/artifacts/"+state.artifactID, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	status, body = state.request(t, http.MethodGet, "/v1/artifacts?search=accepted", state.adminKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	if !strings.Contains(string(body), state.artifactID) {
		t.Fatalf("cleared artifact is not in the shared library for another user: %s", safeResponse(body))
	}

	// SCN-006: malware is permanently rejected, never downloadable, never
	// listed, and its bytes are purged.
	malware := state.uploadArtifact(t, state.workKey, "eicar.txt", "text/plain", "hor-461-malware", eicar())
	rejected := state.waitArtifactState(t, state.workKey, malware, func(s artifactStatus) bool { return s.State == "rejected" })
	if rejected.Reason != "malware" || rejected.Artifact != nil {
		t.Fatalf("malware must be rejected without a usable ref: %+v", rejected)
	}
	row := state.databaseQuery(t, "SELECT storage_state || ':' || (SELECT count(*) FROM artifact.objects o WHERE o.artifact_id=a.id AND o.state<>'purged') FROM artifact.artifacts a WHERE upload_id='"+malware+"'")
	if row != "deleted:0" && row != "deleting:1" {
		t.Fatalf("malware bytes must be purging or purged, got %q", row)
	}

	// An invalid type is a permanent policy rejection, not a malware verdict:
	// a GIF renamed .pdf (executable bytes would rightly be flagged by ClamAV's
	// broken-executable heuristic first).
	renamed := state.uploadArtifact(t, state.workKey, "invoice.pdf", "application/pdf", "hor-461-renamed",
		[]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
	policy := state.waitArtifactState(t, state.workKey, renamed, func(s artifactStatus) bool { return s.State == "rejected" })
	if policy.Reason == "malware" || policy.Reason == "" {
		t.Fatalf("type policy rejection reason=%q", policy.Reason)
	}

	// Oversized declared bodies are refused before any byte is accepted.
	status, body = state.request(t, http.MethodPost, "/v1/artifacts", state.workKey, make([]byte, 26_214_401), map[string]string{
		"Content-Type": "text/plain", "X-Artifact-Filename": "big.txt", "Idempotency-Key": "hor-461-big",
	})
	requireStatus(t, status, http.StatusRequestEntityTooLarge, body)
}

func restartArtifactProcessesStage(t *testing.T, state *deployedState) {
	t.Helper()
	digestBytes := sha256.Sum256(artifactBytes)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	state.restartMinIO(t)
	assertArtifactDownload(t, state, digest)
	state.restartAPI(t)
	assertArtifactDownload(t, state, digest)

	status, body := state.request(t, http.MethodGet, "/v1/work-items/"+state.workItemID+"/artifacts", state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	assertCustomerSafeJSON(t, body, privateSourceMarker)
}

func deleteAndAssertTombstoneStage(t *testing.T, state *deployedState) {
	t.Helper()
	target := state.uploadClearedArtifact(t, state.workKey, "delete-me.csv", "text/csv", "hor-461-delete", []byte("a,b\n1,2\n"))
	status, body := state.request(t, http.MethodDelete, "/v1/artifacts/"+target, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusForbidden, body)
	status, body = state.request(t, http.MethodDelete, "/v1/artifacts/"+target, state.adminKey, nil, nil)
	requireStatus(t, status, http.StatusAccepted, body)
	status, body = state.request(t, http.MethodGet, "/v1/artifacts/"+target, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusNotFound, body)

	query := "SELECT storage_state || ':' || COALESCE(deletion_reason,'') || ':' || (display_name IS NULL)::text FROM artifact.artifacts WHERE id='" + target + "'"
	deadline := time.Now().Add(2 * time.Minute)
	for state.databaseQuery(t, query) != "deleted:admin_explicit:true" {
		if time.Now().After(deadline) {
			t.Fatalf("artifact tombstone=%q want deleted:admin_explicit:true", state.databaseQuery(t, query))
		}
		time.Sleep(2 * time.Second)
	}
	state.restartAPI(t)
	status, body = state.request(t, http.MethodGet, "/v1/artifacts/"+target, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusNotFound, body)
}

func assertArtifactDownload(t *testing.T, state *deployedState, digest string) {
	t.Helper()
	status, body := state.request(t, http.MethodGet, "/v1/artifacts/"+state.artifactID, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	if string(body) != string(artifactBytes) {
		t.Fatalf("artifact bytes changed: got %q", body)
	}
	sum := sha256.Sum256(body)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		t.Fatalf("download digest changed")
	}
}
