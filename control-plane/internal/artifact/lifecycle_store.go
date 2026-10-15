package artifact

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission"
)

// Retry schedule for one trigger (DES-HOR-460-08): attempt 1 immediately,
// attempt 2 after one minute, attempt 3 after five minutes, never a fourth.
var retryDelays = map[int]time.Duration{2: time.Minute, 3: 5 * time.Minute}

// SecurityLease is one fenced security attempt held by a coordinator.
type SecurityLease struct {
	AttemptID      string
	Generation     int64
	TriggerID      string
	TriggerAttempt int
	Artifact       Artifact
	InputObjectID  string
	InputKey       string
	InputZone      string
}

// LeaseSecurityAttempt leases the oldest due attempt with database time and
// SKIP LOCKED, increments its generation, and moves the artifact to scanning.
func (s *Store) LeaseSecurityAttempt(ctx context.Context, owner string, ttl time.Duration) (SecurityLease, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecurityLease{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var l SecurityLease
	err = tx.QueryRow(ctx, `
		WITH next AS (
			SELECT sa.id FROM artifact.security_attempts sa
			JOIN artifact.artifacts a ON a.id=sa.artifact_id
			WHERE sa.state='queued' AND sa.not_before<=now()
			  AND a.storage_state IN ('quarantined','stored')
			ORDER BY sa.not_before, sa.created_at
			FOR UPDATE OF sa SKIP LOCKED LIMIT 1)
		UPDATE artifact.security_attempts sa
		SET state='leased',lease_owner=$1,lease_generation=sa.lease_generation+1,
		    lease_expires_at=now()+$2::interval,started_at=COALESCE(sa.started_at,now())
		FROM next WHERE sa.id=next.id
		RETURNING sa.id::text,sa.lease_generation,sa.trigger_id::text,sa.trigger_attempt,sa.artifact_id::text,
		          sa.input_object_id::text`, owner, ttl.String()).
		Scan(&l.AttemptID, &l.Generation, &l.TriggerID, &l.TriggerAttempt, &l.Artifact.ID, &l.InputObjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecurityLease{}, ErrNotFound
	}
	if err != nil {
		return SecurityLease{}, fmt.Errorf("lease security attempt: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT storage_key,zone FROM artifact.objects WHERE id=$1 AND state='committed'`,
		l.InputObjectID).Scan(&l.InputKey, &l.InputZone); err != nil {
		return SecurityLease{}, fmt.Errorf("resolve security input object: %w", err)
	}
	l.Artifact, err = scanArtifact(tx.QueryRow(ctx, `
		UPDATE artifact.artifacts AS a SET security_state='scanning'
		WHERE a.id=$1 AND a.security_state IN ('pending','rescan_pending','scanning')
		RETURNING `+artifactColumns, l.Artifact.ID))
	if err != nil {
		return SecurityLease{}, fmt.Errorf("enter scanning: %w", err)
	}
	return l, tx.Commit(ctx)
}

// fenceTx locks the exact unexpired lease generation or reports ErrLeaseLost.
func fenceTx(ctx context.Context, tx pgx.Tx, l SecurityLease) error {
	var ok bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM artifact.security_attempts
		WHERE id=$1 AND state='leased' AND lease_generation=$2 AND lease_expires_at>now() FOR UPDATE)`,
		l.AttemptID, l.Generation).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrLeaseLost
	}
	return nil
}

// Evidence is the bounded, content-free verdict evidence of one attempt.
type Evidence struct {
	EngineVersion       string
	SignatureVersion    string
	SignatureAgeSeconds int64
	PolicyVersion       string
	CanonicalMIME       string
	Reason              string
	Signature           string // malware signature name only; never scanner output
}

func finishAttemptTx(ctx context.Context, tx pgx.Tx, l SecurityLease, state string, ev Evidence) error {
	_, err := tx.Exec(ctx, `
		UPDATE artifact.security_attempts SET state=$2,finished_at=now(),engine_version=$3,signature_version=$4,
			signature_age_seconds=$5,policy_version=$6,canonical_mime=$7,outcome_reason=$8
		WHERE id=$1`, l.AttemptID, state, nullable(ev.EngineVersion), nullable(ev.SignatureVersion),
		ev.SignatureAgeSeconds, nullable(ev.PolicyVersion), nullable(ev.CanonicalMIME), nullable(ev.Reason))
	return err
}

// CompleteTransient records a retryable failure and schedules the next
// attempt of the same trigger, or projects retryable failure after attempt 3.
func (s *Store) CompleteTransient(ctx context.Context, l SecurityLease, ev Evidence) (Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fenceTx(ctx, tx, l); err != nil {
		return Artifact{}, err
	}
	if err := finishAttemptTx(ctx, tx, l, "retryable_failed", ev); err != nil {
		return Artifact{}, err
	}
	a, err := scheduleNextTx(ctx, tx, l.Artifact.ID, l.TriggerID, l.TriggerAttempt, l.InputObjectID, ev.Reason)
	if err != nil {
		return Artifact{}, err
	}
	return a, tx.Commit(ctx)
}

func scheduleNextTx(ctx context.Context, tx pgx.Tx, artifactID, triggerID string, triggerAttempt int, objectID, reason string) (Artifact, error) {
	next := triggerAttempt + 1
	if delay, ok := retryDelays[next]; ok {
		if _, err := tx.Exec(ctx, `
			INSERT INTO artifact.security_attempts
				(artifact_id,attempt_number,trigger_id,trigger_attempt,trigger_kind,input_object_id,input_digest,not_before)
			SELECT $1, MAX(attempt_number)+1, $2, $3, 'automatic_retry', $4,
			       (SELECT digest FROM artifact.objects WHERE id=$4), now()+$5::interval
			FROM artifact.security_attempts WHERE artifact_id=$1`,
			artifactID, triggerID, next, objectID, delay.String()); err != nil {
			return Artifact{}, fmt.Errorf("schedule automatic retry: %w", err)
		}
		a, err := scanArtifact(tx.QueryRow(ctx, `
			UPDATE artifact.artifacts AS a SET security_state='pending'
			WHERE a.id=$1 RETURNING `+artifactColumns, artifactID))
		if err != nil {
			return Artifact{}, err
		}
		return a, appendEvent(ctx, tx, artifactID, "security_retry_scheduled", "", reason, map[string]any{"attempt": next})
	}
	a, err := scanArtifact(tx.QueryRow(ctx, `
		UPDATE artifact.artifacts AS a SET security_state='retryable_failed',security_reason=$2
		WHERE a.id=$1 RETURNING `+artifactColumns, artifactID, reason))
	if err != nil {
		return Artifact{}, err
	}
	return a, appendEvent(ctx, tx, artifactID, "security_check_failed", "", reason, nil)
}

// CompleteRejected commits a permanent malware/type verdict, withdraws any
// visibility, and starts the immediate byte purge.
func (s *Store) CompleteRejected(ctx context.Context, l SecurityLease, ev Evidence, children ...ChildRecord) (Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fenceTx(ctx, tx, l); err != nil {
		return Artifact{}, err
	}
	if err := finishAttemptTx(ctx, tx, l, "rejected", ev); err != nil {
		return Artifact{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE artifact.artifacts SET security_state='rejected',security_reason=$2 WHERE id=$1`,
		l.Artifact.ID, ev.Reason); err != nil {
		return Artifact{}, fmt.Errorf("commit rejection: %w", err)
	}
	// An infected email container still yields its children: each enters
	// quarantine with lineage and its own scan, so a malicious attachment
	// never blocks its siblings while the container bytes are purged
	// (DES-HOR-461-03).
	for _, c := range children {
		if err := commitChildTx(ctx, tx, l.Artifact, c); err != nil {
			return Artifact{}, err
		}
	}
	detail := map[string]any{}
	if len(children) > 0 {
		detail["children"] = len(children)
	}
	if ev.Signature != "" {
		detail["signature"] = ev.Signature
	}
	if err := appendEvent(ctx, tx, l.Artifact.ID, "security_rejected", "", ev.Reason, detail); err != nil {
		return Artifact{}, err
	}
	a, err := enterDeletingTx(ctx, tx, l.Artifact.ID, DeleteRequest{ArtifactID: l.Artifact.ID, Reason: DeletionSecurityRejected})
	if err != nil {
		return Artifact{}, err
	}
	return a, tx.Commit(ctx)
}

// ChildRecord is one container child whose quarantine bytes are already
// written under its reserved uploading row. The parent's clearance
// transaction commits it into quarantine with lineage.
type ChildRecord struct {
	ArtifactID   string
	ObjectID     string
	Size         int64
	Digest       string
	HTMLObjectID string
	HTMLSize     int64
	HTMLDigest   string
}

// Promotion is the exact safe-zone object reserved for a cleared original.
type Promotion struct {
	ObjectID string
}

// ReserveChild returns the uploading reconciliation row for one container
// position, reusing a reservation left by an interrupted attempt. A child
// that already committed (a re-scan of a cleared container) is reported with
// reused=true and must not be expanded again.
func (s *Store) ReserveChild(ctx context.Context, parent Artifact, index int, kind, displayName, claimedMIME string) (id string, committed bool, err error) {
	// The adapter already normalizes child names; the metadata boundary never
	// trusts that, so an unsafe name becomes the generated attachment name.
	if name, ok := admission.NormalizeFilename(displayName); ok {
		displayName = name
	} else {
		displayName = "attachment-" + strconv.Itoa(index)
	}
	var state string
	err = s.pool.QueryRow(ctx, `
		SELECT id::text,storage_state FROM artifact.artifacts
		WHERE parent_artifact_id=$1 AND child_index=$2 AND storage_state<>'deleted'`, parent.ID, index).Scan(&id, &state)
	if err == nil {
		return id, state != StorageUploading, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO artifact.artifacts
			(source_type,source_ref,parent_artifact_id,child_index,child_kind,container_depth,
			 created_by_identity_id,initiating_human_identity_id,executing_identity_id,authorization_source,
			 session_id,api_key_id,display_name,claimed_mime,retention_until)
		SELECT p.source_type,p.source_ref,p.id,$2,$3,LEAST(p.container_depth+1,16),
		       p.created_by_identity_id,p.initiating_human_identity_id,p.executing_identity_id,p.authorization_source,
		       p.session_id,p.api_key_id,$4,$5,p.retention_until
		FROM artifact.artifacts p WHERE p.id=$1
		RETURNING id::text`, parent.ID, index, kind, displayName, nullable(claimedMIME)).Scan(&id)
	return id, false, err
}

// CompleteCleared commits, in one transaction: the promoted original, stored
// storage, canonical type, clearance, first registration (or re-registration),
// the metadata search row, extraction eligibility, every container child, and
// the attempt result (section 9 step 8).
func (s *Store) CompleteCleared(ctx context.Context, l SecurityLease, ev Evidence, p Promotion, children []ChildRecord) (Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fenceTx(ctx, tx, l); err != nil {
		return Artifact{}, err
	}
	src := l.Artifact
	if p.ObjectID != "" {
		if err := commitObjectTx(ctx, tx, p.ObjectID, *src.Digest, *src.SizeBytes, ev.CanonicalMIME); err != nil {
			return Artifact{}, fmt.Errorf("commit original object: %w", err)
		}
	}
	if err := finishAttemptTx(ctx, tx, l, "cleared", ev); err != nil {
		return Artifact{}, err
	}
	a, err := scanArtifact(tx.QueryRow(ctx, `
		UPDATE artifact.artifacts AS a SET
			storage_state='stored',security_state='cleared',security_reason=NULL,
			canonical_mime=COALESCE(a.canonical_mime,$2),cleared_at=now(),
			registered_at=COALESCE(a.registered_at,now()),withdrawn_at=NULL,withdrawn_reason=NULL,
			extraction_state=CASE WHEN a.extraction_state='not_eligible' THEN 'pending' ELSE a.extraction_state END
		WHERE a.id=$1 RETURNING `+artifactColumns, src.ID, ev.CanonicalMIME))
	if err != nil {
		return Artifact{}, fmt.Errorf("commit clearance: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO artifact.search_documents (artifact_id,display_name,canonical_mime,source_label,uploader_name,registered_at)
		SELECT a.id, a.display_name, a.canonical_mime, a.source_type,
		       COALESCE((SELECT i.display_name FROM identity.identities i
		                 WHERE i.id=COALESCE(a.initiating_human_identity_id,a.created_by_identity_id)),''),
		       a.registered_at
		FROM artifact.artifacts a WHERE a.id=$1
		ON CONFLICT (artifact_id) DO UPDATE SET display_name=EXCLUDED.display_name`, src.ID); err != nil {
		return Artifact{}, fmt.Errorf("register search projection: %w", err)
	}
	for _, c := range children {
		if err := commitChildTx(ctx, tx, a, c); err != nil {
			return Artifact{}, err
		}
	}
	detail := map[string]any{"children": len(children)}
	if err := appendEvent(ctx, tx, src.ID, "security_cleared", "", "", detail); err != nil {
		return Artifact{}, err
	}
	return a, tx.Commit(ctx)
}

func commitObjectTx(ctx context.Context, tx pgx.Tx, objectID, digest string, size int64, contentType string) error {
	ct, err := tx.Exec(ctx, `
		UPDATE artifact.objects SET state='committed',digest=$2,size_bytes=$3,content_type=$4,committed_at=now()
		WHERE id=$1 AND state='writing'`, objectID, digest, size, nullable(contentType))
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("object %s is not a writing reservation", objectID)
	}
	return nil
}

// commitChildTx moves one reserved child into quarantine with the parent's
// lineage and queues its own security check.
func commitChildTx(ctx context.Context, tx pgx.Tx, parent Artifact, c ChildRecord) error {
	if err := commitObjectTx(ctx, tx, c.ObjectID, c.Digest, c.Size, ""); err != nil {
		return fmt.Errorf("commit child object: %w", err)
	}
	if c.HTMLObjectID != "" {
		// The original HTML is quarantined source evidence: it is never
		// promoted, served, or rendered, and is purged with the body.
		if err := commitObjectTx(ctx, tx, c.HTMLObjectID, c.HTMLDigest, c.HTMLSize, "text/html"); err != nil {
			return fmt.Errorf("commit html source object: %w", err)
		}
	}
	ct, err := tx.Exec(ctx, `
		UPDATE artifact.artifacts SET size_bytes=$2,digest=$3,storage_state='quarantined',quarantined_at=now()
		WHERE id=$1 AND parent_artifact_id=$4 AND storage_state='uploading'`, c.ArtifactID, c.Size, c.Digest, parent.ID)
	if err != nil {
		return fmt.Errorf("commit container child: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("container child %s is not an uploading reservation", c.ArtifactID)
	}
	if err := queueSecurityAttemptTx(ctx, tx, c.ArtifactID, c.ObjectID, c.Digest, "initial", "", ""); err != nil {
		return err
	}
	return appendEvent(ctx, tx, c.ArtifactID, "container_child_admitted", "", "",
		map[string]any{"parentArtifactId": parent.ID})
}

// MarkObjectPurged records an idempotent object delete.
func (s *Store) MarkObjectPurged(ctx context.Context, objectID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE artifact.objects SET state='purged',storage_key=NULL,purged_at=now(),purge_error=NULL
		WHERE id=$1 AND state<>'purged'`, objectID)
	return err
}

// RecordPurgeError keeps a bounded error class on the object.
func (s *Store) RecordPurgeError(ctx context.Context, objectID, class string) {
	_, _ = s.pool.Exec(ctx, `UPDATE artifact.objects SET purge_error=$2 WHERE id=$1 AND state<>'purged'`, objectID, class)
}

// RecordWritingObject reserves a unique key before a coordinator write so an
// interrupted write is always reconcilable.
func (s *Store) RecordWritingObject(ctx context.Context, objectID, artifactID, zone, kind, key string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO artifact.objects (id,artifact_id,zone,kind,storage_key) VALUES ($1,$2,$3,$4,$5)`,
		objectID, artifactID, zone, kind, key)
	return err
}

// PurgeObject is one object still holding bytes for a purge.
type PurgeObject struct {
	ID  string
	Key string
}

// UnpurgedObjects lists every object of an artifact that may still hold bytes.
func (s *Store) UnpurgedObjects(ctx context.Context, artifactID string) ([]PurgeObject, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text,storage_key FROM artifact.objects
		WHERE artifact_id=$1 AND state<>'purged' ORDER BY created_at`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PurgeObject
	for rows.Next() {
		var o PurgeObject
		if err := rows.Scan(&o.ID, &o.Key); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// QuarantineResidue lists quarantine objects of promoted artifacts.
func (s *Store) QuarantineResidue(ctx context.Context, artifactID string) ([]PurgeObject, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id::text,o.storage_key FROM artifact.objects o
		WHERE o.artifact_id=$1 AND o.zone='quarantine' AND o.kind='original' AND o.state<>'purged'`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PurgeObject
	for rows.Next() {
		var o PurgeObject
		if err := rows.Scan(&o.ID, &o.Key); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FinishDeleted minimizes the row to the section 8.8 tombstone once every
// object is purged.
func (s *Store) FinishDeleted(ctx context.Context, id string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM artifact.objects WHERE artifact_id=$1 AND state<>'purged'`, id).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining > 0 {
		return false, nil
	}
	ct, err := tx.Exec(ctx, `
		UPDATE artifact.artifacts SET storage_state='deleted',deleted_at=now(),display_name=NULL,deletion_error=NULL
		WHERE id=$1 AND storage_state='deleting'`, id)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil
	}
	if err := appendEvent(ctx, tx, id, "deletion_completed", "", "", nil); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// RecordDeletionError keeps a bounded error class on a deleting artifact.
func (s *Store) RecordDeletionError(ctx context.Context, id, class string) {
	_, _ = s.pool.Exec(ctx, `UPDATE artifact.artifacts SET deletion_error=$2 WHERE id=$1 AND storage_state='deleting'`, id, class)
}

func (s *Store) listIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Deleting lists artifacts whose bytes are still being purged.
func (s *Store) Deleting(ctx context.Context, limit int) ([]string, error) {
	return s.listIDs(ctx, `SELECT id::text FROM artifact.artifacts WHERE storage_state='deleting'
		ORDER BY deletion_started_at LIMIT $1`, limit)
}

// PromotedWithResidue lists stored artifacts that still have a quarantine
// original (quarantine deletion failed after promotion).
func (s *Store) PromotedWithResidue(ctx context.Context, limit int) ([]string, error) {
	return s.listIDs(ctx, `SELECT DISTINCT a.id::text FROM artifact.artifacts a
		JOIN artifact.objects o ON o.artifact_id=a.id AND o.zone='quarantine' AND o.kind='original' AND o.state<>'purged'
		WHERE a.storage_state='stored' LIMIT $1`, limit)
}

// StaleUploading returns uploads that never committed within the TTL.
func (s *Store) StaleUploading(ctx context.Context, before time.Time, limit int) ([]string, error) {
	return s.listIDs(ctx, `SELECT id::text FROM artifact.artifacts
		WHERE storage_state='uploading' AND created_at < $1 ORDER BY created_at LIMIT $2`, before, limit)
}

// AbandonUpload deletes an incomplete upload once its objects are purged. It
// never became visible, so only the content-free row remains.
func (s *Store) AbandonUpload(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM artifact.objects WHERE artifact_id=$1 AND state<>'purged'`, id).Scan(&remaining); err != nil {
		return err
	}
	if remaining > 0 {
		return nil
	}
	ct, err := tx.Exec(ctx, `
		UPDATE artifact.artifacts SET storage_state='deleted',deleted_at=now(),deletion_started_at=now(),
			deletion_reason='incomplete_upload',display_name=NULL
		WHERE id=$1 AND storage_state='uploading'`, id)
	if err != nil || ct.RowsAffected() == 0 {
		return err
	}
	if err := appendEvent(ctx, tx, id, "upload_abandoned", "", DeletionIncompleteUpload, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ExpiredQuarantine lists quarantined artifacts whose security check is not
// progressing and whose seven-day quarantine retention has passed.
func (s *Store) ExpiredQuarantine(ctx context.Context, before time.Time, limit int) ([]string, error) {
	return s.listIDs(ctx, `SELECT a.id::text FROM artifact.artifacts a
		WHERE a.storage_state='quarantined' AND a.quarantined_at < $1
		  AND NOT EXISTS (SELECT 1 FROM artifact.security_attempts sa
		                  WHERE sa.artifact_id=a.id AND sa.state IN ('queued','leased'))
		ORDER BY a.quarantined_at LIMIT $2`, before, limit)
}

// RetentionDue lists stored artifacts whose retention deadline passed.
func (s *Store) RetentionDue(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return s.listIDs(ctx, `SELECT id::text FROM artifact.artifacts
		WHERE storage_state='stored' AND retention_until IS NOT NULL AND retention_until <= $1
		ORDER BY retention_until LIMIT $2`, now, limit)
}

// ExpireLeases converts expired leases into a transient attempt outcome so a
// crashed coordinator never leaves an artifact scanning forever.
func (s *Store) ExpireLeases(ctx context.Context, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text,lease_generation,trigger_id::text,trigger_attempt,artifact_id::text,input_object_id::text
		FROM artifact.security_attempts WHERE state='leased' AND lease_expires_at < now() LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var leases []SecurityLease
	for rows.Next() {
		var l SecurityLease
		if err := rows.Scan(&l.AttemptID, &l.Generation, &l.TriggerID, &l.TriggerAttempt, &l.Artifact.ID, &l.InputObjectID); err != nil {
			rows.Close()
			return 0, err
		}
		leases = append(leases, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, l := range leases {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return n, err
		}
		ct, err := tx.Exec(ctx, `
			UPDATE artifact.security_attempts SET state='abandoned',finished_at=now(),outcome_reason='lease_expired'
			WHERE id=$1 AND state='leased' AND lease_generation=$2 AND lease_expires_at < now()`, l.AttemptID, l.Generation)
		if err == nil && ct.RowsAffected() == 1 {
			_, err = scheduleNextTx(ctx, tx, l.Artifact.ID, l.TriggerID, l.TriggerAttempt, l.InputObjectID, "lease_expired")
		}
		if err == nil {
			err = tx.Commit(ctx)
			n++
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// QueueOrphanedQuarantine re-enqueues quarantined artifacts that are pending
// security with no open attempt (for example after a restore).
func (s *Store) QueueOrphanedQuarantine(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO artifact.security_attempts
			(artifact_id,attempt_number,trigger_id,trigger_attempt,trigger_kind,input_object_id,input_digest)
		SELECT a.id,
		       COALESCE((SELECT MAX(attempt_number) FROM artifact.security_attempts x WHERE x.artifact_id=a.id),0)+1,
		       gen_random_uuid(),1,'initial',o.id,a.digest
		FROM artifact.artifacts a
		JOIN artifact.objects o ON o.artifact_id=a.id AND o.zone='quarantine' AND o.kind='original' AND o.state='committed'
		WHERE a.storage_state='quarantined' AND a.security_state='pending'
		  AND NOT EXISTS (SELECT 1 FROM artifact.security_attempts sa
		                  WHERE sa.artifact_id=a.id AND sa.state IN ('queued','leased'))`)
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// StaleWritingObjects lists reserved objects that never committed: a failed
// promotion copy or an interrupted container expansion. They were never
// readable and are purged after the reconciliation hold.
func (s *Store) StaleWritingObjects(ctx context.Context, before time.Time, limit int) ([]PurgeObject, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id::text,o.storage_key FROM artifact.objects o
		JOIN artifact.artifacts a ON a.id=o.artifact_id
		WHERE o.state='writing' AND o.created_at < $1 AND a.storage_state<>'uploading'
		ORDER BY o.created_at LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PurgeObject
	for rows.Next() {
		var o PurgeObject
		if err := rows.Scan(&o.ID, &o.Key); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// QueueStats is the bounded processing backlog for metrics.
type QueueStats struct {
	SecurityQueued      int64
	SecurityOldestAge   float64
	QuarantineObjects   int64
	QuarantineBytes     int64
	DeletingCount       int64
	DeletingOldestAge   float64
	RegisteredArtifacts int64
}

func (s *Store) Stats(ctx context.Context) (QueueStats, error) {
	var st QueueStats
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM artifact.security_attempts WHERE state IN ('queued','leased')),
		  COALESCE((SELECT EXTRACT(EPOCH FROM now()-min(created_at)) FROM artifact.security_attempts WHERE state IN ('queued','leased')),0),
		  (SELECT count(*) FROM artifact.artifacts WHERE storage_state IN ('quarantined','promoting')),
		  COALESCE((SELECT sum(size_bytes) FROM artifact.artifacts WHERE storage_state IN ('quarantined','promoting')),0),
		  (SELECT count(*) FROM artifact.artifacts WHERE storage_state='deleting'),
		  COALESCE((SELECT EXTRACT(EPOCH FROM now()-min(deletion_started_at)) FROM artifact.artifacts WHERE storage_state='deleting'),0),
		  (SELECT count(*) FROM artifact.artifacts WHERE registered_at IS NOT NULL AND withdrawn_at IS NULL)`).
		Scan(&st.SecurityQueued, &st.SecurityOldestAge, &st.QuarantineObjects, &st.QuarantineBytes,
			&st.DeletingCount, &st.DeletingOldestAge, &st.RegisteredArtifacts)
	return st, err
}

// WithdrawForRescan is the security-advisory path (section 18): it atomically
// unregisters a cleared artifact, removes its search row, gates every read
// through rescan_pending, and queues an exact re-scan of the stored original.
// A clean re-scan re-registers it; a rejecting one starts deletion.
func (s *Store) WithdrawForRescan(ctx context.Context, id, reason string) (Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactColumns+`
		FROM artifact.artifacts a WHERE a.id=$1 FOR UPDATE`, id))
	if err != nil {
		return Artifact{}, err
	}
	if a.StorageState != StorageStored || a.SecurityState != SecurityCleared {
		return Artifact{}, ErrNotRetryable
	}
	var objectID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM artifact.objects
		WHERE artifact_id=$1 AND zone='original' AND kind='original' AND state='committed'`, id).Scan(&objectID); err != nil {
		return Artifact{}, fmt.Errorf("resolve stored original: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM artifact.search_documents WHERE artifact_id=$1`, id); err != nil {
		return Artifact{}, err
	}
	a, err = scanArtifact(tx.QueryRow(ctx, `
		UPDATE artifact.artifacts AS a SET security_state='rescan_pending',withdrawn_at=now(),withdrawn_reason=$2
		WHERE a.id=$1 RETURNING `+artifactColumns, id, reason))
	if err != nil {
		return Artifact{}, err
	}
	if err := queueSecurityAttemptTx(ctx, tx, id, objectID, *a.Digest, "security_advisory", "", ""); err != nil {
		return Artifact{}, err
	}
	if err := appendEvent(ctx, tx, id, "security_withdrawn", "", reason, nil); err != nil {
		return Artifact{}, err
	}
	return a, tx.Commit(ctx)
}
