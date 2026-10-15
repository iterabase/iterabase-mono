package artifact

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns artifact metadata, objects, attempts, events, the search
// projection, and immutable work links. Every state change goes through the
// database lifecycle trigger.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const artifactColumns = `a.id::text,a.upload_id::text,a.source_type,a.source_ref,a.parent_artifact_id::text,
	a.child_index,a.child_kind,a.container_depth,a.created_by_identity_id::text,a.initiating_human_identity_id::text,
	a.api_key_id::text,a.display_name,a.claimed_mime,a.canonical_mime,a.size_bytes,a.digest,a.storage_state,
	a.security_state,a.security_reason,a.extraction_state,a.registered_at,a.withdrawn_at,a.retention_until,
	a.deletion_reason,a.created_at,a.quarantined_at,a.cleared_at,a.deleted_at,a.row_version`

func scanArtifact(row pgx.Row) (Artifact, error) {
	var a Artifact
	var depth int16
	err := row.Scan(&a.ID, &a.UploadID, &a.SourceType, &a.SourceRef, &a.ParentID,
		&a.ChildIndex, &a.ChildKind, &depth, &a.CreatedByID, &a.InitiatingHumanID,
		&a.APIKeyID, &a.DisplayName, &a.ClaimedMIME, &a.CanonicalMIME, &a.SizeBytes, &a.Digest, &a.StorageState,
		&a.SecurityState, &a.SecurityReason, &a.ExtractionState, &a.RegisteredAt, &a.WithdrawnAt, &a.RetentionUntil,
		&a.DeletionReason, &a.CreatedAt, &a.QuarantinedAt, &a.ClearedAt, &a.DeletedAt, &a.RowVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	a.ContainerDepth = int(depth)
	return a, nil
}

func collectArtifacts(rows pgx.Rows) ([]Artifact, error) {
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func appendEvent(ctx context.Context, tx pgx.Tx, artifactID, event, actor, reason string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO artifact.events (artifact_id,event,actor_identity_id,reason,detail)
		VALUES ($1,$2,$3,$4,$5)`, artifactID, event, nullable(actor), nullable(reason), detail)
	if err != nil {
		return fmt.Errorf("append artifact event %s: %w", event, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Admission
// ---------------------------------------------------------------------------

// uploadingRecord is the reconciliation handle created before any byte is
// written: one uploading artifact and one writing quarantine object.
type uploadingRecord struct {
	ArtifactID  string
	ObjectID    string
	Key         string
	Fingerprint string
	ParentID    string
	ChildIndex  int
	ChildKind   string
}

// CreateUploading records the reconciliation handle. An idempotency-key
// collision returns ErrConflict with the existing row so the caller can decide
// between replay and conflict.
func (s *Store) CreateUploading(ctx context.Context, rec uploadingRecord, in AdmitInput) (Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, fmt.Errorf("begin upload: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := insertUploadingTx(ctx, tx, rec, in)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "artifacts_idempotency" {
			existing, getErr := s.ByIdempotency(ctx, in.Attribution.RequestActorID, in.IdempotencyKey)
			if getErr != nil {
				return Artifact{}, getErr
			}
			return existing, ErrConflict
		}
		return Artifact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Artifact{}, fmt.Errorf("commit upload: %w", err)
	}
	return a, nil
}

func insertUploadingTx(ctx context.Context, tx pgx.Tx, rec uploadingRecord, in AdmitInput) (Artifact, error) {
	var childIndex, childKind any
	if rec.ParentID != "" {
		childIndex, childKind = rec.ChildIndex, rec.ChildKind
	}
	at := in.Attribution
	a, err := scanArtifact(tx.QueryRow(ctx, `
		INSERT INTO artifact.artifacts AS a
			(id,source_type,source_ref,parent_artifact_id,child_index,child_kind,container_depth,
			 created_by_identity_id,initiating_human_identity_id,executing_identity_id,authorization_source,
			 session_id,api_key_id,display_name,claimed_mime,idempotency_key,idempotency_fingerprint,retention_until)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING `+artifactColumns,
		rec.ArtifactID, in.SourceType, nullable(in.SourceRef), nullable(rec.ParentID), childIndex, childKind,
		in.ContainerDepth, at.RequestActorID, nullable(at.InitiatingHumanID), nullable(at.ExecutingID),
		nullable(at.Source), nullable(at.SessionID), nullable(at.APIKeyID), in.DisplayName,
		nullable(in.ClaimedMIME), nullable(in.IdempotencyKey), nullable(rec.Fingerprint), in.RetentionUntil))
	if err != nil {
		return Artifact{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO artifact.objects (id,artifact_id,zone,kind,storage_key,content_type)
		VALUES ($1,$2,'quarantine','original',$3,$4)`,
		rec.ObjectID, rec.ArtifactID, rec.Key, nullable(in.ClaimedMIME)); err != nil {
		return Artifact{}, fmt.Errorf("record quarantine object: %w", err)
	}
	return a, nil
}

// ByIdempotency resolves a request actor's idempotency key.
func (s *Store) ByIdempotency(ctx context.Context, actorID, key string) (Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactColumns+`
		FROM artifact.artifacts a WHERE a.created_by_identity_id=$1 AND a.idempotency_key=$2`, actorID, key))
}

// Fingerprint returns the stored idempotency fingerprint.
func (s *Store) Fingerprint(ctx context.Context, id string) (string, error) {
	var fp *string
	if err := s.pool.QueryRow(ctx, `SELECT idempotency_fingerprint FROM artifact.artifacts WHERE id=$1`, id).Scan(&fp); err != nil {
		return "", err
	}
	if fp == nil {
		return "", nil
	}
	return *fp, nil
}

// CommitQuarantine atomically records integrity, enters quarantine, queues
// security attempt 1, and (for workload outputs) writes the immutable work
// link. Nothing becomes usable here.
func (s *Store) CommitQuarantine(ctx context.Context, artifactID, objectID string, size int64, digest string, scope *Scope) (Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, fmt.Errorf("begin quarantine commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if scope != nil {
		if err := validateScopeTx(ctx, tx, *scope); err != nil {
			return Artifact{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE artifact.objects SET state='committed',digest=$2,size_bytes=$3,committed_at=now()
		WHERE id=$1 AND state='writing'`, objectID, digest, size); err != nil {
		return Artifact{}, fmt.Errorf("commit quarantine object: %w", err)
	}
	a, err := scanArtifact(tx.QueryRow(ctx, `
		UPDATE artifact.artifacts AS a
		SET size_bytes=$2,digest=$3,storage_state='quarantined',quarantined_at=now()
		WHERE a.id=$1 AND a.storage_state='uploading'
		RETURNING `+artifactColumns, artifactID, size, digest))
	if err != nil {
		return Artifact{}, fmt.Errorf("enter quarantine: %w", err)
	}
	if err := queueSecurityAttemptTx(ctx, tx, artifactID, objectID, digest, "initial", "", ""); err != nil {
		return Artifact{}, err
	}
	if scope != nil {
		var node any
		if scope.NodeExecutionID != "" {
			node = scope.NodeExecutionID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO work.artifact_links (artifact_id,work_item_id,attempt_id,node_execution_id,role,metadata)
			VALUES ($1,$2,$3,$4,$5,'{}'::jsonb)`, artifactID, scope.WorkItemID, scope.AttemptID, node, scope.Role); err != nil {
			return Artifact{}, fmt.Errorf("link artifact: %w", err)
		}
	}
	if err := appendEvent(ctx, tx, artifactID, "upload_accepted", a.CreatedByID, "", map[string]any{"sizeBytes": size}); err != nil {
		return Artifact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Artifact{}, fmt.Errorf("commit quarantine: %w", err)
	}
	return a, nil
}

// queueSecurityAttemptTx starts a new three-attempt trigger.
func queueSecurityAttemptTx(ctx context.Context, tx pgx.Tx, artifactID, objectID, digest, trigger, actor, session string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO artifact.security_attempts
			(artifact_id,attempt_number,trigger_id,trigger_attempt,trigger_kind,trigger_actor_identity_id,
			 trigger_session_id,input_object_id,input_digest)
		SELECT $1, COALESCE(MAX(attempt_number),0)+1, gen_random_uuid(), 1, $4, $5, $6, $2, $3
		FROM artifact.security_attempts WHERE artifact_id=$1`,
		artifactID, objectID, digest, trigger, nullable(actor), nullable(session))
	if err != nil {
		return fmt.Errorf("queue security attempt: %w", err)
	}
	return nil
}

func validateScopeTx(ctx context.Context, tx pgx.Tx, scope Scope) error {
	if scope.WorkItemID == "" || scope.AttemptID == "" || scope.Role == "" {
		return fmt.Errorf("%w: incomplete work scope", ErrInvalidInput)
	}
	var ok bool
	var node any
	if scope.NodeExecutionID != "" {
		node = scope.NodeExecutionID
	}
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM work.attempts a
			WHERE a.id=$1 AND a.work_item_id=$2
			  AND ($3::uuid IS NULL OR EXISTS(
				SELECT 1 FROM runtime.node_executions n
				WHERE n.id=$3::uuid AND n.attempt_id=a.id))
		)`, scope.AttemptID, scope.WorkItemID, node).Scan(&ok); err != nil {
		return fmt.Errorf("validate artifact scope: %w", err)
	}
	if !ok {
		return ErrUnauthorized
	}
	return nil
}

// QuarantineBytes is the committed quarantine backlog used for admission
// headroom.
func (s *Store) QuarantineBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(size_bytes),0) FROM artifact.artifacts
		WHERE storage_state IN ('quarantined','promoting')`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (s *Store) Get(ctx context.Context, id string) (Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactColumns+` FROM artifact.artifacts a WHERE a.id=$1`, id))
}

// GetByHandle resolves an artifact id or its provisional upload handle.
func (s *Store) GetByHandle(ctx context.Context, id string) (Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx, `SELECT `+artifactColumns+`
		FROM artifact.artifacts a WHERE a.id=$1 OR a.upload_id=$1 LIMIT 1`, id))
}

// ClearedOriginal resolves the committed original object of a currently
// cleared artifact in the same query that checks clearance (invariant 3).
func (s *Store) ClearedOriginal(ctx context.Context, id string) (Artifact, string, error) {
	var key string
	row := s.pool.QueryRow(ctx, `SELECT `+artifactColumns+`, o.storage_key
		FROM artifact.artifacts a
		JOIN artifact.objects o ON o.artifact_id=a.id AND o.zone='original' AND o.kind='original' AND o.state='committed'
		WHERE a.id=$1 AND artifact.is_cleared(a.id)`, id)
	var a Artifact
	var depth int16
	err := row.Scan(&a.ID, &a.UploadID, &a.SourceType, &a.SourceRef, &a.ParentID,
		&a.ChildIndex, &a.ChildKind, &depth, &a.CreatedByID, &a.InitiatingHumanID,
		&a.APIKeyID, &a.DisplayName, &a.ClaimedMIME, &a.CanonicalMIME, &a.SizeBytes, &a.Digest, &a.StorageState,
		&a.SecurityState, &a.SecurityReason, &a.ExtractionState, &a.RegisteredAt, &a.WithdrawnAt, &a.RetentionUntil,
		&a.DeletionReason, &a.CreatedAt, &a.QuarantinedAt, &a.ClearedAt, &a.DeletedAt, &a.RowVersion, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, "", ErrNotAvailable
	}
	if err != nil {
		return Artifact{}, "", err
	}
	a.ContainerDepth = int(depth)
	return a, key, nil
}

// Children lists a container's children in order.
func (s *Store) Children(ctx context.Context, parentID string) ([]Artifact, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+artifactColumns+`
		FROM artifact.artifacts a WHERE a.parent_artifact_id=$1 ORDER BY a.child_index`, parentID)
	if err != nil {
		return nil, err
	}
	return collectArtifacts(rows)
}

// LinkedToAttempt is the workload read boundary: the artifact is linked to
// the attempt (exact node or attempt-wide) and currently cleared.
func (s *Store) LinkedToAttempt(ctx context.Context, artifactID, attemptID, nodeID string) (bool, error) {
	var ok bool
	var node any
	if nodeID != "" {
		node = nodeID
	}
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM work.artifact_links l
			WHERE l.artifact_id=$1 AND l.attempt_id=$2 AND artifact.is_cleared(l.artifact_id)
			  AND ($3::uuid IS NULL OR l.node_execution_id IS NULL OR l.node_execution_id=$3::uuid)
		)`, artifactID, attemptID, node).Scan(&ok)
	return ok, err
}

// OwnOutputAdmissible reports whether a workload may cite its own output
// (DES-HOR-461-02): linked to the attempt as an output and not rejected.
func (s *Store) OwnOutputAdmissible(ctx context.Context, artifactID, attemptID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM work.artifact_links l
			WHERE l.artifact_id=$1 AND l.attempt_id=$2 AND l.role='output'
			  AND artifact.is_admissible_output(l.artifact_id))`, artifactID, attemptID).Scan(&ok)
	return ok, err
}

// ExecutionScope derives work scope and creator from durable execution state.
func (s *Store) ExecutionScope(ctx context.Context, attemptID, callerScope, callerScopeID string) (Scope, string, error) {
	var scope Scope
	var creator string
	var node *string
	var row pgx.Row
	switch callerScope {
	case "turn":
		row = s.pool.QueryRow(ctx, `
			SELECT a.work_item_id::text,a.id::text,t.node_execution_id::text,wi.scope_identity_id::text
			FROM work.attempts a
			JOIN work.work_items wi ON wi.id=a.work_item_id
			JOIN runtime.turns t ON t.run_id=a.id
			WHERE a.id=$1 AND t.id=$2 AND t.state='running'`, attemptID, callerScopeID)
	case "workflow_step":
		row = s.pool.QueryRow(ctx, `
			SELECT a.work_item_id::text,a.id::text,NULL::text,wi.scope_identity_id::text
			FROM work.attempts a
			JOIN work.work_items wi ON wi.id=a.work_item_id
			JOIN runtime.run_steps rs ON rs.run_id=a.id
			WHERE a.id=$1 AND rs.id=$2 AND rs.state='running'`, attemptID, callerScopeID)
	default:
		return Scope{}, "", ErrUnauthorized
	}
	if err := row.Scan(&scope.WorkItemID, &scope.AttemptID, &node, &creator); errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, "", ErrUnauthorized
	} else if err != nil {
		return Scope{}, "", fmt.Errorf("resolve execution scope: %w", err)
	}
	if node != nil {
		scope.NodeExecutionID = *node
	}
	return scope, creator, nil
}

// InitiatingHuman returns the work item's accountable human, when recorded.
func (s *Store) InitiatingHuman(ctx context.Context, workItemID string) (string, error) {
	var id *string
	err := s.pool.QueryRow(ctx, `SELECT initiating_human_identity_id::text FROM work.work_items WHERE id=$1`, workItemID).Scan(&id)
	if err != nil || id == nil {
		return "", err
	}
	return *id, nil
}

// ---------------------------------------------------------------------------
// Shared library listing and search (section 14)
// ---------------------------------------------------------------------------

// ListQuery selects registered artifacts. Search and recent are mutually
// exclusive orderings; eligibility filters run before rank and count.
type ListQuery struct {
	Search      string
	AfterTime   *time.Time
	AfterID     string
	Offset      int
	Limit       int
	SearchOrder bool
}

// ListItem is one registered artifact with its bounded, text-only snippet.
type ListItem struct {
	Artifact Artifact
	Uploader string
	Snippet  string
}

func (s *Store) List(ctx context.Context, q ListQuery) ([]ListItem, error) {
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	var rows pgx.Rows
	var err error
	if q.Search != "" {
		rows, err = s.pool.Query(ctx, `
			WITH q AS (SELECT websearch_to_tsquery('simple', $1) AS query)
			SELECT `+artifactColumns+`, d.uploader_name,
			       ts_headline('simple', d.display_name, q.query,
			                   'StartSel="",StopSel="",MaxWords=12,MinWords=3,MaxFragments=1')
			FROM artifact.search_documents d
			JOIN artifact.artifacts a ON a.id=d.artifact_id
			CROSS JOIN q
			WHERE artifact.is_cleared(a.id)
			  AND (d.metadata_tsv @@ q.query OR d.content_tsv @@ q.query)
			ORDER BY ts_rank(d.metadata_tsv, q.query) + ts_rank(d.content_tsv, q.query) DESC, a.id
			OFFSET $2 LIMIT $3`, q.Search, q.Offset, q.Limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT `+artifactColumns+`, d.uploader_name, ''
			FROM artifact.search_documents d
			JOIN artifact.artifacts a ON a.id=d.artifact_id
			WHERE artifact.is_cleared(a.id)
			  AND ($1::timestamptz IS NULL OR (a.registered_at, a.id) < ($1, $2::uuid))
			ORDER BY a.registered_at DESC, a.id DESC
			LIMIT $3`, q.AfterTime, nullable(q.AfterID), q.Limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListItem
	for rows.Next() {
		var it ListItem
		a := &it.Artifact
		var depth int16
		if err := rows.Scan(&a.ID, &a.UploadID, &a.SourceType, &a.SourceRef, &a.ParentID,
			&a.ChildIndex, &a.ChildKind, &depth, &a.CreatedByID, &a.InitiatingHumanID,
			&a.APIKeyID, &a.DisplayName, &a.ClaimedMIME, &a.CanonicalMIME, &a.SizeBytes, &a.Digest, &a.StorageState,
			&a.SecurityState, &a.SecurityReason, &a.ExtractionState, &a.RegisteredAt, &a.WithdrawnAt, &a.RetentionUntil,
			&a.DeletionReason, &a.CreatedAt, &a.QuarantinedAt, &a.ClearedAt, &a.DeletedAt, &a.RowVersion,
			&it.Uploader, &it.Snippet); err != nil {
			return nil, err
		}
		a.ContainerDepth = int(depth)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Customer lifecycle requests
// ---------------------------------------------------------------------------

// ActiveReferences counts owning-domain state that can still use the bytes
// (section 16): nonterminal attempts linking the artifact and running tool
// invocations that carry it as an input. Historical terminal references never
// block.
func activeReferencesTx(ctx context.Context, tx pgx.Tx, artifactID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT l.work_item_id::text
		FROM work.artifact_links l
		JOIN runtime.workflow_runs wr ON wr.id=l.attempt_id
		WHERE l.artifact_id=$1 AND wr.state IN ('pending','running','awaiting_approval','waiting')
		UNION
		SELECT DISTINCT a.work_item_id::text
		FROM toolgateway.invocations i
		JOIN work.attempts a ON a.id::text=i.attempt_id::text
		WHERE i.state IN ('dispatching','running')
		  AND i.artifact_input_refs @> jsonb_build_array(jsonb_build_object('artifact_id', $1::text))
		ORDER BY 1`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("query active artifact references: %w", err)
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

// InUseError carries the customer-safe owning work identifiers.
type InUseError struct{ WorkItemIDs []string }

func (e *InUseError) Error() string { return ErrInUse.Error() }
func (e *InUseError) Unwrap() error { return ErrInUse }

// DeleteRequest is the attributable deletion or retention request.
type DeleteRequest struct {
	ArtifactID string
	Reason     string
	ActorID    string
	APIKeyID   string
}

// BeginDelete runs the eligible deletion transaction (section 16): under the
// row lock it blocks active work, then atomically enters deleting, withdraws
// registration, removes the search row, and fences processing. From this
// commit every byte read is denied. Repeating a request is idempotent.
func (s *Store) BeginDelete(ctx context.Context, req DeleteRequest) (Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Artifact{}, fmt.Errorf("begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanArtifact(tx.QueryRow(ctx, `SELECT `+artifactColumns+`
		FROM artifact.artifacts a WHERE a.id=$1 FOR UPDATE`, req.ArtifactID))
	if err != nil {
		return Artifact{}, err
	}
	switch a.StorageState {
	case StorageDeleting, StorageDeleted:
		return a, nil
	case StorageUploading:
		return Artifact{}, ErrNotFound
	}
	if req.Reason != DeletionSecurityRejected {
		refs, err := activeReferencesTx(ctx, tx, a.ID)
		if err != nil {
			return Artifact{}, err
		}
		if len(refs) > 0 {
			return Artifact{}, &InUseError{WorkItemIDs: refs}
		}
	}
	a, err = enterDeletingTx(ctx, tx, a.ID, req)
	if err != nil {
		return Artifact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Artifact{}, fmt.Errorf("commit delete: %w", err)
	}
	return a, nil
}

func enterDeletingTx(ctx context.Context, tx pgx.Tx, id string, req DeleteRequest) (Artifact, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE artifact.security_attempts SET state='abandoned',finished_at=now(),outcome_reason='deletion'
		WHERE artifact_id=$1 AND state IN ('queued','leased')`, id); err != nil {
		return Artifact{}, fmt.Errorf("fence security attempts: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM artifact.search_documents WHERE artifact_id=$1`, id); err != nil {
		return Artifact{}, fmt.Errorf("remove search projection: %w", err)
	}
	a, err := scanArtifact(tx.QueryRow(ctx, `
		UPDATE artifact.artifacts AS a SET storage_state='deleting',
			withdrawn_at=CASE WHEN a.registered_at IS NOT NULL THEN COALESCE(a.withdrawn_at, now()) END,
			withdrawn_reason=CASE WHEN a.registered_at IS NOT NULL THEN $2 END,
			deletion_reason=$2,deletion_started_at=now(),deletion_error=NULL,
			deletion_actor_identity_id=$3,deletion_api_key_id=$4
		WHERE a.id=$1
		RETURNING `+artifactColumns, id, req.Reason, nullable(req.ActorID), nullable(req.APIKeyID)))
	if err != nil {
		return Artifact{}, fmt.Errorf("enter deleting: %w", err)
	}
	if err := appendEvent(ctx, tx, id, "deletion_started", req.ActorID, req.Reason, nil); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// RequestSecurityRetry creates one new three-attempt trigger for an artifact
// whose previous trigger exhausted its transient attempts. That is either a
// first check over the quarantined original or a security-advisory re-scan
// over the retained stored original; the advisory withdrawal stays in place
// until a successful verdict re-registers the artifact.
func (s *Store) RequestSecurityRetry(ctx context.Context, id, actorID, sessionID string) (Artifact, error) {
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
	if a.SecurityState != SecurityRetryableFailed || a.Digest == nil {
		return Artifact{}, ErrNotRetryable
	}
	zone := ""
	switch {
	case a.StorageState == StorageQuarantined:
		zone = "quarantine"
	case a.StorageState == StorageStored && a.WithdrawnAt != nil:
		zone = "original"
	default:
		return Artifact{}, ErrNotRetryable
	}
	var objectID string
	if err := tx.QueryRow(ctx, `
		SELECT id::text FROM artifact.objects
		WHERE artifact_id=$1 AND zone=$2 AND kind='original' AND state='committed'`, id, zone).Scan(&objectID); err != nil {
		return Artifact{}, ErrNotRetryable
	}
	a, err = scanArtifact(tx.QueryRow(ctx, `
		UPDATE artifact.artifacts AS a SET security_state='pending',security_reason=NULL
		WHERE a.id=$1 RETURNING `+artifactColumns, id))
	if err != nil {
		return Artifact{}, err
	}
	if err := queueSecurityAttemptTx(ctx, tx, id, objectID, *a.Digest, "manual_retry", actorID, sessionID); err != nil {
		return Artifact{}, err
	}
	if err := appendEvent(ctx, tx, id, "security_retry_requested", actorID, "", nil); err != nil {
		return Artifact{}, err
	}
	return a, tx.Commit(ctx)
}
