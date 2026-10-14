package graph

import (
	"context"
	"crypto/sha256"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gateway"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
)

// Credential slot Secret keys for an Entra app registration (client
// credentials). HOR-652 writes the same keys.
const (
	SecretKeyTenantID     = "tenant_id"
	SecretKeyClientID     = "client_id"
	SecretKeyClientSecret = "client_secret"
)

// DefaultLoginBaseURL is the Microsoft identity platform authority.
const DefaultLoginBaseURL = "https://login.microsoftonline.com"

// Scope is the app-only Graph scope; granted application permissions apply.
const Scope = "https://graph.microsoft.com/.default"

// tenantPattern accepts a tenant GUID or verified domain, so the value cannot
// change the token endpoint path.
var tenantPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,253}$`)

// tokenTTL is kept well under Entra's minimum access-token lifetime.
const tokenTTL = 30 * time.Minute

// SlotTokens resolves a credential slot's installation-owned Secret
// ([mailbox.CredentialSecretName]) and acquires an app-only token through the
// gateway's OAuth acquirer (HOR-650, ARCH-008). The Secret is read on every
// call so a replaced value takes effect at once; tokens are cached per exact
// credential values.
type SlotTokens struct {
	Secrets      gateway.SecretResolver
	OAuth        gateway.OAuthAcquirer
	LoginBaseURL string
	Now          func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken // by slot
}

type cachedToken struct {
	fingerprint [32]byte
	token       string
	expires     time.Time
}

// Token implements TokenSource.
func (s *SlotTokens) Token(ctx context.Context, slot string) (string, error) {
	name := mailbox.CredentialSecretName(slot)
	values := make(map[string]string, 3)
	for _, key := range []string{SecretKeyTenantID, SecretKeyClientID, SecretKeyClientSecret} {
		v, err := s.Secrets.Resolve(ctx, gateway.SecretRef{Name: name, Key: key})
		if err != nil || strings.TrimSpace(v) == "" {
			return "", &mailbox.CredentialError{Reason: "The Microsoft 365 credentials are not set"}
		}
		values[key] = strings.TrimSpace(v)
	}
	if !tenantPattern.MatchString(values[SecretKeyTenantID]) {
		return "", &mailbox.CredentialError{Reason: "The Microsoft 365 tenant ID is not valid"}
	}
	fingerprint := sha256.Sum256([]byte(values[SecretKeyTenantID] + "\x00" + values[SecretKeyClientID] + "\x00" + values[SecretKeyClientSecret]))
	now := s.now()
	s.mu.Lock()
	if c, ok := s.cache[slot]; ok && c.fingerprint == fingerprint && now.Before(c.expires) {
		s.mu.Unlock()
		return c.token, nil
	}
	s.mu.Unlock()

	tokenURL := s.loginBase() + "/" + values[SecretKeyTenantID] + "/oauth2/v2.0/token"
	token, err := s.OAuth.Acquire(ctx, tokenURL, values[SecretKeyClientID], values[SecretKeyClientSecret], Scope)
	if err != nil {
		// The acquirer's error carries only the token URL, status and an RFC
		// 6749 code (HOR-650); the customer reason carries none of it.
		return "", &mailbox.CredentialError{Reason: "Microsoft 365 rejected the credentials"}
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = make(map[string]cachedToken)
	}
	s.cache[slot] = cachedToken{fingerprint: fingerprint, token: token, expires: now.Add(tokenTTL)}
	s.mu.Unlock()
	return token, nil
}

func (s *SlotTokens) loginBase() string {
	if s.LoginBaseURL != "" {
		return strings.TrimRight(s.LoginBaseURL, "/")
	}
	return DefaultLoginBaseURL
}

func (s *SlotTokens) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
