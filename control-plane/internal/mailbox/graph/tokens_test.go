package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gateway"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
)

type recordingAcquirer struct {
	calls []string
	err   error
}

func (r *recordingAcquirer) Acquire(_ context.Context, tokenURL, clientID, clientSecret, scope string) (string, error) {
	r.calls = append(r.calls, tokenURL+"|"+clientID+"|"+clientSecret+"|"+scope)
	if r.err != nil {
		return "", r.err
	}
	return "token-" + clientSecret, nil
}

func seededSlot(secrets *gateway.FakeSecretResolver, tenant, client, secret string) {
	name := mailbox.CredentialSecretName("m365")
	secrets.Set(name, SecretKeyTenantID, tenant)
	secrets.Set(name, SecretKeyClientID, client)
	secrets.Set(name, SecretKeyClientSecret, secret)
}

func TestSlotTokensAcquireFromTheConventionSecretAndCache(t *testing.T) {
	secrets := gateway.NewFakeSecretResolver()
	seededSlot(secrets, "contoso.onmicrosoft.com", "client-1", "s1")
	oauth := &recordingAcquirer{}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tokens := &SlotTokens{Secrets: secrets, OAuth: oauth, Now: func() time.Time { return now }}

	tok, err := tokens.Token(context.Background(), "m365")
	require.NoError(t, err)
	assert.Equal(t, "token-s1", tok)
	assert.Equal(t, []string{"https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/token|client-1|s1|https://graph.microsoft.com/.default"}, oauth.calls)

	_, err = tokens.Token(context.Background(), "m365")
	require.NoError(t, err)
	assert.Len(t, oauth.calls, 1, "a current token is reused")

	// A replaced value takes effect at once (HOR-652 contract).
	seededSlot(secrets, "contoso.onmicrosoft.com", "client-1", "s2")
	tok, err = tokens.Token(context.Background(), "m365")
	require.NoError(t, err)
	assert.Equal(t, "token-s2", tok)

	now = now.Add(31 * time.Minute)
	_, err = tokens.Token(context.Background(), "m365")
	require.NoError(t, err)
	assert.Len(t, oauth.calls, 3, "an aged token is refreshed")
}

func TestSlotTokensFailuresAreCustomerSafe(t *testing.T) {
	for name, tc := range map[string]struct {
		seed   func(*gateway.FakeSecretResolver)
		oauth  error
		reason string
	}{
		"unset slot":     {seed: func(*gateway.FakeSecretResolver) {}, reason: "The Microsoft 365 credentials are not set"},
		"path in tenant": {seed: func(s *gateway.FakeSecretResolver) { seededSlot(s, "../evil", "c", "s") }, reason: "The Microsoft 365 tenant ID is not valid"},
		"rejected by Entra": {seed: func(s *gateway.FakeSecretResolver) { seededSlot(s, "tenant", "c", "super-secret") },
			oauth: errors.New("oauth token endpoint: HTTP 401: invalid_client super-secret"), reason: "Microsoft 365 rejected the credentials"},
	} {
		t.Run(name, func(t *testing.T) {
			secrets := gateway.NewFakeSecretResolver()
			tc.seed(secrets)
			tokens := &SlotTokens{Secrets: secrets, OAuth: &recordingAcquirer{err: tc.oauth}}
			_, err := tokens.Token(context.Background(), "m365")
			var cred *mailbox.CredentialError
			require.ErrorAs(t, err, &cred)
			assert.Equal(t, tc.reason, cred.Reason)
			assert.NotContains(t, err.Error(), "super-secret")
		})
	}
}
