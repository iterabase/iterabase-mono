//go:build m365live

// Live Microsoft 365 checks (HOR-358, DES-HOR-358-04). They run against the
// dedicated test tenant in the `m365-live` CI job (`make m365-live-test`) and
// locally with a client secret. See docs/runbooks/microsoft-365-mailbox.md.
//
// Environment:
//
//	M365_TEST_TENANT_ID, M365_TEST_CLIENT_ID      the scoped test app (bound mailbox only)
//	M365_TEST_MAILBOX                             the bound mailbox
//	M365_TEST_OTHER_MAILBOX                       a mailbox outside the app's scope
//	M365_TEST_REPLIER_CLIENT_ID                   an app scoped to the other mailbox only
//	M365_TEST_CLIENT_SECRET, M365_TEST_REPLIER_CLIENT_SECRET
//	                                              local runs only; CI uses GitHub OIDC federation
package graph_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gateway"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox/graph"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

const (
	livePollInterval = 10 * time.Second
	deliveryDeadline = 3 * time.Minute
)

type liveEnv struct {
	tenant, mailbox, other string
	main, replier          *recordingTokens
}

func liveEnvironment(t *testing.T) liveEnv {
	t.Helper()
	get := func(name string) string {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			t.Fatalf("%s is required for the live Microsoft 365 checks", name)
		}
		return v
	}
	env := liveEnv{tenant: get("M365_TEST_TENANT_ID"), mailbox: strings.ToLower(get("M365_TEST_MAILBOX")),
		other: strings.ToLower(get("M365_TEST_OTHER_MAILBOX"))}
	env.main = newTokens(t, env.tenant, get("M365_TEST_CLIENT_ID"), os.Getenv("M365_TEST_CLIENT_SECRET"))
	env.replier = newTokens(t, env.tenant, get("M365_TEST_REPLIER_CLIENT_ID"), os.Getenv("M365_TEST_REPLIER_CLIENT_SECRET"))
	return env
}

// recordingTokens is a graph.TokenSource that remembers every token and
// credential it handled, so the leak scan can look for them.
type recordingTokens struct {
	inner   graph.TokenSource
	mu      sync.Mutex
	secrets []string
}

func (r *recordingTokens) Token(ctx context.Context, slot string) (string, error) {
	tok, err := r.inner.Token(ctx, slot)
	if err == nil {
		r.remember(tok)
	}
	return tok, err
}

func (r *recordingTokens) remember(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if v != "" {
			r.secrets = append(r.secrets, v)
		}
	}
}

func newTokens(t *testing.T, tenant, clientID, clientSecret string) *recordingTokens {
	t.Helper()
	rec := &recordingTokens{}
	if clientSecret != "" {
		// Local run: the exact product path (Secret → OAuth acquirer).
		secrets := gateway.NewFakeSecretResolver()
		name := mailbox.CredentialSecretName("live")
		secrets.Set(name, graph.SecretKeyTenantID, tenant)
		secrets.Set(name, graph.SecretKeyClientID, clientID)
		secrets.Set(name, graph.SecretKeyClientSecret, clientSecret)
		rec.inner = &graph.SlotTokens{Secrets: secrets, OAuth: gateway.NewHTTPOAuthAcquirer(nil)}
		rec.remember(clientSecret)
		return rec
	}
	if os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") == "" {
		t.Fatalf("set M365_TEST_CLIENT_SECRET for a local run, or run in the m365-live CI job (GitHub OIDC federation)")
	}
	rec.inner = &federatedTokens{tenant: tenant, clientID: clientID, rec: rec}
	return rec
}

// federatedTokens exchanges this job's GitHub OIDC token for an app-only Graph
// token (Entra workload identity federation, DES-HOR-358-04). No secret exists.
type federatedTokens struct {
	tenant, clientID string
	rec              *recordingTokens
	mu               sync.Mutex
	token            string
	expires          time.Time
}

func (f *federatedTokens) Token(ctx context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && time.Now().Before(f.expires) {
		return f.token, nil
	}
	assertion, err := githubOIDC(ctx)
	if err != nil {
		return "", err
	}
	f.rec.remember(assertion)
	form := url.Values{
		"client_id":             {f.clientID},
		"scope":                 {graph.Scope},
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graph.DefaultLoginBaseURL+"/"+f.tenant+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", &mailbox.CredentialError{Reason: "federated token exchange failed"}
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorCodes  []int  `json:"error_codes"` // AADSTS numbers; never secret
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", &mailbox.CredentialError{Reason: fmt.Sprintf("federated token exchange: HTTP %d %s AADSTS%v", resp.StatusCode, body.Error, body.ErrorCodes)}
	}
	f.token, f.expires = body.AccessToken, time.Now().Add(30*time.Minute)
	return f.token, nil
}

func githubOIDC(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")+"&audience="+url.QueryEscape("api://AzureADTokenExchange"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Value == "" {
		return "", fmt.Errorf("GitHub OIDC token request failed (HTTP %d)", resp.StatusCode)
	}
	return body.Value, nil
}

// graphCall is a direct Graph request for the test's own arrangement and
// cleanup (the replier and message deletion), outside the code under test.
func graphCall(ctx context.Context, tokens graph.TokenSource, method, path string, body any, out any) (int, error) {
	token, err := tokens.Token(ctx, "live")
	if err != nil {
		return 0, err
	}
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, graph.DefaultBaseURL+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

type graphItem struct {
	ID                string `json:"id"`
	Subject           string `json:"subject"`
	InternetMessageID string `json:"internetMessageId"`
	ConversationID    string `json:"conversationId"`
}

// findBySubject polls a folder for a message whose subject contains marker.
func findBySubject(ctx context.Context, t *testing.T, tokens graph.TokenSource, address, folder, marker string) graphItem {
	t.Helper()
	deadline := time.Now().Add(deliveryDeadline)
	for time.Now().Before(deadline) {
		var page struct {
			Value []graphItem `json:"value"`
		}
		status, err := graphCall(ctx, tokens, http.MethodGet, "/users/"+url.PathEscape(address)+"/mailFolders/"+folder+
			"/messages?$top=25&$orderby=receivedDateTime%20desc&$select=id,subject,internetMessageId,conversationId", nil, &page)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		for _, m := range page.Value {
			if strings.Contains(m.Subject, marker) {
				return m
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("no message with %q reached %s/%s within %s", marker, address, folder, deliveryDeadline)
	return graphItem{}
}

// cleanup deletes every message carrying marker from the folders the run used.
func cleanup(t *testing.T, tokens graph.TokenSource, address, marker string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, folder := range []string{"inbox", "sentitems", "drafts"} {
		var page struct {
			Value []graphItem `json:"value"`
		}
		if _, err := graphCall(ctx, tokens, http.MethodGet, "/users/"+url.PathEscape(address)+"/mailFolders/"+folder+
			"/messages?$top=50&$orderby=receivedDateTime%20desc&$select=id,subject", nil, &page); err != nil {
			t.Logf("cleanup %s/%s: %v", address, folder, err)
			continue
		}
		for _, m := range page.Value {
			if strings.Contains(m.Subject, marker) {
				_, _ = graphCall(ctx, tokens, http.MethodDelete, "/users/"+url.PathEscape(address)+"/messages/"+url.PathEscape(m.ID), nil, nil)
			}
		}
	}
}

type toolOutput struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Artifacts []struct {
		MimeType  string `json:"mimeType"`
		SizeBytes int    `json:"sizeBytes"`
	} `json:"artifacts"`
}

// invokeTool runs a released product tool bundle with an access token, exactly
// as the runner does after the gateway resolved the credential.
func invokeTool(ctx context.Context, t *testing.T, tokens graph.TokenSource, dir string, args any) (toolOutput, string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	runner := filepath.Join(filepath.Dir(file), "..", "..", "..", "tool-runner")
	token, err := tokens.Token(ctx, "live")
	require.NoError(t, err)
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, "node", "scripts/invoke-product-tool.mjs", "tools/product/"+dir, string(raw))
	cmd.Dir = runner
	cmd.Env = append(os.Environ(), "M365_TOOL_ACCESS_TOKEN="+token)
	stdout, err := cmd.Output()
	require.NoError(t, err, "invoke %s", dir)
	var out toolOutput
	require.NoError(t, json.Unmarshal(stdout, &out), string(stdout))
	return out, string(stdout)
}

// syncBuffer is a goroutine-safe log sink for the leak scan.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

//nolint:gocyclo // One sequential live proof of every HOR-358 acceptance criterion it owns.
func TestLiveMicrosoft365Mailbox(t *testing.T) {
	env := liveEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	nonce := make([]byte, 6)
	_, _ = rand.Read(nonce)
	marker := "IB-LIVE-" + strings.ToUpper(hex.EncodeToString(nonce))
	t.Cleanup(func() { cleanup(t, env.main, env.mailbox, marker); cleanup(t, env.replier, env.other, marker) })

	pool, _ := testutil.NewPostgres(t)
	store := mailbox.NewStore(pool)
	logs := &syncBuffer{}
	client := graph.New(env.main)
	src, err := store.UpsertOverlay(ctx, mailbox.Mailbox{Name: "live", OwnerRef: "live/live", Provider: mailbox.ProviderMicrosoft365,
		Address: env.mailbox, Folders: []string{"inbox"}, CredentialSlot: "live", PollInterval: livePollInterval})
	require.NoError(t, err)
	intake := &mailbox.Intake{Store: store, Sources: map[string]mailbox.Source{mailbox.ProviderMicrosoft365: client},
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Tick: 2 * time.Second}
	// Baseline: the first pass creates the folder cursor; existing mail is history.
	intake.RunOnce(ctx)
	status, err := store.GetCredentialStatus(ctx, "live")
	require.NoError(t, err)
	require.False(t, status.Failing, "baseline intake failed: %s", status.Reason)

	t.Run("reads outside the bound mailbox are denied by the tenant and the gateway", func(t *testing.T) {
		_, err := client.Changes(ctx, mailbox.Mailbox{Address: env.other, CredentialSlot: "live"}, "inbox", "", time.Now())
		var cred *mailbox.CredentialError
		require.ErrorAs(t, err, &cred, "the tenant must deny the app outside its RBAC for Applications scope")
		out, _ := invokeTool(ctx, t, env.main, "mailbox-read", map[string]any{"mailbox": env.other, "operation": "list_attachments", "messageId": "x"})
		require.NotNil(t, out.Error)
		assert.Equal(t, "access_denied", out.Error.Code)
		permitted, err := store.Permitted(ctx, mailbox.ProviderMicrosoft365, env.other, "live")
		require.NoError(t, err)
		assert.False(t, permitted, "the gateway scope admits only active mailboxes on the slot")
	})

	// Send from the bound mailbox; identifiers are known before the send.
	sendOut, sendRaw := invokeTool(ctx, t, env.main, "mailbox-send", map[string]any{
		"mailbox": env.mailbox, "to": env.other, "subject": "Live check " + marker, "body": "HOR-358 live check " + marker,
	})
	require.Nil(t, sendOut.Error, sendRaw)
	var sent struct {
		Status, InternetMessageID, ConversationID string
	}
	require.NoError(t, json.Unmarshal(sendOut.Result, &sent))
	require.Equal(t, "sent", sent.Status)
	require.NotEmpty(t, sent.InternetMessageID)
	require.NotEmpty(t, sent.ConversationID)

	// The other account receives it under the same Message-ID and replies.
	received := findBySubject(ctx, t, env.replier, env.other, "inbox", marker)
	assert.Equal(t, sent.InternetMessageID, received.InternetMessageID, "the draft's Message-ID survives the send")
	var draft graphItem
	status2, err := graphCall(ctx, env.replier, http.MethodPost, "/users/"+url.PathEscape(env.other)+"/messages/"+url.PathEscape(received.ID)+"/createReply",
		map[string]any{"comment": "Reply " + marker}, &draft)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status2)
	replySentAt := time.Now()
	status2, err = graphCall(ctx, env.replier, http.MethodPost, "/users/"+url.PathEscape(env.other)+"/messages/"+url.PathEscape(draft.ID)+"/send", nil, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, status2)

	// Polling-only intake (no notification URL) picks the reply up and records
	// it with the identifiers that match it to the send.
	runCtx, stop := context.WithCancel(ctx)
	go intake.Run(runCtx)
	var event mailbox.Event
	require.Eventually(t, func() bool {
		events, err := store.ClaimEvents(ctx, 50, time.Second)
		if err != nil {
			return false
		}
		for _, e := range events {
			if strings.Contains(e.Subject, marker) {
				event = e
				return true
			}
		}
		return false
	}, deliveryDeadline+livePollInterval, time.Second, "the reply was not recorded")
	stop()
	pickup := event.RecordedAt.Sub(replySentAt)
	t.Logf("reply recorded %s after it was sent (poll interval %s)", pickup.Round(time.Second), livePollInterval)
	assert.Equal(t, sent.InternetMessageID, event.InReplyTo, "In-Reply-To matches the sent Message-ID")
	assert.Contains(t, event.References, sent.InternetMessageID)
	assert.Equal(t, sent.ConversationID, event.ConversationID, "the reply joins the sent conversation")
	assert.Equal(t, strings.ToLower(env.other), event.SenderAddress)

	t.Run("each new message is recorded exactly once", func(t *testing.T) {
		// Two more passes, the second after a forced resync that replays the
		// overlap window, record nothing new.
		_, err := pool.Exec(ctx, `UPDATE mailbox.sources SET next_poll_at = now() - interval '1 second'`)
		require.NoError(t, err)
		intake.RunOnce(ctx)
		require.NoError(t, store.ResetCursor(ctx, src.ID, "inbox"))
		_, err = pool.Exec(ctx, `UPDATE mailbox.sources SET next_poll_at = now() - interval '1 second'`)
		require.NoError(t, err)
		intake.RunOnce(ctx)
		var count int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM mailbox.messages WHERE subject LIKE '%' || $1 || '%'`, marker).Scan(&count))
		assert.Equal(t, 1, count)
	})

	t.Run("the read tool returns the reply with its matching headers", func(t *testing.T) {
		out, raw := invokeTool(ctx, t, env.main, "mailbox-read", map[string]any{"mailbox": env.mailbox, "operation": "get_message", "messageId": event.ProviderMessageID})
		require.Nil(t, out.Error, raw)
		var msg struct {
			InternetMessageID, InReplyTo, ConversationID, BodyText string
		}
		require.NoError(t, json.Unmarshal(out.Result, &msg))
		assert.Equal(t, event.InternetMessageID, msg.InternetMessageID)
		assert.Equal(t, sent.InternetMessageID, msg.InReplyTo)
		assert.Contains(t, msg.BodyText, marker)
	})

	t.Run("credentials never reach logs, results or the database", func(t *testing.T) {
		var dump string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT coalesce(string_agg(t::text, ' '), '') FROM (
			  SELECT row_to_json(s)::text AS t FROM mailbox.sources s
			  UNION ALL SELECT row_to_json(c)::text FROM mailbox.folder_cursors c
			  UNION ALL SELECT row_to_json(m)::text FROM mailbox.messages m
			  UNION ALL SELECT row_to_json(x)::text FROM mailbox.credential_status x) rows`).Scan(&dump))
		haystack := logs.String() + sendRaw + dump
		for _, tokens := range []*recordingTokens{env.main, env.replier} {
			tokens.mu.Lock()
			for _, secret := range tokens.secrets {
				assert.NotContains(t, haystack, secret)
			}
			tokens.mu.Unlock()
		}
	})
}
