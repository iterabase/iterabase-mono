package graph

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
)

const testToken = "graph-access-token"

type staticTokens struct{ err error }

func (s staticTokens) Token(context.Context, string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return testToken, nil
}

type request struct {
	Method, Path, Query, Prefer, Auth string
	Body                              map[string]any
}

// fakeGraph records requests and answers them with handler.
type fakeGraph struct {
	mu       sync.Mutex
	requests []request
	srv      *httptest.Server
}

func newFakeGraph(t *testing.T, handler func(w http.ResponseWriter, r request)) (*fakeGraph, *Client) {
	t.Helper()
	f := &fakeGraph{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Prefer: r.Header.Get("Prefer"), Auth: r.Header.Get("Authorization")}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			require.NoError(t, json.Unmarshal(raw, &req.Body))
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		handler(w, req)
	}))
	t.Cleanup(f.srv.Close)
	c := New(staticTokens{})
	c.BaseURL = f.srv.URL + "/v1.0"
	return f, c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var testMailbox = mailbox.Mailbox{
	ID: "src-1", Name: "quotes", Provider: mailbox.ProviderMicrosoft365, Address: "quotes@customer.pt",
	Folders: []string{"inbox"}, CredentialSlot: "m365", NotificationURL: "https://hooks.example.com/graph",
}

func TestChangesStartsAFilteredDeltaAndFollowsPages(t *testing.T) {
	since := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	var base string
	f, c := newFakeGraph(t, func(w http.ResponseWriter, r request) {
		if strings.Contains(r.Query, "page=2") {
			writeJSON(w, 200, map[string]any{
				"value":            []any{map[string]any{"id": "m3", "receivedDateTime": "2026-10-14T10:02:00Z"}},
				"@odata.deltaLink": base + "/users/quotes@customer.pt/mailFolders/inbox/messages/delta?$deltatoken=t1",
			})
			return
		}
		writeJSON(w, 200, map[string]any{
			"value": []any{
				map[string]any{"id": "m1", "internetMessageId": "<m1@x>", "conversationId": "c1", "subject": "RFQ",
					"from":             map[string]any{"emailAddress": map[string]any{"name": "Ana", "address": "Ana@Acme.com"}},
					"receivedDateTime": "2026-10-14T10:00:00Z", "hasAttachments": true},
				map[string]any{"id": "draft", "isDraft": true, "receivedDateTime": "2026-10-14T10:00:00Z"},
				map[string]any{"id": "gone", "@removed": map[string]any{"reason": "deleted"}},
			},
			"@odata.nextLink": base + "/users/quotes@customer.pt/mailFolders/inbox/messages/delta?page=2",
		})
	})
	base = c.BaseURL

	first, err := c.Changes(context.Background(), testMailbox, "inbox", "", since)
	require.NoError(t, err)
	assert.True(t, first.More)
	require.Len(t, first.Messages, 1, "drafts and removals are not events")
	assert.Equal(t, mailbox.Message{
		ProviderMessageID: "m1", Folder: "inbox", InternetMessageID: "<m1@x>", ConversationID: "c1", Subject: "RFQ",
		SenderAddress: "Ana@Acme.com", SenderName: "Ana", HasAttachments: true,
		ReceivedAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC),
	}, first.Messages[0])

	second, err := c.Changes(context.Background(), testMailbox, "inbox", first.Cursor, since)
	require.NoError(t, err)
	assert.False(t, second.More)
	assert.Contains(t, second.Cursor, "$deltatoken=t1")
	require.Len(t, second.Messages, 1)

	start := f.requests[0]
	assert.Equal(t, "/v1.0/users/quotes@customer.pt/mailFolders/inbox/messages/delta", start.Path)
	assert.Contains(t, start.Query, "%24filter=receivedDateTime+ge+2026-10-14T09%3A00%3A00Z")
	assert.Contains(t, start.Query, "%24select=")
	assert.Contains(t, start.Prefer, `IdType="ImmutableId"`)
	assert.Equal(t, "Bearer "+testToken, start.Auth)
}

func TestChangesRefusesACursorOutsideGraph(t *testing.T) {
	f, c := newFakeGraph(t, func(w http.ResponseWriter, _ request) { writeJSON(w, 200, map[string]any{}) })
	_, err := c.Changes(context.Background(), testMailbox, "inbox", "https://attacker.example/steal", time.Now())
	assert.ErrorIs(t, err, mailbox.ErrCursorExpired)
	assert.Empty(t, f.requests, "the bearer token must never be sent to a foreign cursor")
}

func TestErrorClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   map[string]any
		header string
		check  func(t *testing.T, err error)
	}{
		"expired delta token": {status: 410, check: func(t *testing.T, err error) { assert.ErrorIs(t, err, mailbox.ErrCursorExpired) }},
		"sync state code": {status: 400, body: map[string]any{"error": map[string]any{"code": "SyncStateNotFound"}},
			check: func(t *testing.T, err error) { assert.ErrorIs(t, err, mailbox.ErrCursorExpired) }},
		"throttled": {status: 429, header: "7", check: func(t *testing.T, err error) {
			var retry *mailbox.RetryAfterError
			require.ErrorAs(t, err, &retry)
			assert.Equal(t, 7*time.Second, retry.After)
		}},
		"outside the application scope": {status: 403, body: map[string]any{"error": map[string]any{"code": "ErrorAccessDenied", "message": "leaky " + testToken}},
			check: func(t *testing.T, err error) {
				var cred *mailbox.CredentialError
				require.ErrorAs(t, err, &cred)
				assert.Equal(t, "Microsoft 365 denied access to quotes@customer.pt", cred.Reason)
			}},
		"rejected token": {status: 401, check: func(t *testing.T, err error) {
			var cred *mailbox.CredentialError
			require.ErrorAs(t, err, &cred)
		}},
		"other error keeps only the code": {status: 400, body: map[string]any{"error": map[string]any{"code": "BadRequest", "message": "echo " + testToken}},
			check: func(t *testing.T, err error) { assert.Equal(t, "graph GET delta: HTTP 400 (BadRequest)", err.Error()) }},
	} {
		t.Run(name, func(t *testing.T) {
			_, c := newFakeGraph(t, func(w http.ResponseWriter, _ request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				writeJSON(w, tc.status, tc.body)
			})
			_, err := c.Changes(context.Background(), testMailbox, "inbox", "", time.Now())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testToken)
			tc.check(t, err)
		})
	}
}

func TestTokenFailureIsACredentialError(t *testing.T) {
	_, c := newFakeGraph(t, func(w http.ResponseWriter, _ request) { writeJSON(w, 200, map[string]any{}) })
	c.Tokens = staticTokens{err: &mailbox.CredentialError{Reason: "Microsoft 365 rejected the credentials"}}
	_, err := c.Changes(context.Background(), testMailbox, "inbox", "", time.Now())
	var cred *mailbox.CredentialError
	require.ErrorAs(t, err, &cred)
}

func TestReplyHeaders(t *testing.T) {
	f, c := newFakeGraph(t, func(w http.ResponseWriter, _ request) {
		writeJSON(w, 200, map[string]any{"internetMessageHeaders": []any{
			map[string]any{"name": "In-Reply-To", "value": " <orig@customer.pt> "},
			map[string]any{"name": "References", "value": "<a@x>\r\n <orig@customer.pt>"},
			map[string]any{"name": "Subject", "value": "ignored"},
		}})
	})
	msg := mailbox.Message{ProviderMessageID: "m/1"}
	require.NoError(t, c.ReplyHeaders(context.Background(), testMailbox, &msg))
	assert.Equal(t, "<orig@customer.pt>", msg.InReplyTo)
	assert.Equal(t, []string{"<a@x>", "<orig@customer.pt>"}, msg.References)
	assert.Equal(t, "/v1.0/users/quotes@customer.pt/messages/m/1", f.requests[0].Path)
}

func TestReplyHeadersToleratesAMessageGoneSinceDelta(t *testing.T) {
	_, c := newFakeGraph(t, func(w http.ResponseWriter, _ request) { writeJSON(w, 404, nil) })
	msg := mailbox.Message{ProviderMessageID: "m1"}
	require.NoError(t, c.ReplyHeaders(context.Background(), testMailbox, &msg))
}

func TestSubscriptionLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	renewStatus := 200
	f, c := newFakeGraph(t, func(w http.ResponseWriter, r request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, 201, map[string]any{"id": "sub-new", "expirationDateTime": r.Body["expirationDateTime"]})
		case http.MethodPatch:
			writeJSON(w, renewStatus, map[string]any{})
		case http.MethodDelete:
			writeJSON(w, 404, nil)
		}
	})

	created, err := c.Subscribe(context.Background(), testMailbox, "inbox", nil, now)
	require.NoError(t, err)
	assert.Equal(t, "sub-new", created.ProviderID)
	assert.Equal(t, now.Add(48*time.Hour), created.ExpiresAt)
	assert.Regexp(t, `^[0-9a-f]{64}$`, created.ClientStateSHA256)
	post := f.requests[0]
	assert.Equal(t, "created", post.Body["changeType"])
	assert.Equal(t, "https://hooks.example.com/graph", post.Body["notificationUrl"])
	assert.Equal(t, "users/quotes@customer.pt/mailFolders('inbox')/messages", post.Body["resource"])
	assert.NotEqual(t, created.ClientStateSHA256, post.Body["clientState"], "only the hash is persisted")

	unchanged, err := c.Subscribe(context.Background(), testMailbox, "inbox", created, now.Add(time.Hour))
	require.NoError(t, err)
	assert.Same(t, created, unchanged, "nothing is due more than a day before expiry")
	assert.Len(t, f.requests, 1)

	renewed, err := c.Subscribe(context.Background(), testMailbox, "inbox", created, now.Add(30*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, "sub-new", renewed.ProviderID)
	assert.Equal(t, now.Add(78*time.Hour), renewed.ExpiresAt)
	assert.Equal(t, http.MethodPatch, f.requests[1].Method)

	renewStatus = 404
	recreated, err := c.Subscribe(context.Background(), testMailbox, "inbox", created, now.Add(30*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, f.requests[len(f.requests)-1].Method, "a lost subscription is recreated")
	assert.NotEqual(t, created.ClientStateSHA256, recreated.ClientStateSHA256)

	require.NoError(t, c.Unsubscribe(context.Background(), testMailbox, *recreated), "an already-gone subscription is not an error")
}

func TestNoRedirectIsFollowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the bearer token followed a redirect")
	}))
	defer other.Close()
	_, c := newFakeGraph(t, func(w http.ResponseWriter, _ request) {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, err := c.Changes(context.Background(), testMailbox, "inbox", "", time.Now())
	require.Error(t, err)
	assert.False(t, errors.Is(err, mailbox.ErrCursorExpired))
}
