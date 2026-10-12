package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	v1 "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gatewayrpc/iterabase/gateway/v1"
)

// SecretResolver resolves a K8s SecretRef to its value. The production
// implementation reads Secrets via the in-cluster Kubernetes API (Secret-read
// RBAC scoped to the platform namespace); tests inject a fake. Values live only
// in gateway memory + the runner's per-invocation context (ARCH-008).
type SecretResolver interface {
	Resolve(ctx context.Context, ref SecretRef) (string, error)
}

// OAuthAcquirer performs an OAuth2 client-credentials grant. The gateway
// centralizes OAuth acquisition/refresh (ARCH-008): the runner receives a
// short-lived bearer token, never the client secret. Tests inject a fake.
type OAuthAcquirer interface {
	Acquire(ctx context.Context, tokenURL, clientID, clientSecret, scope string) (string, error)
}

// BearerSpec is the secret_ref JSONB spec for scheme=bearer.
type BearerSpec struct {
	ValueRef SecretRef `json:"value_ref"`
}

// OAuthSpec is the secret_ref JSONB spec for scheme=oauth_client_credentials.
type OAuthSpec struct {
	ClientID        string    `json:"client_id"`
	ClientSecretRef SecretRef `json:"client_secret_ref"`
	TokenURL        string    `json:"token_url"`
	Scope           string    `json:"scope"`
}

// resolveCredential builds one proto Credential for a binding, resolving secret
// values via the resolvers. The returned Credential is handed to the trusted
// runner over mTLS (ARCH-008).
func resolveCredential(ctx context.Context, b CredentialBinding, secrets SecretResolver, oauth OAuthAcquirer) (*v1.Credential, error) {
	switch b.Scheme {
	case CredBearer:
		var spec BearerSpec
		if err := unmarshalJSON(b.SecretRef, &spec); err != nil {
			return nil, fmt.Errorf("parse bearer spec: %w", err)
		}
		val, err := secrets.Resolve(ctx, spec.ValueRef)
		if err != nil {
			return nil, fmt.Errorf("resolve bearer secret: %w", err)
		}
		return &v1.Credential{
			Scheme:                  v1.CredentialScheme_CREDENTIAL_SCHEME_BEARER,
			BearerValue:             val,
			ResourceConstraintsJson: b.ResourceConstraints,
		}, nil
	case CredOAuthClientCredentials:
		var spec OAuthSpec
		if err := unmarshalJSON(b.SecretRef, &spec); err != nil {
			return nil, fmt.Errorf("parse oauth spec: %w", err)
		}
		secret, err := secrets.Resolve(ctx, spec.ClientSecretRef)
		if err != nil {
			return nil, fmt.Errorf("resolve oauth client secret: %w", err)
		}
		token, err := oauth.Acquire(ctx, spec.TokenURL, spec.ClientID, secret, spec.Scope)
		if err != nil {
			return nil, fmt.Errorf("oauth acquire: %w", err)
		}
		return &v1.Credential{
			Scheme:                  v1.CredentialScheme_CREDENTIAL_SCHEME_OAUTH_CLIENT_CREDENTIALS,
			BearerValue:             token, // the acquired access token; runner uses it as a bearer
			ResourceConstraintsJson: b.ResourceConstraints,
			OauthClientId:           spec.ClientID,
			OauthTokenUrl:           spec.TokenURL,
			OauthScope:              spec.Scope,
		}, nil
	}
	return nil, fmt.Errorf("unknown credential scheme %q", b.Scheme)
}

// resolveCredentialContext resolves the slot bindings for a pool+tool into a
// CredentialContext keyed by slot name, validated against the pinned
// descriptor's declared credential_slots (ARCH-008). The gateway resolves
// EXACTLY the logical slots the descriptor declared: every required slot must
// have a binding with a matching scheme; no extra/undeclared binding is sent to
// the runner. A mismatch fails closed before dispatch.
func resolveCredentialContext(ctx context.Context, poolID, toolName string, tv ToolVersion, store *Store, secrets SecretResolver, oauth OAuthAcquirer) (*v1.CredentialContext, error) {
	declared, err := parseCredentialSlots(tv.CredentialSlots)
	if err != nil {
		return nil, fmt.Errorf("parse declared credential slots: %w", err)
	}
	bindings, err := store.ResolveCredentialBindings(ctx, poolID, toolName)
	if err != nil {
		return nil, err
	}
	bindingByName := make(map[string]CredentialBinding, len(bindings))
	for _, b := range bindings {
		bindingByName[b.SlotName] = b
	}
	slots := make(map[string]*v1.Credential, len(declared))
	for _, slot := range declared {
		b, ok := bindingByName[slot.Name]
		if !ok {
			if slot.Required {
				return nil, fmt.Errorf("required credential slot %q has no binding (ARCH-008)", slot.Name)
			}
			continue // optional slot, intentionally unbound
		}
		if b.Scheme != slot.Scheme {
			return nil, fmt.Errorf("credential slot %q scheme mismatch: binding %q != declared %q (ARCH-008)",
				slot.Name, b.Scheme, slot.Scheme)
		}
		cred, err := resolveCredential(ctx, b, secrets, oauth)
		if err != nil {
			return nil, fmt.Errorf("slot %q: %w", b.SlotName, err)
		}
		slots[b.SlotName] = cred
		delete(bindingByName, slot.Name)
	}
	if len(bindingByName) > 0 {
		extras := make([]string, 0, len(bindingByName))
		for name := range bindingByName {
			extras = append(extras, name)
		}
		return nil, fmt.Errorf("undeclared credential bindings for tool %s would leak to runner: %v (ARCH-008)", toolName, extras)
	}
	return &v1.CredentialContext{Slots: slots}, nil
}

// declaredCredentialSlot is a parsed row of a ToolVersion's credential_slots
// JSONB: {name, scheme, binding_schema, required}.
type declaredCredentialSlot struct {
	Name     string           `json:"name"`
	Scheme   CredentialScheme `json:"scheme"`
	Required bool             `json:"required"`
}

func parseCredentialSlots(raw []byte) ([]declaredCredentialSlot, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var slots []declaredCredentialSlot
	if err := json.Unmarshal(raw, &slots); err != nil {
		return nil, err
	}
	return slots, nil
}

// --- production implementations ---

// FakeSecretResolver is an in-memory SecretResolver for tests. (Production
// uses k8sSecretResolver in cmd/gateway.)
type FakeSecretResolver struct {
	mu    sync.Mutex
	store map[string]string // key: name+"/"+key
}

// NewFakeSecretResolver returns a SecretResolver backed by an in-memory map.
// Tests seed values with Set.
func NewFakeSecretResolver() *FakeSecretResolver {
	return &FakeSecretResolver{store: make(map[string]string)}
}

// Set seeds a secret value (test helper).
func (f *FakeSecretResolver) Set(name, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[name+"/"+key] = value
}

func (f *FakeSecretResolver) Resolve(_ context.Context, ref SecretRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.store[ref.Name+"/"+ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s not found", ref.Name, ref.Key)
	}
	return v, nil
}

// httpOAuthAcquirer performs a real OAuth2 client-credentials grant over HTTP.
type httpOAuthAcquirer struct{ client *http.Client }

// NewHTTPOAuthAcquirer returns an OAuthAcquirer that performs real grants. The
// grant never follows redirects: a 307/308 would replay the form, client secret
// included, to a location the token endpoint chose.
func NewHTTPOAuthAcquirer(client *http.Client) OAuthAcquirer {
	c := http.Client{Timeout: 10 * time.Second}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &httpOAuthAcquirer{client: &c}
}

// Acquire errors surface in caller responses and the durable invocation record,
// so they are built only from the configured token URL, the numeric status and
// an allow-listed RFC 6749 error code. Nothing the token endpoint or transport
// chose (body, status reason, redirect location, error text) is included, since
// any of it may echo the submitted client secret (ARCH-008, HOR-650).
func (h *httpOAuthAcquirer) Acquire(ctx context.Context, tokenURL, clientID, clientSecret, scope string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	if scope != "" {
		form.Set("scope", scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("oauth token endpoint %q: invalid token URL", tokenURL)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "", fmt.Errorf("oauth token endpoint %s: request timed out", tokenURL)
		}
		return "", fmt.Errorf("oauth token endpoint %s: request failed", tokenURL)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("oauth token endpoint %s: HTTP %d: reading response failed", tokenURL, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		if code := oauthErrorCode(body); code != "" {
			return "", fmt.Errorf("oauth token endpoint %s: HTTP %d: %s", tokenURL, resp.StatusCode, code)
		}
		return "", fmt.Errorf("oauth token endpoint %s: HTTP %d", tokenURL, resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("oauth token endpoint %s: unparseable token response", tokenURL)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("oauth token endpoint %s: token response missing access_token", tokenURL)
	}
	return tok.AccessToken, nil
}

// oauthErrorCodes is the closed RFC 6749 §5.2 token error code set.
var oauthErrorCodes = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true,
	"unauthorized_client": true, "unsupported_grant_type": true, "invalid_scope": true,
}

// oauthErrorCode extracts the RFC 6749 §5.2 error code from a token error
// response. Only a code from the closed set is returned, so no provider-chosen
// text (which could echo request values) passes through; otherwise "".
func oauthErrorCode(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || !oauthErrorCodes[e.Error] {
		return ""
	}
	return e.Error
}

// fakeOAuthAcquirer returns a canned token for tests.
type fakeOAuthAcquirer struct{ token string }

// NewFakeOAuthAcquirer returns an OAuthAcquirer that returns the given token.
func NewFakeOAuthAcquirer(token string) OAuthAcquirer { return &fakeOAuthAcquirer{token: token} }

func (f *fakeOAuthAcquirer) Acquire(_ context.Context, _, _, _, _ string) (string, error) {
	return f.token, nil
}
