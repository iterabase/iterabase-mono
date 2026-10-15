package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission"
)

// Config bounds admission. A zero default retention means indefinite
// retention for cleared artifacts (DES-HOR-460-12).
type Config struct {
	DefaultRetention time.Duration
	// QuarantineMaxBytes is the committed quarantine backlog above which new
	// uploads fail before any byte is accepted (section 11).
	QuarantineMaxBytes int64
	// MaxConcurrentUploads bounds the uploads one process streams at once.
	MaxConcurrentUploads int
}

func (c Config) defaults() Config {
	if c.QuarantineMaxBytes == 0 {
		c.QuarantineMaxBytes = 2 << 30
	}
	if c.MaxConcurrentUploads == 0 {
		c.MaxConcurrentUploads = 4
	}
	return c
}

// Service is the API/gateway side of the artifact domain. It holds only the
// ingest (quarantine write-only) and serve (cleared read-only) credentials;
// scanning, promotion, purge, and retention belong to the Coordinator.
type Service struct {
	store   *Store
	ingest  IngestStore
	serve   ServeStore
	cfg     Config
	log     *slog.Logger
	uploads chan struct{}
}

func NewService(store *Store, ingest IngestStore, serve ServeStore, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.defaults()
	return &Service{store: store, ingest: ingest, serve: serve, cfg: cfg, log: log,
		uploads: make(chan struct{}, cfg.MaxConcurrentUploads)}
}

func (s *Service) Ready(ctx context.Context) error { return s.ingest.Ready(ctx) }

// Store exposes authorization queries to the gateway adapter; byte operations
// remain on Service so callers cannot bypass lifecycle checks.
func (s *Service) Store() *Store { return s.store }

// Admit streams one byte source into quarantine. It returns the committed,
// still-unusable artifact; nothing is shared, downloadable, or consumable
// until the coordinator clears it. A replay of the same idempotency key with
// the same payload returns the existing artifact; a changed payload conflicts.
func (s *Service) Admit(ctx context.Context, in AdmitInput, body io.Reader) (Artifact, error) {
	name, err := displayName(in.DisplayName)
	if err != nil {
		return Artifact{}, err
	}
	in.DisplayName = name
	if err := validateAdmit(in); err != nil {
		return Artifact{}, err
	}
	if body == nil {
		return Artifact{}, fmt.Errorf("%w: body is required", ErrInvalidInput)
	}
	if in.ExpectedSize != nil && *in.ExpectedSize > MaxSize {
		return Artifact{}, ErrTooLarge
	}
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	default:
		return Artifact{}, ErrCapacity
	}
	backlog, err := s.store.QuarantineBytes(ctx)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrCapacity, err)
	}
	if backlog >= s.cfg.QuarantineMaxBytes {
		return Artifact{}, ErrCapacity
	}
	if in.RetentionUntil == nil && s.cfg.DefaultRetention > 0 {
		t := time.Now().Add(s.cfg.DefaultRetention)
		in.RetentionUntil = &t
	}

	rec := uploadingRecord{ArtifactID: uuid.NewString(), ObjectID: uuid.NewString(), Fingerprint: fingerprint(in)}
	rec.Key = ObjectKey(ZoneQuarantine, rec.ArtifactID, rec.ObjectID)
	pending, err := s.store.CreateUploading(ctx, rec, in)
	if errors.Is(err, ErrConflict) {
		return s.replay(ctx, pending, rec.Fingerprint, in, body)
	}
	if err != nil {
		return Artifact{}, err
	}

	size, digest, err := s.stream(ctx, rec.Key, in, body)
	if err != nil {
		// The uploading row is the reconciliation handle: this process cannot
		// read or delete quarantine, so the coordinator purges the object.
		return Artifact{}, err
	}
	return s.store.CommitQuarantine(ctx, pending.ID, rec.ObjectID, size, digest, in.Scope)
}

// stream writes the bounded body once into a create-only quarantine object
// while hashing it, then verifies the caller's integrity claims.
func (s *Service) stream(ctx context.Context, key string, in AdmitInput, body io.Reader) (int64, string, error) {
	h := sha256.New()
	counter := &countWriter{}
	limited := &io.LimitedReader{R: body, N: MaxSize + 1}
	tee := io.TeeReader(limited, io.MultiWriter(h, counter))
	size := int64(-1)
	if in.ExpectedSize != nil {
		size = *in.ExpectedSize
	}
	contentType := in.ClaimedMIME
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := s.ingest.PutNew(ctx, key, tee, size, contentType); err != nil {
		if counter.n > MaxSize {
			return 0, "", ErrTooLarge
		}
		return 0, "", err
	}
	if counter.n > MaxSize {
		return 0, "", ErrTooLarge
	}
	if counter.n == 0 {
		return 0, "", fmt.Errorf("%w: empty file", ErrInvalidInput)
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if in.ExpectedSize != nil && *in.ExpectedSize != counter.n {
		return 0, "", fmt.Errorf("%w: expected %d, got %d", ErrSize, *in.ExpectedSize, counter.n)
	}
	if in.ExpectedDigest != "" && !strings.EqualFold(in.ExpectedDigest, digest) {
		return 0, "", fmt.Errorf("%w: expected %s, got %s", ErrDigest, in.ExpectedDigest, digest)
	}
	return counter.n, digest, nil
}

// replay resolves an idempotency-key collision. It hashes the new body
// without storing it: the same committed payload returns the existing handle.
func (s *Service) replay(ctx context.Context, existing Artifact, fp string, in AdmitInput, body io.Reader) (Artifact, error) {
	stored, err := s.store.Fingerprint(ctx, existing.ID)
	if err != nil {
		return Artifact{}, err
	}
	if stored != fp {
		return Artifact{}, ErrConflict
	}
	if existing.StorageState == StorageUploading {
		return Artifact{}, ErrUploadInFlight
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(body, MaxSize+1))
	if err != nil {
		return Artifact{}, err
	}
	if existing.Digest == nil || existing.SizeBytes == nil ||
		*existing.Digest != "sha256:"+hex.EncodeToString(h.Sum(nil)) || *existing.SizeBytes != n {
		return Artifact{}, ErrConflict
	}
	return existing, nil
}

// displayName is the metadata-boundary form of a claimed filename (section 6):
// path segments and controls are removed, and bidi overrides or executable
// double extensions are rejected, so only the normalized name ever reaches
// shared search, status, or download metadata. It is the same normalization
// the admission adapter validates, applied for every source.
func displayName(claimed string) (string, error) {
	if claimed == "" {
		return "", nil // validateAdmit reports the missing name
	}
	name, ok := admission.NormalizeFilename(claimed)
	if !ok {
		return "", fmt.Errorf("%w: unsafe filename", ErrInvalidInput)
	}
	return name, nil
}

// fingerprint binds an idempotency key to the request metadata.
func fingerprint(in AdmitInput) string {
	h := sha256.New()
	for _, part := range []string{in.SourceType, in.SourceRef, in.DisplayName, in.ClaimedMIME, strings.ToLower(in.ExpectedDigest)} {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Status resolves a provisional upload handle or artifact id.
func (s *Service) Status(ctx context.Context, handle string) (Artifact, error) {
	return s.store.GetByHandle(ctx, handle)
}

// Open returns the bytes of a currently cleared original. Clearance is
// re-checked in the same query that resolves the object.
func (s *Service) Open(ctx context.Context, id string) (Artifact, io.ReadCloser, error) {
	a, key, err := s.store.ClearedOriginal(ctx, id)
	if err != nil {
		return Artifact{}, nil, err
	}
	r, err := s.serve.Get(ctx, key)
	if err != nil {
		return Artifact{}, nil, err
	}
	return a, r, nil
}

// Stat returns metadata for an id the caller is already authorized to see.
func (s *Service) Stat(ctx context.Context, id string) (Artifact, error) {
	return s.store.Get(ctx, id)
}

// List returns registered, currently cleared artifacts.
func (s *Service) List(ctx context.Context, q ListQuery) ([]ListItem, error) {
	return s.store.List(ctx, q)
}

// RequestDelete starts the attributable deletion lifecycle. Byte purge is
// asynchronous and owned by the coordinator.
func (s *Service) RequestDelete(ctx context.Context, req DeleteRequest) (Artifact, error) {
	if req.Reason == "" {
		req.Reason = DeletionAdmin
	}
	return s.store.BeginDelete(ctx, req)
}

// RetrySecurity creates one new security trigger after transient failure.
func (s *Service) RetrySecurity(ctx context.Context, id, actorID, sessionID string) (Artifact, error) {
	return s.store.RequestSecurityRetry(ctx, id, actorID, sessionID)
}

//nolint:gocyclo // Fail-closed admission validation enumerates each caller-supplied field.
func validateAdmit(in AdmitInput) error {
	switch in.SourceType {
	case SourceUpload, SourceChat, SourceTeams, SourceMailbox, SourceRunner, SourceTool, SourceWorkflow, SourceSandbox:
	default:
		return fmt.Errorf("%w: invalid source type", ErrInvalidInput)
	}
	if in.Attribution.RequestActorID == "" {
		return fmt.Errorf("%w: request actor is required", ErrInvalidInput)
	}
	if in.ContainerDepth < 0 || in.ContainerDepth > 1 {
		return fmt.Errorf("%w: invalid container depth", ErrInvalidInput)
	}
	if in.DisplayName == "" || len(in.DisplayName) > 255 || !utf8.ValidString(in.DisplayName) {
		return fmt.Errorf("%w: a display filename of at most 255 bytes is required", ErrInvalidInput)
	}
	if len(in.IdempotencyKey) > 128 {
		return fmt.Errorf("%w: idempotency key exceeds 128 bytes", ErrInvalidInput)
	}
	if in.ClaimedMIME != "" {
		if _, _, err := mime.ParseMediaType(in.ClaimedMIME); err != nil {
			return fmt.Errorf("%w: invalid content type", ErrInvalidInput)
		}
	}
	if in.ExpectedSize != nil && *in.ExpectedSize <= 0 {
		return fmt.Errorf("%w: a non-empty body is required", ErrInvalidInput)
	}
	if in.ExpectedDigest != "" {
		if len(in.ExpectedDigest) != len("sha256:")+64 || !strings.HasPrefix(strings.ToLower(in.ExpectedDigest), "sha256:") {
			return fmt.Errorf("%w: expected digest must be sha256:<hex>", ErrInvalidInput)
		}
		if _, err := hex.DecodeString(in.ExpectedDigest[len("sha256:"):]); err != nil {
			return fmt.Errorf("%w: malformed expected digest", ErrInvalidInput)
		}
	}
	return nil
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
