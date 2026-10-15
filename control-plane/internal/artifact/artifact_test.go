package artifact_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/artifacttest"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanner"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/spiffe/testca"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/testutil"
)

const (
	coordinatorID = "spiffe://iterabase.local/control-plane/artifact-coordinator"
	scannerID     = "spiffe://iterabase.local/control-plane/artifact-scanner"
)

// fakeEngine stands in for clamd only: it reports the test marker (and the
// EICAR self-test sample) as malware and can simulate an outage. Like clamd's
// ScanMail, it also finds the marker inside encoded MIME attachments, so an
// email carrying a marked attachment is itself flagged.
type fakeEngine struct {
	mu        sync.Mutex
	down      bool
	infectAll bool
}

func (e *fakeEngine) setDown(v bool) { e.mu.Lock(); e.down = v; e.mu.Unlock() }
func (e *fakeEngine) isDown() bool   { e.mu.Lock(); defer e.mu.Unlock(); return e.down }

func (e *fakeEngine) Ping(context.Context) error              { return nil }
func (e *fakeEngine) Version(context.Context) (string, error) { return "ClamAV 1.4.6/test", nil }
func (e *fakeEngine) Reload(context.Context) error            { return nil }
func (e *fakeEngine) Scan(_ context.Context, data []byte) (bool, string, error) {
	if e.isDown() && !bytes.Contains(data, []byte("EICAR")) {
		return false, "", errors.New("engine unavailable")
	}
	e.mu.Lock()
	infectAll := e.infectAll
	e.mu.Unlock()
	if infectAll || containsMarker(data) {
		return true, "Iterabase.Test.Marker", nil
	}
	if bytes.Contains(data, []byte("EICAR-STANDARD-ANTIVIRUS-TEST-FILE")) {
		return true, "Eicar-Test-Signature", nil
	}
	return false, "", nil
}

// harness wires the real service, coordinator, mTLS scanner client, scanner
// adapter, and admission policy to Postgres and to MinIO with the shipped
// zone policies. Only the malware engine is fake.
type harness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	minio  testutil.MinIOConfig
	svc    *artifactstore.Service
	coord  *artifactstore.Coordinator
	store  *artifactstore.Store
	engine *fakeEngine
	// scanEngine is the engine behind the real adapter: engine by default,
	// or real clamd.
	scanEngine scanner.Engine
	actorID    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	engine := &fakeEngine{}
	h := newHarnessWithEngine(t, engine)
	h.engine = engine
	return h
}

func newHarnessWithEngine(t *testing.T, engine scanner.Engine) *harness {
	t.Helper()
	pool := testutil.NewPostgresPool(t)
	m := testutil.NewMinIO(t)
	ctx := context.Background()
	h := &harness{t: t, pool: pool, minio: m, store: artifactstore.NewStore(pool), scanEngine: engine}
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO identity.identities (key,kind,source,display_name)
		VALUES ('artifact-user','user','local','Artifact User') RETURNING id::text`).Scan(&h.actorID))

	zone := func(c testutil.MinIOCredential) artifactstore.LifecycleStore {
		s, err := artifactstore.NewMinIOStore(artifactstore.MinIOConfig{Endpoint: m.Endpoint, AccessKey: c.AccessKey,
			SecretKey: c.SecretKey, Bucket: m.Bucket})
		require.NoError(t, err)
		return s
	}
	h.svc = artifactstore.NewService(h.store, zone(m.Ingest), zone(m.Serve), artifactstore.Config{}, nil)
	require.NoError(t, h.svc.Ready(ctx))

	scan := h.startScanner()
	h.coord = artifactstore.NewCoordinator(h.store, zone(m.Lifecycle), scan, artifactstore.CoordinatorConfig{
		Owner: "test", LeaseTTL: time.Minute, ScanTimeout: 20 * time.Second}, nil, nil)
	return h
}

// startScanner serves the real adapter over HTTP/2 mTLS and returns the real
// coordinator-side client bound to the exact scanner identity.
func (h *harness) startScanner() artifactstore.Scanner {
	t := h.t
	ca, err := testca.New()
	require.NoError(t, err)
	serverCert, err := ca.Leaf(testca.LeafOpts{SPIFFEID: scannerID, DNSNames: []string{"localhost"}, IsServer: true})
	require.NoError(t, err)
	clientCert, err := ca.Leaf(testca.LeafOpts{SPIFFEID: coordinatorID})
	require.NoError(t, err)

	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	writeState(t, state, time.Now())
	adapter := &scanner.Adapter{Engine: h.scanEngine, StatePath: state, AllowedClient: coordinatorID}
	adapter.Refresh(context.Background())
	ready, cause := adapter.Ready()
	require.True(t, ready, cause)

	srv := httptest.NewUnstartedServer(adapter)
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs: ca.Pool, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	caFile, certFile, keyFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(caFile, ca.CertPEM, 0o600))
	writeKeyPair(t, clientCert, certFile, keyFile)
	client, err := artifactstore.NewMTLSScanner(artifactstore.MTLSScannerConfig{URL: "https://localhost:" + port,
		ServerSPIFFEID: scannerID, CAFile: caFile, ClientCertFile: certFile, ClientKeyFile: keyFile})
	require.NoError(t, err)
	return client
}

func writeState(t *testing.T, path string, checked time.Time) {
	t.Helper()
	b, err := json.Marshal(scanner.SignatureState{Version: "test-1", CheckedAt: checked})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
}

func writeKeyPair(t *testing.T, cert tls.Certificate, certFile, keyFile string) {
	t.Helper()
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
}

func (h *harness) admit(name, mimeType string, data []byte, opts ...func(*artifactstore.AdmitInput)) artifactstore.Artifact {
	h.t.Helper()
	in := artifactstore.AdmitInput{SourceType: artifactstore.SourceUpload, DisplayName: name, ClaimedMIME: mimeType,
		Attribution:    artifactstore.Attribution{InitiatingHumanID: h.actorID, RequestActorID: h.actorID, Source: artifactstore.AuthBrowser},
		ContainerDepth: 1}
	for _, o := range opts {
		o(&in)
	}
	a, err := h.svc.Admit(context.Background(), in, bytes.NewReader(data))
	require.NoError(h.t, err)
	return a
}

func (h *harness) drain() {
	h.t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		worked, err := h.coord.ProcessOne(ctx)
		require.NoError(h.t, err)
		if !worked {
			h.coord.Sweep(ctx)
			if worked, err = h.coord.ProcessOne(ctx); err != nil || !worked {
				require.NoError(h.t, err)
				return
			}
		}
	}
	h.t.Fatal("coordinator did not settle")
}

func (h *harness) get(id string) artifactstore.Artifact {
	h.t.Helper()
	a, err := h.store.Get(context.Background(), id)
	require.NoError(h.t, err)
	return a
}

func (h *harness) listed(id string) bool {
	h.t.Helper()
	items, err := h.svc.List(context.Background(), artifactstore.ListQuery{Limit: 100})
	require.NoError(h.t, err)
	for _, it := range items {
		if it.Artifact.ID == id {
			return true
		}
	}
	return false
}

func (h *harness) rootClient() *minio.Client {
	c, err := minio.New(h.minio.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(h.minio.Root.AccessKey, h.minio.Root.SecretKey, "")})
	require.NoError(h.t, err)
	return c
}

func (h *harness) objectCount(prefix string) int {
	n := 0
	for obj := range h.rootClient().ListObjects(context.Background(), h.minio.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		require.NoError(h.t, obj.Err)
		n++
	}
	return n
}

// SCN-005: bytes are unusable until clearance, then shared, searchable,
// downloadable, and the quarantine copy is purged.
func TestSafeArtifactIsSharedOnlyAfterClearance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.admit("Quarterly Report.pdf", "application/pdf", admissiontest.PDF())
	assert.Equal(t, artifactstore.StatusCheckingSecurity, a.Status())
	assert.False(t, h.listed(a.ID), "quarantined artifacts are not in the shared library")
	_, _, err := h.svc.Open(ctx, a.ID)
	assert.ErrorIs(t, err, artifactstore.ErrNotAvailable, "quarantined bytes cannot be fetched")
	_, err = h.svc.Admit(ctx, artifactstore.AdmitInput{}, nil)
	assert.Error(t, err)

	h.drain()
	cleared := h.get(a.ID)
	assert.True(t, cleared.Cleared())
	assert.Equal(t, "application/pdf", *cleared.CanonicalMIME)
	assert.Equal(t, artifactstore.ExtractionPending, cleared.ExtractionState, "clearance makes extraction eligible (HOR-462)")
	assert.True(t, h.listed(a.ID))
	found, err := h.svc.List(ctx, artifactstore.ListQuery{Search: "quarterly", Limit: 10})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, a.ID, found[0].Artifact.ID)
	assert.Equal(t, "Artifact User", found[0].Uploader)

	_, r, err := h.svc.Open(ctx, a.ID)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	_ = r.Close()
	assert.Equal(t, admissiontest.PDF(), got)
	assert.Zero(t, h.objectCount("quarantine/"), "the quarantine copy is purged after promotion")
	assert.Equal(t, 1, h.objectCount("originals/"))
}

// The zone credentials are enforced by MinIO itself (DES-HOR-460-02).
func TestZoneCredentialsAreLeastPrivilege(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.admit("notes.txt", "text/plain", admissiontest.TXT())
	var key string
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT storage_key FROM artifact.objects WHERE artifact_id=$1`, a.ID).Scan(&key))

	client := func(c testutil.MinIOCredential) *minio.Client {
		cl, err := minio.New(h.minio.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(c.AccessKey, c.SecretKey, "")})
		require.NoError(t, err)
		return cl
	}
	denied := func(err error) {
		require.Error(t, err)
		assert.Equal(t, "AccessDenied", minio.ToErrorResponse(err).Code)
	}
	for name, c := range map[string]testutil.MinIOCredential{"ingest": h.minio.Ingest, "serve": h.minio.Serve} {
		t.Run(name, func(t *testing.T) {
			_, err := client(c).StatObject(ctx, h.minio.Bucket, key, minio.StatObjectOptions{})
			denied(err)
			denied(client(c).RemoveObject(ctx, h.minio.Bucket, key, minio.RemoveObjectOptions{}))
			for obj := range client(c).ListObjects(ctx, h.minio.Bucket, minio.ListObjectsOptions{Recursive: true}) {
				denied(obj.Err)
				break
			}
		})
	}
	_, err := client(h.minio.Serve).PutObject(ctx, h.minio.Bucket, "originals/forged", strings.NewReader("x"), 1, minio.PutObjectOptions{})
	denied(err)
	_, err = client(h.minio.Ingest).PutObject(ctx, h.minio.Bucket, "originals/forged", strings.NewReader("x"), 1, minio.PutObjectOptions{})
	denied(err)
	ingest, err := artifactstore.NewMinIOStore(artifactstore.MinIOConfig{Endpoint: h.minio.Endpoint,
		AccessKey: h.minio.Ingest.AccessKey, SecretKey: h.minio.Ingest.SecretKey, Bucket: h.minio.Bucket})
	require.NoError(t, err)
	assert.ErrorIs(t, ingest.PutNew(ctx, key, strings.NewReader("overwrite"), 9, "text/plain"), artifactstore.ErrObjectExists,
		"quarantine objects are create-only")
	_, err = client(h.minio.Lifecycle).StatObject(ctx, h.minio.Bucket, key, minio.StatObjectOptions{})
	assert.NoError(t, err, "only the lifecycle credential reads quarantine")
}

// SCN-006 (malware): permanent rejection, immediate purge, minimal tombstone.
func TestMalwareIsRejectedPurgedAndNeverUsable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.admit("invoice.pdf", "application/pdf", admissiontest.WithMarker(admissiontest.PDF()))
	h.drain()
	got := h.get(a.ID)
	assert.Equal(t, artifactstore.SecurityRejected, got.SecurityState)
	assert.Equal(t, "malware", *got.SecurityReason)
	assert.Equal(t, artifactstore.StorageDeleted, got.StorageState)
	assert.Equal(t, artifactstore.StatusRejected, got.Status())
	assert.Nil(t, got.DisplayName, "the tombstone keeps no filename")
	assert.NotNil(t, got.Digest, "the tombstone keeps the digest")
	assert.Zero(t, h.objectCount(""), "rejected bytes are purged")
	assert.False(t, h.listed(a.ID))
	_, _, err := h.svc.Open(ctx, a.ID)
	assert.ErrorIs(t, err, artifactstore.ErrNotAvailable)
	_, err = h.svc.RetrySecurity(ctx, a.ID, h.actorID, "")
	assert.ErrorIs(t, err, artifactstore.ErrNotRetryable, "unchanged malware bytes cannot be retried")
}

// The admission corpus is rejected or cleared through the full pipeline.
func TestCorpusThroughThePipeline(t *testing.T) {
	h := newHarness(t)
	ids := map[string]admissiontest.Case{}
	for _, c := range admissiontest.Corpus() {
		if c.Depth > 1 {
			continue
		}
		ids[h.admit(c.Filename, c.ClaimedMIME, c.Data).ID] = c
	}
	h.drain()
	for id, c := range ids {
		got := h.get(id)
		if c.Accepted {
			assert.True(t, got.Cleared(), c.Name)
			assert.Equal(t, c.CanonicalMIME, *got.CanonicalMIME, c.Name)
			children, err := h.store.Children(context.Background(), id)
			require.NoError(t, err)
			assert.Len(t, children, c.Children, c.Name)
		} else {
			assert.Equal(t, artifactstore.SecurityRejected, got.SecurityState, c.Name)
			assert.Equal(t, c.Reason, *got.SecurityReason, c.Name)
			assert.Equal(t, artifactstore.StorageDeleted, got.StorageState, c.Name)
		}
	}
}

// Ticket acceptance: a received message with a PDF and an attached .msg
// holding a further PDF yields body, PDF, .msg container, its body, and its
// inner PDF, each with lineage; a malicious sibling is rejected alone. The
// engine flags the message itself (clamd ScanMail), so the container bytes
// are rejected and never cleared while every child is still judged alone.
func TestEmailContainersExpandWithLineage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inner := admissiontest.MSG("Supplier quote", "Please find the quote attached.",
		admissiontest.Attachment{Filename: "quote.pdf", ContentType: "application/pdf", Data: admissiontest.PDF()})
	message := admissiontest.EML("Order 42", "Body of the received email.", "",
		admissiontest.Attachment{Filename: "order.pdf", ContentType: "application/pdf", Data: admissiontest.PDF()},
		admissiontest.Attachment{Filename: "forwarded.msg", ContentType: "application/vnd.ms-outlook", Data: inner},
		admissiontest.Attachment{Filename: "virus.txt", ContentType: "text/plain", Data: admissiontest.WithMarker(admissiontest.TXT())},
	)
	root := h.admit("message.eml", "message/rfc822", message, func(in *artifactstore.AdmitInput) {
		in.SourceType, in.SourceRef, in.ContainerDepth = artifactstore.SourceMailbox, "mailbox/inbox/message-42", 0
		in.Attribution = artifactstore.Attribution{RequestActorID: h.actorID, Source: artifactstore.AuthSystem}
	})
	h.drain()
	rootState := h.get(root.ID)
	require.Equal(t, artifactstore.SecurityRejected, rootState.SecurityState, "the infected message bytes are rejected")
	require.False(t, rootState.Cleared())
	children, err := h.store.Children(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, children, 4)
	byName := map[string]artifactstore.Artifact{}
	for _, c := range children {
		byName[deref(c.DisplayName)] = c
		assert.Equal(t, root.ID, *c.ParentID)
		assert.Equal(t, artifactstore.SourceMailbox, c.SourceType, "children inherit the source and its lineage")
		assert.Equal(t, "mailbox/inbox/message-42", *c.SourceRef)
	}
	assert.Equal(t, artifactstore.ChildBody, *byName["message-body.txt"].ChildKind)
	assert.True(t, byName["message-body.txt"].Cleared())
	assert.True(t, byName["order.pdf"].Cleared())
	virus := h.get(children[3].ID)
	assert.Equal(t, artifactstore.SecurityRejected, virus.SecurityState, "the malicious sibling is rejected")
	msg := byName["forwarded.msg"]
	require.True(t, msg.Cleared(), "siblings are unaffected by the rejected file")
	grand, err := h.store.Children(ctx, msg.ID)
	require.NoError(t, err)
	require.Len(t, grand, 2, "the attached .msg expands into its body and PDF")
	for _, g := range grand {
		assert.True(t, g.Cleared())
		assert.Equal(t, 2, g.ContainerDepth)
	}

	// An uploaded .eml is depth 1: an attached message inside it stays an
	// unexpanded container (DES-HOR-461-03).
	uploaded := h.admit("forward.eml", "message/rfc822", admissiontest.EML("Fwd", "See attached.", "",
		admissiontest.Attachment{Filename: "forwarded.msg", ContentType: "application/vnd.ms-outlook", Data: inner}))
	h.drain()
	kids, err := h.store.Children(ctx, uploaded.ID)
	require.NoError(t, err)
	require.Len(t, kids, 2)
	for _, k := range kids {
		sub, err := h.store.Children(ctx, k.ID)
		require.NoError(t, err)
		assert.Empty(t, sub)
	}
}

// SCN-006 (transient): a scanner outage never claims a verdict; three
// attempts on the one-minute/five-minute schedule, then a browser retry
// creates exactly one new trigger.
func TestScannerOutageIsRetryableAndFailsClosed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.engine.setDown(true)
	a := h.admit("data.csv", "text/csv", admissiontest.CSV())
	fastForward := func() {
		_, err := h.pool.Exec(ctx, `UPDATE artifact.security_attempts SET not_before=now() WHERE state='queued'`)
		require.NoError(t, err)
	}
	var delays []time.Duration
	for attempt := 1; attempt <= 3; attempt++ {
		worked, err := h.coord.ProcessOne(ctx)
		require.NoError(t, err)
		require.True(t, worked)
		var notBefore, created time.Time
		err = h.pool.QueryRow(ctx, `SELECT not_before, created_at FROM artifact.security_attempts
			WHERE artifact_id=$1 AND state='queued'`, a.ID).Scan(&notBefore, &created)
		if attempt < 3 {
			require.NoError(t, err)
			delays = append(delays, notBefore.Sub(created).Round(time.Minute))
			assert.Equal(t, artifactstore.StatusCheckingSecurity, h.get(a.ID).Status())
			fastForward()
		}
	}
	assert.Equal(t, []time.Duration{time.Minute, 5 * time.Minute}, delays)
	got := h.get(a.ID)
	assert.Equal(t, artifactstore.SecurityRetryableFailed, got.SecurityState)
	assert.Equal(t, artifactstore.StatusSecurityCheckFailed, got.Status())
	assert.False(t, h.listed(a.ID))
	worked, err := h.coord.ProcessOne(ctx)
	require.NoError(t, err)
	assert.False(t, worked, "there is no automatic fourth attempt")

	h.engine.setDown(false)
	_, err = h.svc.RetrySecurity(ctx, a.ID, h.actorID, "")
	require.NoError(t, err)
	h.drain()
	assert.True(t, h.get(a.ID).Cleared())
	var triggers int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(DISTINCT trigger_id) FROM artifact.security_attempts WHERE artifact_id=$1`, a.ID).Scan(&triggers))
	assert.Equal(t, 2, triggers)
}

// Stale signatures make the scanner unready: no clean verdict is issued.
func TestStaleSignaturesFailClosed(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	writeState(t, state, time.Now().Add(-25*time.Hour))
	adapter := &scanner.Adapter{Engine: &fakeEngine{}, StatePath: state, AllowedClient: coordinatorID}
	adapter.Refresh(context.Background())
	ready, cause := adapter.Ready()
	assert.False(t, ready)
	assert.Equal(t, "signatures_stale", cause)
	rec := httptest.NewRecorder()
	adapter.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/admit", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code, "an unauthenticated caller is refused before readiness")

	require.NoError(t, os.Remove(state))
	adapter.Refresh(context.Background())
	ready, cause = adapter.Ready()
	assert.False(t, ready)
	assert.Equal(t, "signatures_unavailable", cause)
}

func TestUploadLimitsAndIdempotency(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	in := artifactstore.AdmitInput{SourceType: artifactstore.SourceUpload, DisplayName: "big.txt", ClaimedMIME: "text/plain",
		Attribution: artifactstore.Attribution{RequestActorID: h.actorID}, ContainerDepth: 1}
	over := artifactstore.MaxSize + 1
	declared := in
	declared.ExpectedSize = &over
	_, err := h.svc.Admit(ctx, declared, bytes.NewReader(nil))
	assert.ErrorIs(t, err, artifactstore.ErrTooLarge, "a declared size over 25 MiB is refused before any byte")
	_, err = h.svc.Admit(ctx, in, io.LimitReader(zeroReader{}, over))
	assert.ErrorIs(t, err, artifactstore.ErrTooLarge, "a streamed body is cut at 25 MiB + 1")

	keyed := in
	keyed.DisplayName, keyed.IdempotencyKey = "same.txt", "idem-1"
	first, err := h.svc.Admit(ctx, keyed, bytes.NewReader([]byte("hello")))
	require.NoError(t, err)
	replay, err := h.svc.Admit(ctx, keyed, bytes.NewReader([]byte("hello")))
	require.NoError(t, err)
	assert.Equal(t, first.ID, replay.ID, "the same payload replays the existing handle")
	_, err = h.svc.Admit(ctx, keyed, bytes.NewReader([]byte("changed")))
	assert.ErrorIs(t, err, artifactstore.ErrConflict)
	renamed := keyed
	renamed.DisplayName = "other.txt"
	_, err = h.svc.Admit(ctx, renamed, bytes.NewReader([]byte("hello")))
	assert.ErrorIs(t, err, artifactstore.ErrConflict)
}

// Controlled deletion: active work blocks; a terminal reference does not;
// bytes are purged and only a content-free tombstone remains.
func TestDeletionBlocksActiveWorkAndKeepsTombstone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.admit("contract.pdf", "application/pdf", admissiontest.PDF())
	h.drain()
	runID := seedLinkedRun(t, h.pool, h.actorID, a.ID, "running")

	_, err := h.svc.RequestDelete(ctx, artifactstore.DeleteRequest{ArtifactID: a.ID, ActorID: h.actorID})
	var inUse *artifactstore.InUseError
	require.ErrorAs(t, err, &inUse)
	assert.Len(t, inUse.WorkItemIDs, 1)
	assert.True(t, h.get(a.ID).Cleared(), "a blocked deletion changes nothing")

	_, err = h.pool.Exec(ctx, `UPDATE runtime.workflow_runs SET state='succeeded', finished_at=now() WHERE id=$1`, runID)
	require.NoError(t, err)
	deleting, err := h.svc.RequestDelete(ctx, artifactstore.DeleteRequest{ArtifactID: a.ID, ActorID: h.actorID})
	require.NoError(t, err)
	assert.Equal(t, artifactstore.StorageDeleting, deleting.StorageState)
	assert.False(t, h.listed(a.ID), "deletion withdraws search before any object work")
	_, _, err = h.svc.Open(ctx, a.ID)
	assert.ErrorIs(t, err, artifactstore.ErrNotAvailable)

	h.coord.Sweep(ctx)
	got := h.get(a.ID)
	assert.Equal(t, artifactstore.StorageDeleted, got.StorageState)
	assert.Nil(t, got.DisplayName)
	assert.Equal(t, artifactstore.DeletionAdmin, *got.DeletionReason)
	assert.Zero(t, h.objectCount(""))
	again, err := h.svc.RequestDelete(ctx, artifactstore.DeleteRequest{ArtifactID: a.ID, ActorID: h.actorID})
	require.NoError(t, err, "deletion is idempotent")
	assert.Equal(t, artifactstore.StorageDeleted, again.StorageState)
	var links int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM work.artifact_links WHERE artifact_id=$1`, a.ID).Scan(&links))
	assert.Equal(t, 1, links, "historical references are preserved")
}

// Partial object-store failure keeps the artifact deleting and unreadable
// until the purge completes.
func TestPartialPurgeStaysDeleting(t *testing.T) {
	pool := testutil.NewPostgresPool(t)
	ctx := context.Background()
	var actor string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO identity.identities (key,kind,source,display_name)
		VALUES ('purge-user','user','local','Purge User') RETURNING id::text`).Scan(&actor))
	var fail bool
	var mu sync.Mutex
	setFail := func(v bool) { mu.Lock(); fail = v; mu.Unlock() }
	objects := artifacttest.NewMemoryObjects()
	objects.FailDelete = func(string) bool { mu.Lock(); defer mu.Unlock(); return fail }
	svc := artifactstore.NewService(artifactstore.NewStore(pool), objects, objects, artifactstore.Config{}, nil)
	a, err := svc.Admit(ctx, artifactstore.AdmitInput{SourceType: artifactstore.SourceUpload, DisplayName: "x.txt",
		ClaimedMIME: "text/plain", Attribution: artifactstore.Attribution{RequestActorID: actor}, ContainerDepth: 1},
		strings.NewReader("bytes"))
	require.NoError(t, err)
	coord := artifacttest.NewCoordinator(pool, objects, &artifacttest.Scanner{})
	require.NoError(t, artifacttest.Drain(ctx, coord))
	_, err = svc.RequestDelete(ctx, artifactstore.DeleteRequest{ArtifactID: a.ID, ActorID: actor})
	require.NoError(t, err)
	setFail(true)
	coord.Sweep(ctx)
	got, err := svc.Stat(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, artifactstore.StorageDeleting, got.StorageState)
	_, _, err = svc.Open(ctx, a.ID)
	assert.ErrorIs(t, err, artifactstore.ErrNotAvailable)
	setFail(false)
	coord.Sweep(ctx)
	got, err = svc.Stat(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, artifactstore.StorageDeleted, got.StorageState)
}

// Security advisory (section 18): withdrawal gates the stored original at
// once; a clean re-scan re-registers it, a rejecting one purges it.
func TestSecurityAdvisoryRescan(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.admit("advisory.txt", "text/plain", admissiontest.TXT())
	h.drain()
	require.True(t, h.get(a.ID).Cleared())

	_, err := h.store.WithdrawForRescan(ctx, a.ID, "security_advisory")
	require.NoError(t, err)
	assert.False(t, h.listed(a.ID), "withdrawal removes search before re-scan")
	_, _, err = h.svc.Open(ctx, a.ID)
	assert.ErrorIs(t, err, artifactstore.ErrNotAvailable)
	assert.Equal(t, artifactstore.StatusUnavailable, h.get(a.ID).Status())
	h.drain()
	assert.True(t, h.get(a.ID).Cleared(), "a clean re-scan re-registers the same original")
	assert.True(t, h.listed(a.ID))

	_, err = h.store.WithdrawForRescan(ctx, a.ID, "engine_defect")
	require.NoError(t, err)
	h.engine.mu.Lock()
	h.engine.infectAll = true
	h.engine.mu.Unlock()
	defer func() { h.engine.mu.Lock(); h.engine.infectAll = false; h.engine.mu.Unlock() }()
	// The adapter's EICAR self-test still passes; the stored bytes are flagged.
	h.drain()
	got := h.get(a.ID)
	assert.Equal(t, artifactstore.SecurityRejected, got.SecurityState)
	assert.Equal(t, artifactstore.StorageDeleted, got.StorageState, "a rejecting re-scan purges the original")
	assert.Zero(t, h.objectCount(""))
}

func TestDatabaseRejectsIllegalTransitions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.admit("x.txt", "text/plain", admissiontest.TXT())
	h.drain()
	for name, stmt := range map[string]string{
		"return to quarantine": `UPDATE artifact.artifacts SET storage_state='quarantined' WHERE id=$1`,
		"rewrite digest":       `UPDATE artifact.artifacts SET digest='sha256:` + strings.Repeat("0", 64) + `' WHERE id=$1`,
		"un-reject":            `UPDATE artifact.artifacts SET security_state='pending' WHERE id=$1`,
		"change source":        `UPDATE artifact.artifacts SET source_type='tool' WHERE id=$1`,
	} {
		_, err := h.pool.Exec(ctx, stmt, a.ID)
		assert.Error(t, err, name)
	}
	_, err := h.pool.Exec(ctx, `DELETE FROM artifact.events WHERE artifact_id=$1`, a.ID)
	assert.Error(t, err, "events are append-only")
	_, err = h.pool.Exec(ctx, `UPDATE artifact.security_attempts SET outcome_reason='forged' WHERE artifact_id=$1`, a.ID)
	assert.Error(t, err, "terminal attempts are immutable")
}

func seedLinkedRun(t *testing.T, pool *pgxpool.Pool, actor, artifactID, state string) string {
	t.Helper()
	ctx := context.Background()
	var defID, runID, itemID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO workflow.definitions (key,version,digest,spec_json,scope_identity_id,source_type,pool_key)
		VALUES ('artifact-wf','1','sha256:test','{}',$1,'operator_artifact','ns/pool') RETURNING id::text`, actor).Scan(&defID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO runtime.workflow_runs (kind,definition_key,scope_identity_id,session_id,session_dir,state,started_at)
		VALUES ('workflow','artifact-wf:1',$1,'s1','/sessions/s1',$2,now()) RETURNING id::text`, actor, state).Scan(&runID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO work.work_items (workflow_key,scope_identity_id,title,start_identity_id,start_idempotency_key,start_payload_hash)
		VALUES ('artifact-wf',$1,'Artifact work',$1,'k1','h1') RETURNING id::text`, actor).Scan(&itemID))
	_, err := pool.Exec(ctx, `
		INSERT INTO work.attempts (id,work_item_id,number,definition_id,definition_key,definition_version,definition_digest,graph_snapshot)
		VALUES ($1,$2,1,$3,'artifact-wf:1','1','sha256:test','{}')`, runID, itemID, defID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO work.artifact_links (artifact_id,work_item_id,attempt_id,role) VALUES ($1,$2,$3,'source')`,
		artifactID, itemID, runID)
	require.NoError(t, err)
	return runID
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
