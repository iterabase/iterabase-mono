package e2e_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
)

// The deployed V2 authority fixture (HOR-582). Sign-in is always on, so every
// deployed scenario installs a verified-TLS mail relay (the runtime fixture's
// SMTP sink), lets the overlay-named first Admin complete the emailed setup
// link, onboards an Operator through the real access-request flow, and seeds
// V2 personal credentials for API automation (credential lifecycle is HOR-514).
const (
	authPublicOrigin  = "https://app.e2e.iterabase.test"
	authAdminEmail    = "admin@e2e.iterabase.test"
	authOperatorEmail = "operator@e2e.iterabase.test"
	mailSinkName      = "e2e-smtp-sink"
	mailSinkCASecret  = "e2e-smtp-ca"
	mailSinkTLSSecret = "e2e-smtp-sink-tls"
)

// webSession is one signed-in browser session driven over the verified API
// forward: the opaque session cookie plus its session-bound CSRF proof.
type webSession struct {
	email      string
	password   string
	identityID string
	cookie     string
	csrf       string
}

// headers returns the browser-equivalent headers for a cookie request. Unsafe
// methods carry the CSRF proof; every request carries the configured origin
// the edge would present.
func (session webSession) headers(extra map[string]string) map[string]string {
	headers := map[string]string{"Cookie": session.cookie, "Origin": authPublicOrigin, "X-CSRF-Token": session.csrf}
	for key, value := range extra {
		headers[key] = value
	}
	return headers
}

// authValues configures the always-on authentication prerequisites against the
// in-cluster mail sink, trusted through its private CA.
func authValues() map[string]any {
	return map[string]any{
		"publicOrigin": authPublicOrigin,
		"email": map[string]any{
			"host": mailSinkName + "." + controlPlaneNamespace + ".svc", "port": 2525, "mode": "starttls",
			"from": "Iterabase <no-reply@e2e.iterabase.test>", "caSecret": mailSinkCASecret,
		},
		"bootstrap": map[string]any{"adminEmail": authAdminEmail, "adminLocale": "en"},
	}
}

// installMailSinkStage deploys the verified-TLS SMTP sink before the platform
// so the first-boot setup email has a relay to reach.
func installMailSinkStage(t *testing.T, state *deployedState) {
	t.Helper()
	if state.runtimeImage.repository == "" {
		t.Fatal("the deployed authority journey requires the runtime fixture image (SMTP sink)")
	}
	caPEM, certPEM, keyPEM := mailSinkCertificates(t, mailSinkName+"."+controlPlaneNamespace+".svc")
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: Secret
metadata:
  name: %[2]s
  namespace: %[1]s
type: Opaque
data:
  ca.crt: %[3]s
---
apiVersion: v1
kind: Secret
metadata:
  name: %[4]s
  namespace: %[1]s
type: kubernetes.io/tls
data:
  tls.crt: %[5]s
  tls.key: %[6]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[7]s
  namespace: %[1]s
spec:
  replicas: 1
  selector: {matchLabels: {app: %[7]s}}
  template:
    metadata: {labels: {app: %[7]s}}
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532}
      containers:
        - name: sink
          image: %[8]s
          imagePullPolicy: Never
          env: [{name: E2E_RUNTIME_MODE, value: smtp-sink}]
          ports: [{containerPort: 2525, name: smtp}, {containerPort: 8080, name: http}]
          readinessProbe: {httpGet: {path: /health, port: http}}
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          volumeMounts: [{name: tls, mountPath: /tls, readOnly: true}]
      volumes: [{name: tls, secret: {secretName: %[4]s}}]
---
apiVersion: v1
kind: Service
metadata:
  name: %[7]s
  namespace: %[1]s
spec:
  selector: {app: %[7]s}
  ports: [{name: smtp, port: 2525, targetPort: smtp}, {name: http, port: 8080, targetPort: http}]
`, controlPlaneNamespace, mailSinkCASecret, b64(caPEM), mailSinkTLSSecret, b64(certPEM), b64(keyPEM),
		mailSinkName, state.runtimeImage.reference())
	path := state.writeManifest(t, "mail-sink.yaml", manifest)
	state.kubectl(t, 60*time.Second, "apply", "-f", path)
	state.kubectl(t, 4*time.Minute, "rollout", "status", "deployment/"+mailSinkName, "-n", controlPlaneNamespace, "--timeout=3m")
}

// mailSinkCertificates issues a private CA and a sink leaf for the exact
// in-cluster service name, so the control plane verifies the relay.
func mailSinkCertificates(t *testing.T, serverName string) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "e2e mail sink CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: serverName}, DNSNames: []string{serverName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

type sinkMessage struct {
	To   []string `json:"to"`
	Data string   `json:"data"`
}

var authLinkPattern = regexp.MustCompile(`https://[^\s"<>]+/auth/(verify|setup|reset)\?token=([A-Za-z0-9_\-]+)`)

// awaitAuthLink waits for the newest unused one-time link of the given kind
// sent to the recipient, and registers the token with the redactor before it
// can reach any evidence.
func (state *deployedState) awaitAuthLink(t *testing.T, recipient, kind string) string {
	t.Helper()
	forward, err := state.client.PortForward(state.ctx, controlPlaneNamespace, "svc/"+mailSinkName, 8080, "http")
	if err != nil {
		t.Fatalf("port-forward mail sink: %v", err)
	}
	defer func() { _ = forward.Stop() }()
	client, err := httpx.Client(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		token, err := state.newestLink(client, forward.URL, recipient, kind)
		if err != nil {
			t.Fatalf("observe mail sink for %s email to %s: %v", kind, recipient, err)
		}
		if token != "" {
			state.redactor.Add(token)
			state.mu.Lock()
			state.usedLinks[token] = true
			state.mu.Unlock()
			return token
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no %s email reached %s within the delivery bound", kind, recipient)
	return ""
}

// newestLink observes the mail sink once. Only a valid mailbox response with no
// matching link returns ("", nil) and keeps polling; a transport, status, or
// decoding failure, or a link outside the public origin, is an observation
// error that fails immediately (docs/testing/STRATEGY.md).
func (state *deployedState) newestLink(client *http.Client, base, recipient, kind string) (string, error) {
	req, err := http.NewRequestWithContext(state.ctx, http.MethodGet, base+"/messages", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("read mailbox: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read mailbox: status %d", response.StatusCode)
	}
	var messages []sinkMessage
	if err := json.NewDecoder(response.Body).Decode(&messages); err != nil {
		return "", fmt.Errorf("decode mailbox: %w", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if len(message.To) == 0 || !strings.EqualFold(message.To[0], recipient) {
			continue
		}
		for _, match := range authLinkPattern.FindAllStringSubmatch(message.Data, -1) {
			if match[1] == kind && !state.usedLinks[match[2]] {
				if !strings.HasPrefix(match[0], authPublicOrigin+"/auth/") {
					return "", fmt.Errorf("%s link is outside the public origin %s", kind, authPublicOrigin)
				}
				return match[2], nil
			}
		}
	}
	return "", nil
}

// signIn creates a browser session through the public sign-in endpoint.
func (state *deployedState) signIn(t *testing.T, email, password string) webSession {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(state.ctx, http.MethodPost, state.apiForward.URL+"/v1/auth/sign-in", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", authPublicOrigin)
	resp, err := state.apiClient.Do(req)
	if err != nil {
		t.Fatalf("sign in %s: %v", email, err)
	}
	defer resp.Body.Close()
	var payload struct {
		Profile   struct{ ID string } `json:"profile"`
		CSRFToken string              `json:"csrfToken"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&payload) != nil || payload.CSRFToken == "" {
		t.Fatalf("sign in %s: HTTP %d", email, resp.StatusCode)
	}
	session := webSession{email: email, password: password, identityID: payload.Profile.ID, csrf: payload.CSRFToken}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__Host-iterabase_session" {
			if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" {
				t.Fatalf("session cookie violates the __Host- contract: %+v", cookie)
			}
			session.cookie = cookie.Name + "=" + cookie.Value
			state.redactor.Add(cookie.Value)
		}
	}
	if session.cookie == "" {
		t.Fatal("sign-in did not set the browser session cookie")
	}
	state.redactor.Add(session.csrf)
	state.recordRequest(t, requestEvidence{Method: http.MethodPost, Path: "/v1/auth/sign-in", Status: resp.StatusCode})
	return session
}

// sessionJSON issues a cookie-authenticated JSON request.
func (state *deployedState) sessionJSON(t *testing.T, session webSession, method, path string, value any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return state.request(t, method, path, "", body, session.headers(map[string]string{"Content-Type": "application/json"}))
}

// publicJSON issues an unauthenticated public-journey request from the edge
// origin.
func (state *deployedState) publicJSON(t *testing.T, method, path string, value any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return state.request(t, method, path, "", body, map[string]string{"Content-Type": "application/json", "Origin": authPublicOrigin})
}

// completeSetup finishes first-time setup from an emailed link.
func (state *deployedState) completeSetup(t *testing.T, email, password string) {
	t.Helper()
	state.redactor.Add(password)
	token := state.awaitAuthLink(t, email, "setup")
	status, body := state.publicJSON(t, http.MethodPost, "/v1/auth/setup", map[string]string{
		"token": token, "displayName": strings.SplitN(email, "@", 2)[0], "locale": "en", "password": password,
	})
	requireStatus(t, status, http.StatusOK, body)
}

// ensurePrincipals proves the first-Admin and access-request journeys once and
// leaves the scenario with an Admin and an Operator session plus V2 personal
// credentials: the Operator's work key (all non-Admin work/artifact actions)
// and the Admin's key (artifact deletion, People and value reads).
func (state *deployedState) ensurePrincipals(t *testing.T) {
	t.Helper()
	if state.admin.cookie != "" {
		return
	}
	// First Admin: the overlay-named email received a one-time setup link on
	// first boot; setup creates no session, sign-in does.
	state.completeSetup(t, authAdminEmail, "e2e-admin-long-password")
	state.admin = state.signIn(t, authAdminEmail, "e2e-admin-long-password")

	state.operator = state.onboard(t, authOperatorEmail, "operator", "e2e-operator-long-password")

	state.workIdentityID = state.operator.identityID
	state.workKey = state.seedPersonalCredential(t, state.operator.identityID,
		"workflows.read", "workflows.start", "work.read", "work.feedback.write", "artifacts.read", "artifacts.upload")
	state.adminKey = state.seedPersonalCredential(t, state.admin.identityID,
		"work.read", "value.read", "people.read", "artifacts.read", "artifacts.delete")
}

// onboard runs the access-request flow for a new person: request, verify the
// emailed link, Admin approval with recent password proof, emailed setup, and
// sign-in.
func (state *deployedState) onboard(t *testing.T, email, role, password string) webSession {
	t.Helper()
	status, body := state.publicJSON(t, http.MethodPost, "/v1/auth/request-access", map[string]string{"email": email, "locale": "en"})
	requireStatus(t, status, http.StatusAccepted, body)
	status, body = state.publicJSON(t, http.MethodPost, "/v1/auth/verify", map[string]string{"token": state.awaitAuthLink(t, email, "verify")})
	requireStatus(t, status, http.StatusOK, body)

	status, body = state.sessionJSON(t, state.admin, http.MethodPost, "/v1/auth/reauthenticate", map[string]string{"password": state.admin.password})
	requireStatus(t, status, http.StatusOK, body)
	status, body = state.request(t, http.MethodGet, "/v1/access-requests", "", nil, state.admin.headers(nil))
	requireStatus(t, status, http.StatusOK, body)
	var pending []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	mustDecode(t, body, &pending)
	requestID := ""
	for _, request := range pending {
		if strings.EqualFold(request.Email, email) {
			requestID = request.ID
		}
	}
	if requestID == "" {
		t.Fatalf("verified access request for %s is not pending Admin review", email)
	}
	status, body = state.sessionJSON(t, state.admin, http.MethodPost, "/v1/access-requests/"+requestID+"/approve", map[string]string{"role": role})
	requireStatus(t, status, http.StatusOK, body)

	state.completeSetup(t, email, password)
	return state.signIn(t, email, password)
}

// seedPersonalCredential issues a V2 personal credential for an active human
// directly in the credential store, exactly as the HOR-514 lifecycle will:
// hash-only persistence, immutable actions, mandatory rates and expiry. The
// raw secret is registered with the redactor before it is used.
func (state *deployedState) seedPersonalCredential(t *testing.T, ownerID string, actions ...string) string {
	t.Helper()
	random := make([]byte, 20)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	full := "cp-" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random)
	sum := sha256.Sum256([]byte(full))
	state.redactor.Add(full)
	if !fixtureIdentityPattern.MatchString(ownerID) {
		t.Fatalf("invalid fixture owner %q", ownerID)
	}
	for _, action := range actions {
		if !fixtureActionPattern.MatchString(action) {
			t.Fatalf("invalid fixture action %q", action)
		}
	}
	state.databaseQuery(t, fmt.Sprintf(`
		INSERT INTO identity.api_keys
			(identity_id, key_hash, prefix, name, key_type, owner_user_identity_id, actor_identity_id,
			 actions, rate_rpm, rate_tpm, expires_at, credential_family_id, created_by_identity_id, status)
		VALUES ('%[1]s', '%[2]s', '%[3]s', 'deployed-e2e', 'personal', '%[1]s', '%[1]s',
			ARRAY['%[4]s']::text[], 600, 600000, now() + interval '1 day', gen_random_uuid(), '%[1]s', 'active')
		RETURNING id`, ownerID, hex.EncodeToString(sum[:]), full[:12], strings.Join(actions, "','")))
	return full
}

var (
	fixtureIdentityPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)
	fixtureActionPattern   = regexp.MustCompile(`^[a-z]+(\.[a-z]+)+$`)
)

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// browserSecrets are every fixture credential the browser journey can expose:
// API keys, passwords, and session cookies.
func (state *deployedState) browserSecrets() []string {
	var secrets []string
	for _, value := range []string{
		state.adminKey, state.workKey, state.admin.password, state.operator.password,
		strings.TrimPrefix(state.admin.cookie, "__Host-iterabase_session="),
		strings.TrimPrefix(state.operator.cookie, "__Host-iterabase_session="),
	} {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}
