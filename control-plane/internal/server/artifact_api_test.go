package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/artifacttest"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// rawUpload sends one raw artifact stream with the given headers.
func (h *authAPIHarness) rawUpload(name, key string, body []byte, headers map[string]string) authHTTPResponse {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/artifacts", bytes.NewReader(body))
	request.Header.Set("Origin", authTestOrigin)
	request.Header.Set("Content-Type", "text/plain")
	if name != "" {
		request.Header.Set("X-Artifact-Filename", name)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	response := authHTTPResponse{status: recorder.Code, header: recorder.Header(), body: map[string]any{}}
	_ = json.Unmarshal(recorder.Body.Bytes(), &response.body)
	return response
}

func (h *authAPIHarness) clearArtifacts() {
	h.t.Helper()
	h.scanArtifacts(nil)
}

// scanArtifacts drives every pending artifact through a scripted scanner.
func (h *authAPIHarness) scanArtifacts(decide func(artifactstore.ScanRequest, []byte) artifacttest.Verdict) {
	h.t.Helper()
	scanner := &artifacttest.Scanner{Decide: decide}
	require.NoError(h.t, artifacttest.Drain(context.Background(), artifacttest.NewCoordinator(h.pool, h.objects, scanner)))
}

// TestArtifactRoutesEnforceQuarantineVisibility proves the section 13/14
// contract: a provisional handle, an initiating-human/Admin pre-registration
// predicate, shared visibility only after clearance, cleared-only downloads,
// and browser-only security retry.
//
//nolint:gocyclo // One sequential customer-boundary proof.
func TestArtifactRoutesEnforceQuarantineVisibility(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	alice := h.activeUser("alice@example.com", "operator", "alice-long-password")
	aliceSession := h.signIn("alice@example.com", "alice-long-password", alice)
	bob := h.activeUser("bob@example.com", "operator", "bob-long-password")
	bobSession := h.signIn("bob@example.com", "bob-long-password", bob)
	bearer := func(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }
	bobReader := h.personalKey(bob.ID, identity.ActionArtifactsRead)
	aliceUploadOnly := h.personalKey(alice.ID, identity.ActionArtifactsUpload)

	missing := h.rawUpload("notes.txt", "", []byte("hello"), aliceSession.unsafeHeaders())
	assert.Equal(t, http.StatusBadRequest, missing.status)
	assert.Equal(t, "idempotency_key_required", missing.body["code"])
	noName := h.rawUpload("", "k1", []byte("hello"), aliceSession.unsafeHeaders())
	assert.Equal(t, "filename_required", noName.body["code"])
	noCSRF := h.rawUpload("notes.txt", "k1", []byte("hello"), aliceSession.headers())
	assert.Equal(t, http.StatusForbidden, noCSRF.status)

	uploaded := h.rawUpload("Q3%20notes.txt", "k1", []byte("hello"), aliceSession.unsafeHeaders())
	require.Equal(t, http.StatusAccepted, uploaded.status, "%v", uploaded.body)
	assert.Equal(t, "checking_security", uploaded.body["state"])
	assert.NotContains(t, uploaded.body, "artifact", "the provisional handle is not an artifact ref")
	handle := uploaded.body["uploadId"].(string)
	status := "/v1/artifacts/" + handle + "/status"

	own := h.do(http.MethodGet, status, nil, aliceSession.headers())
	require.Equal(t, http.StatusOK, own.status)
	assert.Equal(t, "checking_security", own.body["state"])
	assert.Equal(t, "Q3 notes.txt", own.body["displayName"])
	assert.Equal(t, http.StatusOK, h.do(http.MethodGet, status, nil, admin.headers()).status, "an Admin browser sees security status")
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, status, nil, bobSession.headers()).status, "another Operator learns nothing")
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, status, nil, bearer(bobReader)).status)
	assert.Equal(t, http.StatusForbidden, h.do(http.MethodGet, status, nil, bearer(aliceUploadOnly)).status, "upload-only keys cannot poll")
	list := h.do(http.MethodGet, "/v1/artifacts", nil, bobSession.headers())
	require.Equal(t, http.StatusOK, list.status)
	assert.Empty(t, list.body["items"], "quarantined uploads are absent from the shared library")

	h.clearArtifacts()
	cleared := h.do(http.MethodGet, status, nil, bobSession.headers())
	require.Equal(t, http.StatusOK, cleared.status, "after clearance the artifact is shared")
	ref := cleared.body["artifact"].(map[string]any)
	artifactID := ref["artifactId"].(string)
	assert.Equal(t, "text/plain", ref["mimeType"])
	list = h.do(http.MethodGet, "/v1/artifacts?search=notes", nil, bearer(bobReader))
	require.Equal(t, http.StatusOK, list.status)
	require.Len(t, list.body["items"], 1)

	request := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+artifactID, nil)
	request.Header.Set("Authorization", "Bearer "+bobReader)
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	body, _ := io.ReadAll(recorder.Body)
	assert.Equal(t, "hello", string(body))
	assert.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, recorder.Header().Get("Content-Disposition"), "attachment")

	replay := h.rawUpload("Q3%20notes.txt", "k1", []byte("hello"), aliceSession.unsafeHeaders())
	assert.Equal(t, handle, replay.body["uploadId"], "an idempotent replay returns the same handle")
	conflict := h.rawUpload("Q3%20notes.txt", "k1", []byte("changed"), aliceSession.unsafeHeaders())
	assert.Equal(t, "idempotency_conflict", conflict.body["code"])

	retry := "/v1/artifacts/" + artifactID + "/security-retries"
	assert.Equal(t, "bearer_not_accepted", h.do(http.MethodPost, retry, nil, bearer(bobReader)).body["code"])
	assert.Equal(t, "not_retryable", h.do(http.MethodPost, retry, nil, aliceSession.unsafeHeaders()).body["code"])

	deleter := h.personalKey(admin.user.ID, identity.ActionArtifactsRead, identity.ActionArtifactsDelete)
	deleted := h.do(http.MethodDelete, "/v1/artifacts/"+artifactID, nil, bearer(deleter))
	require.Equal(t, http.StatusAccepted, deleted.status, "%v", deleted.body)
	assert.Equal(t, "unavailable", deleted.body["state"])
	assert.Equal(t, http.StatusNotFound, h.do(http.MethodGet, "/v1/artifacts/"+artifactID, nil, bearer(bobReader)).status,
		"deleting bytes are never served")
}

// TestArtifactContainerStatusAppliesChildVisibility proves DES-HOR-460-10
// holds through a shared container: polling a registered parent reveals only
// the children the caller could poll directly.
func TestArtifactContainerStatusAppliesChildVisibility(t *testing.T) {
	h := newAuthAPIHarness(t)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	alice := h.activeUser("alice@example.com", "operator", "alice-long-password")
	aliceSession := h.signIn("alice@example.com", "alice-long-password", alice)
	bob := h.activeUser("bob@example.com", "operator", "bob-long-password")
	bobSession := h.signIn("bob@example.com", "bob-long-password", bob)
	bobReader := h.personalKey(bob.ID, identity.ActionArtifactsRead)

	uploaded := h.rawUpload("mail.eml", "k-mail", []byte("container"), aliceSession.unsafeHeaders())
	require.Equal(t, http.StatusAccepted, uploaded.status, "%v", uploaded.body)
	h.scanArtifacts(func(_ artifactstore.ScanRequest, data []byte) artifacttest.Verdict {
		switch string(data) {
		case "container":
			return artifacttest.Verdict{CanonicalMIME: "message/rfc822", Children: []artifacttest.Child{
				{Kind: artifactstore.ChildBody, Filename: "body.txt", ClaimedMIME: "text/plain", Data: []byte("body")},
				{Kind: artifactstore.ChildAttachment, Filename: "good.txt", ClaimedMIME: "text/plain", Data: []byte("good")},
				{Kind: artifactstore.ChildAttachment, Filename: "bad.txt", ClaimedMIME: "text/plain", Data: []byte("bad")},
			}}
		case "bad":
			return artifacttest.Verdict{Malware: true}
		}
		return artifacttest.Verdict{}
	})
	status := "/v1/artifacts/" + uploaded.body["uploadId"].(string) + "/status"
	children := func(headers map[string]string) []string {
		t.Helper()
		response := h.do(http.MethodGet, status, nil, headers)
		require.Equal(t, http.StatusOK, response.status, "%v", response.body)
		var names []string
		raw, _ := response.body["children"].([]any)
		for _, child := range raw {
			names = append(names, child.(map[string]any)["displayName"].(string))
		}
		return names
	}
	assert.Equal(t, []string{"body.txt", "good.txt"}, children(bobSession.headers()),
		"another Operator sees only registered children of a shared container")
	assert.Equal(t, []string{"body.txt", "good.txt"}, children(map[string]string{"Authorization": "Bearer " + bobReader}))
	assert.Equal(t, []string{"body.txt", "good.txt", "bad.txt"}, children(aliceSession.headers()),
		"the uploader sees every child, including the rejected one")
	assert.Equal(t, []string{"body.txt", "good.txt", "bad.txt"}, children(admin.headers()),
		"a browser Admin holds the security-status path")
}
