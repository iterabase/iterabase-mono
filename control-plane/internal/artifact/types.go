// Package artifact implements the V2 shared artifact domain
// (docs/architecture/v2-artifact-processing.md): one quarantine path for every
// source, independent storage/security/extraction state, delayed shared
// registration, PostgreSQL search, and a controlled deletion lifecycle that
// keeps only a content-free tombstone.
//
// PostgreSQL is the only authority for whether bytes may be used
// (`artifact.is_cleared`); object presence in MinIO never proves anything.
// Customer and workload processes hold only the ingest (quarantine
// write-only) and serve (cleared read-only) object credentials. The
// coordinator alone holds the lifecycle credential and drives scanning,
// promotion, purge, and retention.
package artifact

import (
	"errors"
	"time"
)

var (
	ErrNotFound       = errors.New("artifact: not found")
	ErrNotAvailable   = errors.New("artifact: not available")
	ErrUnauthorized   = errors.New("artifact: unauthorized")
	ErrInvalidInput   = errors.New("artifact: invalid input")
	ErrDigest         = errors.New("artifact: digest mismatch")
	ErrSize           = errors.New("artifact: size mismatch")
	ErrTooLarge       = errors.New("artifact: exceeds maximum size")
	ErrConflict       = errors.New("artifact: idempotency conflict")
	ErrInUse          = errors.New("artifact: in use by active work")
	ErrNotRetryable   = errors.New("artifact: not retryable")
	ErrCapacity       = errors.New("artifact: processing capacity unavailable")
	ErrUploadInFlight = errors.New("artifact: upload in progress")
	ErrLeaseLost      = errors.New("artifact: processing lease lost")
)

// MaxSize is the V2 per-file limit: exactly 25 MiB (DES-HOR-460-04).
const MaxSize int64 = 26_214_400

// Storage lifecycle (section 7).
const (
	StorageUploading   = "uploading"
	StorageQuarantined = "quarantined"
	StoragePromoting   = "promoting"
	StorageStored      = "stored"
	StorageDeleting    = "deleting"
	StorageDeleted     = "deleted"
)

// Security lifecycle (section 7). Malware scan and type validation run in one
// isolated adapter call (DES-HOR-461-01), so `scanning` covers both checks.
const (
	SecurityPending         = "pending"
	SecurityScanning        = "scanning"
	SecurityCleared         = "cleared"
	SecurityRejected        = "rejected"
	SecurityRetryableFailed = "retryable_failed"
	SecurityRescanPending   = "rescan_pending"
)

// Extraction lifecycle. HOR-462 owns the extractor; clearance only makes an
// artifact eligible.
const (
	ExtractionNotEligible     = "not_eligible"
	ExtractionPending         = "pending"
	ExtractionReady           = "ready"
	ExtractionFailed          = "failed"
	ExtractionRetryableFailed = "retryable_failed"
)

// Sources (section 8.1). Every source enters the same quarantine path.
const (
	SourceUpload   = "upload"
	SourceChat     = "chat"
	SourceTeams    = "teams"
	SourceMailbox  = "mailbox"
	SourceRunner   = "runner"
	SourceTool     = "tool"
	SourceWorkflow = "workflow"
	SourceSandbox  = "sandbox"
)

// Authorization sources recorded as attribution evidence.
const (
	AuthBrowser  = "browser"
	AuthAPI      = "api"
	AuthChat     = "chat"
	AuthWorkload = "workload"
	AuthSystem   = "system"
)

// Container children (DES-HOR-461-03).
const (
	ChildBody       = "body"
	ChildAttachment = "attachment"
)

// Customer projection codes (section 7). These are stable semantic codes; the
// app localizes them and never shows engine, pod, or storage detail.
const (
	StatusCheckingSecurity        = "checking_security"
	StatusSecurityCheckFailed     = "security_check_failed_retryable"
	StatusRejected                = "rejected"
	StatusProcessingContent       = "processing_content"
	StatusSafeContentNotExtracted = "safe_content_not_extracted"
	StatusReady                   = "ready"
	StatusUnavailable             = "unavailable"
	StatusDeleted                 = "deleted"
)

// Deletion reasons recorded on the tombstone.
const (
	DeletionAdmin            = "admin_explicit"
	DeletionRetention        = "retention_expired"
	DeletionSecurityRejected = "security_rejected"
	DeletionUnscannedExpired = "unscanned_expired"
	DeletionIncompleteUpload = "incomplete_upload"
)

// Attribution is the authenticated boundary that admitted the bytes. Callers
// never supply it; the API/gateway derive it from the verified credential.
type Attribution struct {
	InitiatingHumanID string
	RequestActorID    string
	ExecutingID       string
	Source            string // AuthBrowser | AuthAPI | AuthChat | AuthWorkload | AuthSystem
	SessionID         string
	APIKeyID          string
}

// Artifact is the authoritative metadata row. Deleted artifacts keep only the
// tombstone fields (section 8.8).
type Artifact struct {
	ID                string
	UploadID          string
	SourceType        string
	SourceRef         *string
	ParentID          *string
	ChildIndex        *int
	ChildKind         *string
	ContainerDepth    int
	CreatedByID       string
	InitiatingHumanID *string
	APIKeyID          *string
	DisplayName       *string
	ClaimedMIME       *string
	CanonicalMIME     *string
	SizeBytes         *int64
	Digest            *string
	StorageState      string
	SecurityState     string
	SecurityReason    *string
	ExtractionState   string
	RegisteredAt      *time.Time
	WithdrawnAt       *time.Time
	RetentionUntil    *time.Time
	DeletionReason    *string
	CreatedAt         time.Time
	QuarantinedAt     *time.Time
	ClearedAt         *time.Time
	DeletedAt         *time.Time
	RowVersion        int64
}

// Cleared is the in-memory form of artifact.is_cleared. Every byte use
// re-checks it in SQL immediately before the use; this is only a projection.
func (a Artifact) Cleared() bool {
	return a.StorageState == StorageStored && a.SecurityState == SecurityCleared &&
		a.RegisteredAt != nil && a.WithdrawnAt == nil
}

// Registered reports current shared-library visibility.
func (a Artifact) Registered() bool { return a.RegisteredAt != nil && a.WithdrawnAt == nil }

// Status is the customer projection of the independent states.
func (a Artifact) Status() string {
	switch {
	case a.StorageState == StorageDeleted:
		if a.SecurityState == SecurityRejected {
			return StatusRejected
		}
		return StatusDeleted
	case a.SecurityState == SecurityRejected:
		return StatusRejected
	case a.StorageState == StorageDeleting:
		return StatusUnavailable
	case a.SecurityState == SecurityRetryableFailed:
		return StatusSecurityCheckFailed
	case a.SecurityState == SecurityRescanPending || a.WithdrawnAt != nil:
		return StatusUnavailable
	case a.SecurityState != SecurityCleared:
		return StatusCheckingSecurity
	case a.ExtractionState == ExtractionReady:
		return StatusReady
	case a.ExtractionState == ExtractionFailed || a.ExtractionState == ExtractionRetryableFailed:
		return StatusSafeContentNotExtracted
	default:
		return StatusProcessingContent
	}
}

// MIMEType is the canonical type once validated, otherwise the advisory claim.
func (a Artifact) MIMEType() string {
	if a.CanonicalMIME != nil {
		return *a.CanonicalMIME
	}
	if a.ClaimedMIME != nil && *a.ClaimedMIME != "" {
		return *a.ClaimedMIME
	}
	return "application/octet-stream"
}

// Ref is the canonical wire reference to the immutable original. It never
// asserts availability: every consuming boundary re-checks clearance.
type Ref struct {
	ArtifactID string `json:"artifactId"`
	MIMEType   string `json:"mimeType"`
	SizeBytes  int64  `json:"sizeBytes"`
	Digest     string `json:"digest"`
}

// Ref returns the reference once integrity is committed (quarantined or
// later). It is the same identity before and after clearance.
func (a Artifact) Ref() (Ref, error) {
	if a.SizeBytes == nil || a.Digest == nil || a.StorageState == StorageUploading {
		return Ref{}, ErrNotAvailable
	}
	return Ref{ArtifactID: a.ID, MIMEType: a.MIMEType(), SizeBytes: *a.SizeBytes, Digest: *a.Digest}, nil
}

// Scope is an immutable relationship to durable customer work. Run and attempt
// share one UUID in v1, so only AttemptID is stored.
type Scope struct {
	WorkItemID      string
	AttemptID       string
	NodeExecutionID string
	Role            string
}

// AdmitInput is trusted, authorization-derived metadata plus optional caller
// integrity claims for one byte stream entering quarantine.
type AdmitInput struct {
	SourceType     string
	SourceRef      string
	Attribution    Attribution
	DisplayName    string
	ClaimedMIME    string
	ExpectedSize   *int64
	ExpectedDigest string
	IdempotencyKey string
	RetentionUntil *time.Time
	Scope          *Scope
	// ContainerDepth is 0 for a received mailbox message root and 1 for every
	// other directly admitted artifact (DES-HOR-461-03).
	ContainerDepth int
}
