package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type artifactMetadata struct {
	ID                  string  `json:"artifactId"`
	CreatedByIdentityID string  `json:"createdByIdentityId"`
	MIMEType            string  `json:"mimeType"`
	SizeBytes           *int64  `json:"sizeBytes,omitempty"`
	Digest              *string `json:"digest,omitempty"`
	State               string  `json:"state"`
	AvailableAt         *string `json:"availableAt,omitempty"`
	DeletionReason      *string `json:"deletionReason,omitempty"`
}

var artifactBytes = []byte("case_id,result\n478,accepted\n")

func setupArtifactJourneyStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.ensurePrincipals(t)
	state.applyWorkFixture(t)
}

func uploadPublishAndLinkArtifactStage(t *testing.T, state *deployedState) {
	t.Helper()
	digestBytes := sha256.Sum256(artifactBytes)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	headers := map[string]string{
		"Content-Type":      "text/csv",
		"X-Artifact-SHA256": digest,
		"X-Artifact-Source": "hor-478-synthetic",
	}
	// The Admin reporting/deletion credential does not hold artifacts.upload.
	status, body := state.request(t, http.MethodPost, "/v1/artifacts", state.adminKey, artifactBytes, headers)
	requireStatus(t, status, http.StatusForbidden, body)
	status, body = state.request(t, http.MethodPost, "/v1/artifacts", state.workKey, artifactBytes, headers)
	requireStatus(t, status, http.StatusCreated, body)
	assertCustomerSafeJSON(t, body)
	var artifact artifactMetadata
	mustDecode(t, body, &artifact)
	if artifact.ID == "" || artifact.State != "available" || artifact.AvailableAt == nil ||
		artifact.Digest == nil || *artifact.Digest != digest || artifact.SizeBytes == nil || *artifact.SizeBytes != int64(len(artifactBytes)) ||
		artifact.CreatedByIdentityID != state.workIdentityID {
		t.Fatalf("artifact was not atomically published with attributable immutable metadata: %s", safeResponse(body))
	}
	state.artifactID = artifact.ID

	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	status, body = state.request(t, http.MethodPost, "/v1/artifacts", state.workKey, []byte("rejected"), map[string]string{
		"Content-Type": "text/plain", "X-Artifact-SHA256": wrongDigest,
	})
	requireStatus(t, status, http.StatusBadRequest, body)

	start := workStartBody("Case 478 — linked artifact")
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
	}
	mustDecode(t, body, &links)
	if len(links) != 1 || links[0].ArtifactID != state.artifactID || links[0].Role != "source" || links[0].Digest == nil || *links[0].Digest != digest {
		t.Fatalf("work artifact link mismatch: %s", safeResponse(body))
	}

	assertArtifactDownload(t, state, digest)
	status, body = state.request(t, http.MethodHead, "/v1/artifacts/"+state.artifactID, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
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
	status, body := state.request(t, http.MethodDelete, "/v1/artifacts/"+state.artifactID, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusForbidden, body)
	status, body = state.request(t, http.MethodDelete, "/v1/artifacts/"+state.artifactID, state.adminKey, nil, nil)
	requireStatus(t, status, http.StatusNoContent, body)
	status, body = state.request(t, http.MethodGet, "/v1/artifacts/"+state.artifactID, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusGone, body)

	row := state.databaseQuery(t, "SELECT state || ':' || COALESCE(deletion_reason,'') FROM artifact.artifacts WHERE id='"+state.artifactID+"'")
	if row != "deleted:admin_explicit" {
		t.Fatalf("artifact tombstone=%q want deleted:admin_explicit", row)
	}
	state.restartAPI(t)
	status, body = state.request(t, http.MethodGet, "/v1/artifacts/"+state.artifactID, state.workKey, nil, nil)
	requireStatus(t, status, http.StatusGone, body)
	row = state.databaseQuery(t, "SELECT state || ':' || COALESCE(deletion_reason,'') FROM artifact.artifacts WHERE id='"+state.artifactID+"'")
	if row != "deleted:admin_explicit" {
		t.Fatalf("artifact tombstone after restart=%q", row)
	}
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
