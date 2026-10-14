package gateway_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gateway"
	v1 "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/gatewayrpc/iterabase/gateway/v1"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/mailbox"
)

// TestGateway_MailboxToolScope proves the gateway side of mailbox scoping
// (HOR-358, DES-HOR-358-03): a platform mailbox tool runs only for an active
// mailbox on the credential slot bound to the tool. Anything else is denied
// before dispatch.
func TestGateway_MailboxToolScope(t *testing.T) {
	secrets := gateway.NewFakeSecretResolver()
	secrets.Set(mailbox.CredentialSecretName("m365"), "client_secret", "entra-secret")
	env := newTestEnv(t, secrets)
	ctx := context.Background()
	const tool = "platform.mailbox.read"
	require.NoError(t, env.store.UpsertPoolGrant(ctx, env.poolID, tool, gateway.EffectReadOnly, nil))
	bind := func(secretName string) {
		require.NoError(t, env.store.UpsertCredentialBinding(ctx, env.poolID, tool, "microsoft365",
			gateway.CredOAuthClientCredentials,
			[]byte(fmt.Sprintf(`{"client_id":"app","client_secret_ref":{"name":%q,"key":"client_secret"},"token_url":"https://login.example/t/oauth2/v2.0/token","scope":"https://graph.microsoft.com/.default"}`, secretName)),
			[]byte(`{}`)))
	}
	bind(mailbox.CredentialSecretName("m365"))

	mailboxes := mailbox.NewStore(env.pgpool)
	_, err := mailboxes.UpsertOverlay(ctx, mailbox.Mailbox{Name: "quotes", OwnerRef: "customer/quotes", Provider: mailbox.ProviderMicrosoft365,
		Address: "quotes@customer.pt", Folders: []string{"inbox"}, CredentialSlot: "m365"})
	require.NoError(t, err)
	_, err = mailboxes.UpsertOverlay(ctx, mailbox.Mailbox{Name: "finance", OwnerRef: "customer/finance", Provider: mailbox.ProviderMicrosoft365,
		Address: "finance@customer.pt", Folders: []string{"inbox"}, CredentialSlot: "finance-app"})
	require.NoError(t, err)

	dispatched := 0
	rr := startRefRunner(t, env, &v1.ToolDescriptor{
		Name: tool, Version: "1.0.0", Digest: "sha256:mailbox-read-1", Description: "read mail",
		EffectClass: v1.EffectClass_EFFECT_CLASS_READ_ONLY, InputSchema: []byte(`{"type":"object"}`), Timeout: durationPtr(5 * time.Second),
		CredentialSlots: []*v1.CredentialSlot{{Name: "microsoft365", Scheme: v1.CredentialScheme_CREDENTIAL_SCHEME_OAUTH_CLIENT_CREDENTIALS, Required: true}},
	}, func(*v1.Invoke) (*v1.InvokeResult, bool) {
		dispatched++
		return &v1.InvokeResult{State: v1.InvokeState_INVOKE_STATE_SUCCEEDED, ResultJson: []byte(`{}`)}, false
	})
	t.Cleanup(rr.close)

	runID, turnID := seedTurnAttempt(t, env)
	gc := gatewayClient(env, env.supervisor)
	call := 0
	invoke := func(args string) *v1.InvokeResponse {
		call++
		resp, err := gc.InvokeTool(ctx, connect.NewRequest(&v1.InvokeRequest{
			AttemptId: runID, CallerScope: v1.CallerScope_CALLER_SCOPE_TURN, CallerScopeId: turnID, FencingGeneration: 1,
			ToolCallId: fmt.Sprintf("call-%d", call), ToolName: tool, ToolVersionDigest: "sha256:mailbox-read-1",
			ArgumentsJson: []byte(args), IdempotencyKey: fmt.Sprintf("k-%d", call),
		}))
		require.NoError(t, err)
		return resp.Msg
	}
	denied := func(t *testing.T, resp *v1.InvokeResponse) {
		t.Helper()
		assert.Equal(t, v1.InvokeState_INVOKE_STATE_FAILED, resp.State)
		require.NotNil(t, resp.Error)
		assert.Equal(t, "permission_denied", resp.Error.Code)
	}

	// Without a scope the gateway denies every mailbox tool.
	denied(t, invoke(`{"mailbox":"quotes@customer.pt","operation":"get_message","messageId":"m"}`))

	env.svc.SetMailboxScope(mailboxes)
	resp := invoke(`{"mailbox":"Quotes@Customer.pt","operation":"get_message","messageId":"m"}`)
	assert.Equal(t, v1.InvokeState_INVOKE_STATE_SUCCEEDED, resp.State)
	assert.Equal(t, 1, dispatched)

	denied(t, invoke(`{"mailbox":"ceo@customer.pt","operation":"get_message","messageId":"m"}`))
	denied(t, invoke(`{"mailbox":"finance@customer.pt","operation":"get_message","messageId":"m"}`)) // other credential slot
	denied(t, invoke(`{"operation":"get_message","messageId":"m"}`))

	require.NoError(t, mailboxes.DisableOverlay(ctx, "customer/quotes"))
	denied(t, invoke(`{"mailbox":"quotes@customer.pt","operation":"get_message","messageId":"m"}`))

	// A binding outside the credential-slot Secret convention is not a slot.
	_, err = mailboxes.UpsertOverlay(ctx, mailbox.Mailbox{Name: "quotes", OwnerRef: "customer/quotes", Provider: mailbox.ProviderMicrosoft365,
		Address: "quotes@customer.pt", Folders: []string{"inbox"}, CredentialSlot: "m365"})
	require.NoError(t, err)
	bind("hand-made-secret")
	denied(t, invoke(`{"mailbox":"quotes@customer.pt","operation":"get_message","messageId":"m"}`))
	assert.Equal(t, 1, dispatched, "denied invocations are never dispatched")
}
