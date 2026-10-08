package e2e_test

import (
	"net/http"
	"testing"
	"time"
)

type delegatedTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func exerciseIdentityAPIStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.captureBootstrapKeys(t)

	status, jwksBody := state.request(t, http.MethodGet, "/.well-known/jwks.json", "", nil, nil)
	requireStatus(t, status, http.StatusOK, jwksBody)
	var keys jwkSet
	mustDecode(t, jwksBody, &keys)
	if len(keys.Keys) == 0 {
		t.Fatal("deployed JWKS has no signing key")
	}

	// API key scopes (401/403 on /v1/token, /v1/users and /v1/work-items) are
	// proven by the F1 server TestAPI; this stage keeps only what needs the real
	// operator and process: the IdentityMapping path, JWKS across restart, and
	// revocation when the mapping is deleted (C5).
	tokenRequest := map[string]any{"provider": "teams", "type": "user", "externalID": "aad:deployed-alice"}

	identity := `apiVersion: platform.iterabase.com/v1alpha1
kind: IdentityMapping
metadata:
  name: deployed-alice
  namespace: iterabase-system
spec:
  identity:
    kind: user
    displayName: Alice Deployed
  bindings:
    - provider: teams
      type: user
      externalID: aad:deployed-alice
`
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "identity.yaml", identity))
	state.kubectl(t, 2*time.Minute, "wait", "--for=jsonpath={.status.ready}=true", "identitymapping/deployed-alice",
		"-n", controlPlaneNamespace, "--timeout=90s")
	identityID := state.kubectl(t, 30*time.Second, "get", "identitymapping/deployed-alice", "-n", controlPlaneNamespace,
		"-o", "jsonpath={.status.identityID}")
	if identityID == "" {
		t.Fatal("Ready IdentityMapping has no materialized identity ID")
	}

	status, body := state.requestJSON(t, http.MethodPost, "/v1/token", state.tokenKey, tokenRequest)
	requireStatus(t, status, http.StatusOK, body)
	var delegated delegatedTokenResponse
	mustDecode(t, body, &delegated)
	if delegated.AccessToken == "" || delegated.TokenType != "Bearer" || delegated.ExpiresIn <= 0 {
		t.Fatalf("invalid delegated-token response: token=%v type=%q expires=%d", delegated.AccessToken != "", delegated.TokenType, delegated.ExpiresIn)
	}
	state.redactor.Add(delegated.AccessToken)
	claims, err := verifyRS256(delegated.AccessToken, keys)
	if err != nil {
		t.Fatalf("verify delegated RS256 token: %v", err)
	}
	if subject, _ := claims["sub"].(string); subject != identityID {
		t.Fatalf("delegated token subject=%q want materialized identity=%q", subject, identityID)
	}

	oldAdminKey, oldTokenKey := state.adminKey, state.tokenKey
	state.restartAPI(t)
	status, body = state.request(t, http.MethodGet, "/v1/users", oldAdminKey, nil, nil)
	requireStatus(t, status, http.StatusOK, body)
	status, body = state.requestJSON(t, http.MethodPost, "/v1/token", oldTokenKey, tokenRequest)
	requireStatus(t, status, http.StatusOK, body)
	var restartedDelegated delegatedTokenResponse
	mustDecode(t, body, &restartedDelegated)
	if restartedDelegated.AccessToken == "" || restartedDelegated.TokenType != "Bearer" || restartedDelegated.ExpiresIn <= 0 {
		t.Fatalf("invalid restarted delegated-token response: token=%v type=%q expires=%d", restartedDelegated.AccessToken != "", restartedDelegated.TokenType, restartedDelegated.ExpiresIn)
	}
	state.redactor.Add(restartedDelegated.AccessToken)
	status, restartedJWKS := state.request(t, http.MethodGet, "/.well-known/jwks.json", "", nil, nil)
	requireStatus(t, status, http.StatusOK, restartedJWKS)
	if string(restartedJWKS) != string(jwksBody) {
		t.Fatal("JWKS changed across API process restart")
	}
	var restartedKeys jwkSet
	mustDecode(t, restartedJWKS, &restartedKeys)
	restartedClaims, err := verifyRS256(restartedDelegated.AccessToken, restartedKeys)
	if err != nil {
		t.Fatalf("verify restarted delegated RS256 token: %v", err)
	}
	if subject, _ := restartedClaims["sub"].(string); subject != identityID {
		t.Fatalf("restarted delegated token subject=%q want materialized identity=%q", subject, identityID)
	}

	state.kubectl(t, 30*time.Second, "delete", "identitymapping/deployed-alice", "-n", controlPlaneNamespace)
	status, body = state.requestJSON(t, http.MethodPost, "/v1/token", oldTokenKey, tokenRequest)
	requireStatus(t, status, http.StatusForbidden, body)
}
