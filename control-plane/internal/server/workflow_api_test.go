package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/workflow"
)

// Secret execution details the customer workflow catalogue must never expose.
var workflowSecrets = []string{
	"SECRET-PROMPT-intake", "secret-model-ref", "secret-skill", "secret.tool.write",
	"secret-pool", "secret-value-model", "secretSchemaField", "responseSchema", "outputSchema",
}

// seedCatalogueWorkflow registers a Ready workflow whose graph carries prompts,
// models, skills, capabilities, a pool, a value model and schemas.
func seedCatalogueWorkflow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var scopeID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO identity.identities(key,kind,source,display_name)VALUES('workflow:default/quotes','workflow','local','Quotes')RETURNING id`).Scan(&scopeID))
	text := func(en, pt string) workflow.CanonicalLocalizedText {
		return workflow.CanonicalLocalizedText{EN: en, PT: pt}
	}
	spec := workflow.CanonicalSpec{
		Key: "default/quotes", ScopeIdentityKey: "workflow:default/quotes", PoolRef: "secret-pool",
		DefaultModelRef: "secret-model-ref", ValueModelRef: "secret-value-model",
		Source: workflow.CanonicalSource{Type: "manual_api"},
		Skills: []workflow.CanonicalSkill{{Name: "secret-skill", Version: "1", Digest: "sha256:skill"}},
		RequestedCapabilities: []workflow.CanonicalCapability{{
			Tool: "secret.tool.write", MaxEffectClass: "idempotent_write",
		}},
		Graph: workflow.CanonicalGraph{EntryNode: "intake", MaxTransitions: 20,
			Nodes: []workflow.CanonicalNode{
				{Key: "send", Label: text("Sending quote", "A enviar cotação"), Kind: workflow.NodeAgentTask,
					Prompt: "SECRET-PROMPT-send", Outcomes: []string{"sent"}},
				{Key: "approve", Label: text("Waiting for approval", "A aguardar aprovação"), Kind: workflow.NodeHumanGate,
					Outcomes: []string{"approved"},
					HumanGate: &workflow.CanonicalHumanGate{
						Type: "approval", Title: text("Approve the quote", "Aprovar a cotação"),
						ResponseSchema: json.RawMessage(`{"type":"object","properties":{"secretSchemaField":{"type":"string"}}}`),
					}},
				{Key: "intake", Label: text("Reading the request", "A ler o pedido"), Kind: workflow.NodeAgentTask,
					Prompt: "SECRET-PROMPT-intake", ModelRef: "secret-model-ref", Skills: []string{"secret-skill"},
					Capabilities: []string{"secret.tool.write"}, Outcomes: []string{"drafted"},
					OutputSchema: json.RawMessage(`{"type":"object","properties":{"secretSchemaField":{"type":"string"}}}`)},
			},
			Edges: []workflow.CanonicalEdge{
				{From: "intake", Outcome: "drafted", To: "approve"},
				{From: "approve", Outcome: "approved", To: "send"},
			},
			TerminalOutcomes: []workflow.CanonicalTerminalOutcome{{Node: "send", Outcome: "sent"}}},
		Presentation: workflow.CanonicalPresentation{
			Title: workflow.CanonicalText{EN: "Quotes", PT: "Cotações"},
			Coworker: workflow.CanonicalCoworker{
				Name: "Beatriz", Mark: workflow.CanonicalCoworkerMark{Shape: "petal", Color: "violet"},
				Role:    workflow.CanonicalText{EN: "Quotation coordinator", PT: "Coordenadora de cotações"},
				Summary: workflow.CanonicalText{EN: "Prepares quotes.", PT: "Prepara cotações."},
				OnItsOwn: []workflow.CanonicalOnItsOwnLine{
					{Text: workflow.CanonicalText{EN: "Reads requests", PT: "Lê pedidos"}, Kind: "reads"},
					{
						Text: workflow.CanonicalText{EN: "Emails suppliers", PT: "Envia emails a fornecedores"}, Kind: "sends",
						Detail: &workflow.CanonicalText{EN: "Pre-approved", PT: "Pré-aprovados"},
						Items:  []string{"rates@northsealines.example"},
					},
				},
				Connections: []workflow.CanonicalText{{EN: "Outlook", PT: "Outlook"}},
			},
		},
	}
	specJSON, err := json.Marshal(spec)
	require.NoError(t, err)
	presentation, err := json.Marshal(spec.Presentation)
	require.NoError(t, err)
	_, err = workflow.NewStore(pool).RegisterDefinition(ctx, workflow.Definition{
		Key: "default/quotes", Version: "3", Digest: "sha256:quotes-v3", SpecJSON: specJSON,
		ValidationStatus: workflow.ValidationValid, ScopeIdentityID: scopeID, SourceType: "manual_api",
		PoolKey: "default/secret-pool", Presentation: presentation,
	})
	require.NoError(t, err)
}

func (h *authAPIHarness) raw(path string, headers map[string]string) (int, string) {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Origin", authTestOrigin)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// TestWorkflowCatalogueIsCustomerSafe proves GET /v1/workflows and
// GET /v1/workflows/{key} return the exact customer-safe summary to every role
// holding workflows.read, never leak execution details, 404 an unknown key,
// and refuse unauthenticated or under-privileged callers.
func TestWorkflowCatalogueIsCustomerSafe(t *testing.T) {
	h := newAuthAPIHarness(t)
	ctx := context.Background()
	seedManualValueWorkflow(t, ctx, h.pool)
	seedCatalogueWorkflow(t, ctx, h.pool)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)
	reader := h.personalKey(operator.ID, identity.ActionWorkflowsRead)

	expected := `{
		"key":"default/quotes","version":"3",
		"title":{"en":"Quotes","pt":"Cotações"},
		"coworker":{"name":"Beatriz","mark":{"shape":"petal","color":"violet"},
			"role":{"en":"Quotation coordinator","pt":"Coordenadora de cotações"},
			"summary":{"en":"Prepares quotes.","pt":"Prepara cotações."},
			"onItsOwn":[
				{"text":{"en":"Reads requests","pt":"Lê pedidos"},"kind":"reads"},
				{"text":{"en":"Emails suppliers","pt":"Envia emails a fornecedores"},"kind":"sends",
					"detail":{"en":"Pre-approved","pt":"Pré-aprovados"},"items":["rates@northsealines.example"]}],
			"connections":[{"en":"Outlook","pt":"Outlook"}]},
		"steps":[
			{"key":"intake","kind":"agent_task","label":{"en":"Reading the request","pt":"A ler o pedido"}},
			{"key":"approve","kind":"human_gate","label":{"en":"Waiting for approval","pt":"A aguardar aprovação"}},
			{"key":"send","kind":"agent_task","label":{"en":"Sending quote","pt":"A enviar cotação"}}],
		"alwaysAsks":[{"nodeKey":"approve","type":"approval","title":{"en":"Approve the quote","pt":"Aprovar a cotação"}}]}`

	var bodies []string
	for name, headers := range map[string]map[string]string{
		"admin session":      admin.headers(),
		"operator session":   operatorSession.headers(),
		"workflows.read key": {"Authorization": "Bearer " + reader},
	} {
		status, body := h.raw("/v1/workflows", headers)
		require.Equal(t, http.StatusOK, status, "%s: %s", name, body)
		var list struct {
			Workflows []json.RawMessage `json:"workflows"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &list), name)
		require.Len(t, list.Workflows, 2, name)
		// Sorted by coworker name: Beatriz before Marco.
		assert.JSONEq(t, expected, string(list.Workflows[0]), name)
		assert.Contains(t, string(list.Workflows[1]), `"name":"Marco"`, name)

		status, one := h.raw("/v1/workflows/default%2Fquotes", headers)
		require.Equal(t, http.StatusOK, status, "%s: %s", name, one)
		assert.JSONEq(t, expected, one, name)
		bodies = append(bodies, body, one)
	}
	for _, body := range bodies {
		for _, secret := range workflowSecrets {
			assert.NotContains(t, body, secret)
		}
	}

	status, body := h.raw("/v1/workflows/default%2Funknown", operatorSession.headers())
	assert.Equal(t, http.StatusNotFound, status)
	assert.JSONEq(t, `{"error":"workflow not found"}`, body)

	status, _ = h.raw("/v1/workflows", nil)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, _ = h.raw("/v1/workflows/default%2Fquotes", nil)
	assert.Equal(t, http.StatusUnauthorized, status)
	workOnly := h.personalKey(operator.ID, identity.ActionWorkRead)
	status, _ = h.raw("/v1/workflows", map[string]string{"Authorization": "Bearer " + workOnly})
	assert.Equal(t, http.StatusForbidden, status)
}

// TestWorkItemsFilterByWorkflowKeyAndOperatorSeesActorNames proves the
// workflowKey list filter is an exact match and that an Operator reads the
// display name (never the email) of the person behind a timeline event.
func TestWorkItemsFilterByWorkflowKeyAndOperatorSeesActorNames(t *testing.T) {
	h := newAuthAPIHarness(t)
	ctx := context.Background()
	seedManualValueWorkflow(t, ctx, h.pool)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)

	headers := operatorSession.unsafeHeaders()
	headers["Idempotency-Key"] = "filter-1"
	started := h.do(http.MethodPost, "/v1/work-items", map[string]any{
		"workflowKey": "walter/manual", "title": "Quotation — ACME", "source": map[string]any{"m": 1},
		"sourcePresentation": map[string]any{"kind": "email", "title": "Quote", "evidence": []any{}},
	}, headers)
	require.Equal(t, http.StatusCreated, started.status, "%v", started.body)
	itemID, _ := started.body["id"].(string)

	assert.Len(t, h.list("/v1/work-items?workflowKey=walter%2Fmanual", operatorSession.headers()), 1)
	assert.Empty(t, h.list("/v1/work-items?workflowKey=walter", operatorSession.headers()), "exact match only")
	assert.Empty(t, h.list("/v1/work-items?workflowKey=default%2Fquotes", operatorSession.headers()))
	assert.Len(t, h.list("/v1/work-items", operatorSession.headers()), 1)

	for name, session := range map[string]signedIn{"operator": operatorSession, "admin": admin} {
		events := h.list("/v1/work-items/"+itemID+"/timeline", session.headers())
		var attributed bool
		for _, event := range events {
			if event["actorIdentityId"] == operator.ID {
				assert.Equal(t, "Person", event["actorDisplayName"], name)
				attributed = true
			}
		}
		assert.True(t, attributed, "%s sees the person behind the start", name)
		raw, err := json.Marshal(events)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "operator@example.com", name)
	}
}

// TestTimeBackIsVisibleToEveryRoleWithoutMoney proves the Dashboard timeBack
// block (DES-HOR-459-02) is identical for an Admin and an Operator, while the
// euro value stays clipped for the Operator and timeBack carries no money.
func TestTimeBackIsVisibleToEveryRoleWithoutMoney(t *testing.T) {
	h := newAuthAPIHarness(t)
	ctx := context.Background()
	seedManualValueWorkflow(t, ctx, h.pool)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)

	headers := operatorSession.unsafeHeaders()
	headers["Idempotency-Key"] = "time-back-1"
	started := h.do(http.MethodPost, "/v1/work-items", map[string]any{
		"workflowKey": "walter/manual", "title": "Quotation — ACME", "source": map[string]any{"m": 1},
		"sourcePresentation": map[string]any{"kind": "email", "title": "Quote", "evidence": []any{}},
	}, headers)
	require.Equal(t, http.StatusCreated, started.status, "%v", started.body)
	itemID, _ := started.body["id"].(string)
	// Credit the completion exactly as the engine does: the attempt's immutable
	// value-model snapshot (baselineSeconds 1200) priced the entry.
	_, err := h.pool.Exec(ctx, `
		INSERT INTO work.value_ledger (work_item_id,attempt_id,value_model_id,kind,amount,currency,formula_snapshot)
		SELECT wi.id, a.id, a.value_model_id, 'completion_credit', 10, 'EUR', a.value_model_snapshot
		FROM work.work_items wi JOIN work.attempts a ON a.work_item_id = wi.id
		WHERE wi.id = $1`, itemID)
	require.NoError(t, err)

	timeBack := map[string]any{}
	for name, session := range map[string]signedIn{"admin": admin, "operator": operatorSession} {
		dashboard := h.do(http.MethodGet, "/v1/work-dashboard", nil, session.headers())
		require.Equal(t, http.StatusOK, dashboard.status, name)
		got, _ := dashboard.body["timeBack"].(map[string]any)
		timeBack[name] = got
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		assert.JSONEq(t, `{"configured":true,"seconds":1200,"byWorkflow":[{"workflowKey":"walter/manual","seconds":1200}]}`, string(raw), name)
		assert.NotContains(t, string(raw), "EUR", name)
		value, _ := dashboard.body["value"].(map[string]any)
		assert.Equal(t, name == "admin", value["configured"], "%s euro value visibility", name)
		if name == "operator" {
			assert.Empty(t, value["totals"], "an Operator never receives euro totals")
		}
	}
	assert.Equal(t, timeBack["admin"], timeBack["operator"])
}

// TestWorkItemsCarryWhoAnsweredTheLastApprovalOrDecision proves the
// work-item list and detail return answeredBy (DES-HOR-459-10): the display
// name of the person who answered the most recent resolved approval or
// decision gate across attempts, ignoring information gates, and never an
// email-like name.
func TestWorkItemsCarryWhoAnsweredTheLastApprovalOrDecision(t *testing.T) {
	h := newAuthAPIHarness(t)
	ctx := context.Background()
	seedManualValueWorkflow(t, ctx, h.pool)
	admin := h.provisionAdmin("admin@example.com", "admin-long-password")
	operator := h.activeUser("operator@example.com", "operator", "operator-long-password")
	operatorSession := h.signIn("operator@example.com", "operator-long-password", operator)
	_, err := h.pool.Exec(ctx, `UPDATE identity.local_users SET display_name='Rita Lopes' WHERE identity_id=$1`, operator.ID)
	require.NoError(t, err)

	headers := operatorSession.unsafeHeaders()
	headers["Idempotency-Key"] = "answered-1"
	started := h.do(http.MethodPost, "/v1/work-items", map[string]any{
		"workflowKey": "walter/manual", "title": "Quotation — ACME", "source": map[string]any{"m": 1},
		"sourcePresentation": map[string]any{"kind": "email", "title": "Quote", "evidence": []any{}},
	}, headers)
	require.Equal(t, http.StatusCreated, started.status, "%v", started.body)
	itemID, _ := started.body["id"].(string)

	resolve := func(kind, responder, resolvedAgo string) {
		t.Helper()
		_, err := h.pool.Exec(ctx, `
			INSERT INTO work.blockers (work_item_id,attempt_id,kind,title,description,state,response_outcome,responded_by,resolved_at)
			SELECT wi.id, a.id, $2, '{"en":"Gate","pt":"Portão"}', '{"en":"","pt":""}', 'resolved', 'done', $3,
			       now() - $4::interval
			FROM work.work_items wi JOIN work.attempts a ON a.work_item_id = wi.id WHERE wi.id = $1`,
			itemID, kind, responder, resolvedAgo)
		require.NoError(t, err)
	}
	answeredBy := func() []any {
		t.Helper()
		var out []any
		items := h.list("/v1/work-items", admin.headers())
		require.Len(t, items, 1)
		detail := h.do(http.MethodGet, "/v1/work-items/"+itemID, nil, admin.headers())
		require.Equal(t, http.StatusOK, detail.status)
		for _, item := range []map[string]any{items[0], detail.body} {
			out = append(out, item["answeredBy"])
		}
		return out
	}

	assert.Equal(t, []any{nil, nil}, answeredBy(), "nothing answered yet")
	resolve("information", operator.ID, "3 hours")
	assert.Equal(t, []any{nil, nil}, answeredBy(), "an information gate is not an answer")

	resolve("approval", operator.ID, "2 hours")
	resolve("information", admin.user.ID, "1 hour")
	want := map[string]any{"displayName": "Rita Lopes"}
	assert.Equal(t, []any{want, want}, answeredBy(), "the latest approval wins over a later information gate")

	_, err = h.pool.Exec(ctx, `UPDATE identity.local_users SET display_name='Admin Person' WHERE identity_id=$1`, admin.user.ID)
	require.NoError(t, err)
	resolve("decision", admin.user.ID, "30 minutes")
	want = map[string]any{"displayName": "Admin Person"}
	assert.Equal(t, []any{want, want}, answeredBy(), "a later decision is the most recent answer")

	_, err = h.pool.Exec(ctx, `UPDATE identity.local_users SET display_name='admin@example.com' WHERE identity_id=$1`, admin.user.ID)
	require.NoError(t, err)
	assert.Equal(t, []any{nil, nil}, answeredBy(), "an email-like name is suppressed")
	raw, err := json.Marshal(h.list("/v1/work-items", operatorSession.headers()))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "admin@example.com")
}
