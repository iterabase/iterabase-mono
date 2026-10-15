package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanproto"
	cpmetrics "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/metrics"
)

// CoordinatorConfig bounds the trusted lifecycle worker (section 11).
type CoordinatorConfig struct {
	Owner         string
	Concurrency   int
	LeaseTTL      time.Duration
	ScanTimeout   time.Duration
	PollInterval  time.Duration
	SweepInterval time.Duration
	UploadTTL     time.Duration
	QuarantineTTL time.Duration
	OrphanHold    time.Duration
	QuarantineCap int64
}

func (c CoordinatorConfig) defaults() CoordinatorConfig {
	if c.Owner == "" {
		c.Owner = "coordinator-" + uuid.NewString()
	}
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = 5 * time.Minute
	}
	if c.ScanTimeout == 0 {
		c.ScanTimeout = 120 * time.Second
	}
	if c.PollInterval == 0 {
		c.PollInterval = time.Second
	}
	if c.SweepInterval == 0 {
		c.SweepInterval = 30 * time.Second
	}
	if c.UploadTTL == 0 {
		c.UploadTTL = time.Hour
	}
	if c.QuarantineTTL == 0 {
		c.QuarantineTTL = 7 * 24 * time.Hour
	}
	if c.OrphanHold == 0 {
		c.OrphanHold = 24 * time.Hour
	}
	return c
}

// Coordinator is the only holder of the lifecycle object credential. It
// leases security attempts, streams exact quarantine bytes to the isolated
// scanner, admits container children, promotes cleared originals, and purges
// bytes. It never parses customer content itself.
type Coordinator struct {
	store   *Store
	objects LifecycleStore
	scanner Scanner
	cfg     CoordinatorConfig
	log     *slog.Logger
	metrics *cpmetrics.Metrics
	now     func() time.Time
}

func NewCoordinator(store *Store, objects LifecycleStore, scanner Scanner, cfg CoordinatorConfig, log *slog.Logger, m *cpmetrics.Metrics) *Coordinator {
	if log == nil {
		log = slog.Default()
	}
	return &Coordinator{store: store, objects: objects, scanner: scanner, cfg: cfg.defaults(), log: log, metrics: m, now: time.Now}
}

// Ready requires the database and the lifecycle object credential.
func (c *Coordinator) Ready(ctx context.Context) error {
	if _, err := c.store.Stats(ctx); err != nil {
		return err
	}
	return c.objects.Ready(ctx)
}

// Run starts the scan workers and the reconciliation sweeper until ctx ends.
func (c *Coordinator) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.scanLoop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(c.cfg.SweepInterval)
		defer t.Stop()
		for {
			c.Sweep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	wg.Wait()
}

func (c *Coordinator) scanLoop(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := c.ProcessOne(ctx)
		if err != nil && ctx.Err() == nil {
			c.log.Warn("artifact security attempt failed", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.PollInterval):
		}
	}
}

// ProcessOne leases and completes at most one security attempt.
func (c *Coordinator) ProcessOne(ctx context.Context) (bool, error) {
	lease, err := c.store.LeaseSecurityAttempt(ctx, c.cfg.Owner, c.cfg.LeaseTTL)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	start := c.now()
	outcome, err := c.process(ctx, lease)
	if errors.Is(err, ErrLeaseLost) {
		outcome = "lease_lost"
	}
	if c.metrics != nil {
		c.metrics.ArtifactSecurityAttempts.WithLabelValues(outcome).Inc()
		c.metrics.ArtifactScanDuration.WithLabelValues(outcome).Observe(c.now().Sub(start).Seconds())
	}
	return true, err
}

func (c *Coordinator) process(ctx context.Context, l SecurityLease) (string, error) {
	resp, verify, release, err := c.scan(ctx, l)
	if err != nil {
		var te *TransientError
		if !errors.As(err, &te) {
			te = &TransientError{Reason: "processing_error", Err: err}
		}
		c.log.Warn("artifact security attempt transient failure", "attempt", l.AttemptID, "reason", te.Reason, "error", err)
		_, cErr := c.store.CompleteTransient(ctx, l, Evidence{Reason: te.Reason})
		return "transient", cErr
	}
	defer release()
	v := resp.Verdict
	ev := Evidence{EngineVersion: v.EngineVersion, SignatureVersion: v.SignatureVersion,
		SignatureAgeSeconds: v.SignatureAgeSeconds, PolicyVersion: v.PolicyVersion,
		CanonicalMIME: v.CanonicalMIME, Reason: v.Reason, Signature: v.Signature}
	if v.Malware == scanproto.MalwareInfected {
		ev.Reason, ev.CanonicalMIME = "malware", ""
		if v.Children == 0 {
			_, err := c.store.CompleteRejected(ctx, l, ev)
			return "malware", err
		}
		// An infected email container: its own bytes are rejected, and its
		// children are admitted with lineage for independent scans, so the
		// malicious part cannot block its siblings (DES-HOR-461-03).
		if err := verify(); err != nil {
			_, cErr := c.store.CompleteTransient(ctx, l, Evidence{Reason: "input_mismatch"})
			c.log.Error("artifact quarantine bytes do not match committed digest", "attempt", l.AttemptID)
			return "transient", cErr
		}
		children, err := c.admitChildren(ctx, l, resp)
		if err != nil {
			_, cErr := c.store.CompleteTransient(ctx, l, Evidence{Reason: transientReason(err)})
			return "transient", errors.Join(err, cErr)
		}
		_, err = c.store.CompleteRejected(ctx, l, ev, children...)
		return "malware", err
	}
	if err := verify(); err != nil {
		_, cErr := c.store.CompleteTransient(ctx, l, Evidence{Reason: "input_mismatch"})
		c.log.Error("artifact quarantine bytes do not match committed digest", "attempt", l.AttemptID)
		return "transient", cErr
	}
	if !v.Accepted {
		ev.CanonicalMIME = ""
		_, err := c.store.CompleteRejected(ctx, l, ev)
		return "policy", err
	}
	children, err := c.admitChildren(ctx, l, resp)
	if err != nil {
		_, cErr := c.store.CompleteTransient(ctx, l, Evidence{Reason: transientReason(err)})
		return "transient", errors.Join(err, cErr)
	}
	var promo Promotion
	if l.Artifact.StorageState == StorageQuarantined {
		if promo, err = c.promote(ctx, l); err != nil {
			_, cErr := c.store.CompleteTransient(ctx, l, Evidence{Reason: "promotion_failed"})
			return "transient", errors.Join(err, cErr)
		}
	}
	if _, err := c.store.CompleteCleared(ctx, l, ev, promo, children); err != nil {
		return "transient", err
	}
	c.purgeResidue(ctx, l.Artifact.ID)
	return "cleared", nil
}

// scan streams the exact quarantine object. It returns a verifier that
// proves the bytes sent matched the committed digest once the request body is
// done, and a release that ends the exchange. Verification never ends the
// exchange: child frames keep streaming on the same response until release.
func (c *Coordinator) scan(ctx context.Context, l SecurityLease) (*ScanResponse, func() error, func(), error) {
	if l.Artifact.Digest == nil || l.Artifact.SizeBytes == nil {
		return nil, nil, nil, transient("input_mismatch", errors.New("artifact has no committed integrity"))
	}
	obj, err := c.objects.Get(ctx, l.InputKey)
	if err != nil {
		return nil, nil, nil, transient("store_unavailable", err)
	}
	h := sha256.New()
	counter := &countWriter{}
	body := io.TeeReader(io.LimitReader(obj, *l.Artifact.SizeBytes+1), io.MultiWriter(h, counter))
	scanCtx, cancel := context.WithTimeout(ctx, c.cfg.ScanTimeout)
	req := ScanRequest{RequestID: l.AttemptID + "/" + fmt.Sprint(l.Generation), Digest: *l.Artifact.Digest,
		Size: *l.Artifact.SizeBytes, Filename: deref(l.Artifact.DisplayName),
		ClaimedMIME: deref(l.Artifact.ClaimedMIME), ContainerDepth: l.Artifact.ContainerDepth,
		Generated: l.Artifact.ChildKind != nil && *l.Artifact.ChildKind == ChildBody}
	resp, err := c.scanner.Scan(scanCtx, req, body)
	if err != nil {
		cancel()
		_ = obj.Close()
		return nil, nil, nil, err
	}
	var once sync.Once
	var verr error
	verify := func() error {
		once.Do(func() {
			if counter.n != *l.Artifact.SizeBytes || "sha256:"+hex.EncodeToString(h.Sum(nil)) != *l.Artifact.Digest {
				verr = transient("input_mismatch", errors.New("streamed bytes differ from committed digest"))
			}
		})
		return verr
	}
	release := func() {
		_ = resp.Close()
		cancel()
		_ = obj.Close()
	}
	return resp, verify, release, nil
}

// admitChildren writes every container child into its own reserved
// quarantine object. Children of a re-scanned (already stored) container were
// admitted by the original clearance and are drained, not duplicated.
func (c *Coordinator) admitChildren(ctx context.Context, l SecurityLease, resp *ScanResponse) ([]ChildRecord, error) {
	var out []ChildRecord
	var expanded int64
	for {
		h, data, html, err := resp.NextChild()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		expanded += h.Size + h.SourceHTMLSize
		if expanded > scanproto.MaxExpandedBytes {
			return nil, transient("protocol_error", errors.New("expansion exceeds aggregate bound"))
		}
		if l.Artifact.StorageState != StorageQuarantined {
			continue
		}
		childID, committed, err := c.store.ReserveChild(ctx, l.Artifact, h.Index, h.Kind, h.Filename, h.ClaimedMIME)
		if err != nil {
			return nil, transient("store_unavailable", err)
		}
		if committed {
			continue
		}
		rec := ChildRecord{ArtifactID: childID, Size: h.Size, Digest: h.Digest}
		if rec.ObjectID, err = c.writeChildObject(ctx, childID, "original", h.ClaimedMIME, data, h.Size, h.Digest); err != nil {
			return nil, err
		}
		if h.SourceHTMLSize > 0 {
			hh := sha256.New()
			buf := io.TeeReader(html, hh)
			key, objectID := c.reserveKey(childID)
			if err := c.store.RecordWritingObject(ctx, objectID, childID, ZoneQuarantine, "html_source", key); err != nil {
				return nil, transient("store_unavailable", err)
			}
			if err := c.objects.PutNew(ctx, key, buf, h.SourceHTMLSize, "text/html"); err != nil {
				return nil, transient("store_unavailable", err)
			}
			rec.HTMLObjectID, rec.HTMLSize, rec.HTMLDigest = objectID, h.SourceHTMLSize, digestOf(hh)
		}
		out = append(out, rec)
	}
}

func (c *Coordinator) reserveKey(artifactID string) (key, objectID string) {
	objectID = uuid.NewString()
	return ObjectKey(ZoneQuarantine, artifactID, objectID), objectID
}

func (c *Coordinator) writeChildObject(ctx context.Context, childID, kind, contentType string, data io.Reader, size int64, digest string) (string, error) {
	key, objectID := c.reserveKey(childID)
	if err := c.store.RecordWritingObject(ctx, objectID, childID, ZoneQuarantine, kind, key); err != nil {
		return "", transient("store_unavailable", err)
	}
	h := sha256.New()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := c.objects.PutNew(ctx, key, io.TeeReader(data, h), size, contentType); err != nil {
		return "", transient("store_unavailable", err)
	}
	if digestOf(h) != digest {
		return "", transient("protocol_error", errors.New("child digest mismatch"))
	}
	return objectID, nil
}

// promote copies the exact quarantine object into the cleared-original zone
// and re-verifies the destination digest before any database promotion.
func (c *Coordinator) promote(ctx context.Context, l SecurityLease) (Promotion, error) {
	objectID := uuid.NewString()
	key := ObjectKey(ZoneOriginal, l.Artifact.ID, objectID)
	if err := c.store.RecordWritingObject(ctx, objectID, l.Artifact.ID, ZoneOriginal, "original", key); err != nil {
		return Promotion{}, err
	}
	if err := c.objects.Copy(ctx, l.InputKey, key); err != nil {
		return Promotion{}, err
	}
	r, err := c.objects.Get(ctx, key)
	if err != nil {
		return Promotion{}, err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, MaxSize+1))
	if err != nil {
		return Promotion{}, err
	}
	if n != *l.Artifact.SizeBytes || digestOf(h) != *l.Artifact.Digest {
		return Promotion{}, errors.New("promoted object differs from quarantine digest")
	}
	return Promotion{ObjectID: objectID}, nil
}

// purgeResidue deletes the now-unreadable quarantine copy after promotion.
// Failure is harmless: the sweeper retries it.
func (c *Coordinator) purgeResidue(ctx context.Context, artifactID string) {
	objs, err := c.store.QuarantineResidue(ctx, artifactID)
	if err != nil {
		return
	}
	c.purge(ctx, objs, "quarantine")
}

func (c *Coordinator) purge(ctx context.Context, objs []PurgeObject, zone string) bool {
	ok := true
	for _, o := range objs {
		if err := c.objects.Delete(ctx, o.Key); err != nil {
			c.store.RecordPurgeError(context.WithoutCancel(ctx), o.ID, "object_delete_failed")
			ok = false
			continue
		}
		if err := c.store.MarkObjectPurged(ctx, o.ID); err != nil {
			ok = false
			continue
		}
		if c.metrics != nil {
			c.metrics.ArtifactPurgedObjects.WithLabelValues(zone).Inc()
		}
	}
	return ok
}

// Sweep reconciles every cross-store boundary (section 12): expired leases,
// orphaned quarantine, deletions, incomplete uploads, promotion residue,
// unreferenced reservations, quarantine expiry, and retention.
func (c *Coordinator) Sweep(ctx context.Context) {
	now := c.now()
	c.sweepAttempts(ctx)
	c.sweepObjects(ctx, now)
	c.sweepLifecycle(ctx, now)
	c.observe(ctx)
}

const sweepBatch = 100

// sweepAttempts fences expired leases and re-queues orphaned quarantine.
func (c *Coordinator) sweepAttempts(ctx context.Context) {
	if n, err := c.store.ExpireLeases(ctx, sweepBatch); err != nil {
		c.log.Warn("expire artifact leases", "error", err)
	} else if n > 0 {
		c.reconciled("lease_expired", n)
	}
	if n, err := c.store.QueueOrphanedQuarantine(ctx); err != nil {
		c.log.Warn("requeue orphaned quarantine", "error", err)
	} else if n > 0 {
		c.reconciled("orphaned_quarantine", int(n))
	}
}

// sweepObjects finishes purges and removes never-readable residue.
func (c *Coordinator) sweepObjects(ctx context.Context, now time.Time) {
	if ids, err := c.store.Deleting(ctx, sweepBatch); err == nil {
		for _, id := range ids {
			c.finishDeletion(ctx, id)
		}
	}
	if ids, err := c.store.StaleUploading(ctx, now.Add(-c.cfg.UploadTTL), sweepBatch); err == nil {
		for _, id := range ids {
			if objs, err := c.store.UnpurgedObjects(ctx, id); err == nil && c.purge(ctx, objs, "quarantine") {
				if err := c.store.AbandonUpload(ctx, id); err == nil {
					c.reconciled("stale_upload", 1)
				}
			}
		}
	}
	if ids, err := c.store.PromotedWithResidue(ctx, sweepBatch); err == nil {
		for _, id := range ids {
			c.purgeResidue(ctx, id)
		}
	}
	if objs, err := c.store.StaleWritingObjects(ctx, now.Add(-c.cfg.OrphanHold), sweepBatch); err == nil && len(objs) > 0 {
		c.purge(ctx, objs, "orphan")
		c.reconciled("orphan_object", len(objs))
	}
}

// sweepLifecycle expires unscanned quarantine and finite retention through
// the same active-reference guard as Admin deletion.
func (c *Coordinator) sweepLifecycle(ctx context.Context, now time.Time) {
	if ids, err := c.store.ExpiredQuarantine(ctx, now.Add(-c.cfg.QuarantineTTL), sweepBatch); err == nil {
		for _, id := range ids {
			c.expire(ctx, id, DeletionUnscannedExpired)
		}
	}
	if ids, err := c.store.RetentionDue(ctx, now, sweepBatch); err == nil {
		for _, id := range ids {
			c.expire(ctx, id, DeletionRetention)
		}
	}
}

func (c *Coordinator) expire(ctx context.Context, id, reason string) {
	_, err := c.store.BeginDelete(ctx, DeleteRequest{ArtifactID: id, Reason: reason})
	switch {
	case errors.Is(err, ErrInUse):
		// Retention never forces deletion through active work; it retries.
		c.deletion("retention_blocked")
	case err != nil:
		c.log.Warn("artifact lifecycle expiry failed", "reason", reason, "error", err)
	default:
		c.deletion("requested")
	}
}

func (c *Coordinator) finishDeletion(ctx context.Context, id string) {
	objs, err := c.store.UnpurgedObjects(ctx, id)
	if err != nil {
		return
	}
	if !c.purge(ctx, objs, "all") {
		c.store.RecordDeletionError(ctx, id, "object_delete_failed")
		c.deletion("retried")
		return
	}
	if done, err := c.store.FinishDeleted(ctx, id); err == nil && done {
		c.deletion("completed")
	}
}

func (c *Coordinator) observe(ctx context.Context) {
	if c.metrics == nil {
		return
	}
	st, err := c.store.Stats(ctx)
	if err != nil {
		return
	}
	c.metrics.ArtifactSecurityQueue.WithLabelValues().Set(float64(st.SecurityQueued))
	c.metrics.ArtifactSecurityOldest.WithLabelValues().Set(st.SecurityOldestAge)
	c.metrics.ArtifactQuarantineBytes.WithLabelValues().Set(float64(st.QuarantineBytes))
	c.metrics.ArtifactQuarantineObjects.WithLabelValues().Set(float64(st.QuarantineObjects))
	c.metrics.ArtifactDeleting.WithLabelValues().Set(float64(st.DeletingCount))
	c.metrics.ArtifactDeletingOldest.WithLabelValues().Set(st.DeletingOldestAge)
	c.metrics.ArtifactRegistered.WithLabelValues().Set(float64(st.RegisteredArtifacts))
	if c.cfg.QuarantineCap > 0 {
		c.metrics.ArtifactQuarantineLimit.WithLabelValues().Set(float64(c.cfg.QuarantineCap))
	}
}

func (c *Coordinator) reconciled(kind string, n int) {
	if c.metrics != nil {
		c.metrics.ArtifactReconciliations.WithLabelValues(kind).Add(float64(n))
	}
}

func (c *Coordinator) deletion(result string) {
	if c.metrics != nil {
		c.metrics.ArtifactDeletions.WithLabelValues(result).Inc()
	}
}

func transientReason(err error) string {
	var te *TransientError
	if errors.As(err, &te) {
		return te.Reason
	}
	return "processing_error"
}

func digestOf(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
