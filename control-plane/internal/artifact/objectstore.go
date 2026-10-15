package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Object zones (DES-HOR-460-02). Each zone is a key prefix in the artifact
// bucket whose access is granted per credential by the MinIO policies the
// chart provisions; object keys are unique and never overwritten.
const (
	ZoneQuarantine = "quarantine"
	ZoneOriginal   = "original"
	ZoneDerived    = "derived"
)

// zonePrefix maps a zone to its bucket prefix.
func zonePrefix(zone string) string {
	switch zone {
	case ZoneQuarantine:
		return "quarantine/"
	case ZoneOriginal:
		return "originals/"
	default:
		return "derived/"
	}
}

// ObjectKey is the unique, opaque storage key for one physical object. It
// carries no customer-controlled segment.
func ObjectKey(zone, artifactID, objectID string) string {
	return zonePrefix(zone) + artifactID + "/" + objectID
}

// IngestStore is the quarantine write-only credential held by the API and
// gateway. It can create new quarantine objects and nothing else: it cannot
// read, list, overwrite, or delete any zone.
type IngestStore interface {
	PutNew(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Ready(ctx context.Context) error
}

// ServeStore is the cleared-read credential held by the API and gateway. It
// reads original and derived zones only; the database decides whether a read
// is allowed.
type ServeStore interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// LifecycleStore is the coordinator-only credential: it reads quarantine,
// promotes into cleared zones, and purges every zone.
type LifecycleStore interface {
	IngestStore
	ServeStore
	Copy(ctx context.Context, srcKey, dstKey string) error
	Delete(ctx context.Context, key string) error
}

// ErrObjectExists reports that a create-only write found an existing key.
var ErrObjectExists = errors.New("artifact: object already exists")

// MinIOConfig configures one zone credential against the artifact bucket.
type MinIOConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Secure    bool
}

type minioStore struct {
	client *minio.Client
	bucket string
}

// NewMinIOStore returns a store bound to one credential. The credential's
// MinIO policy, not this type, limits which methods succeed.
func NewMinIOStore(cfg MinIOConfig) (LifecycleStore, error) {
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("artifact minio endpoint, access key, secret key, and bucket are required")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}
	return &minioStore{client: client, bucket: cfg.Bucket}, nil
}

// Ready proves the endpoint answers without needing list or read rights,
// which the ingest credential deliberately lacks.
func (m *minioStore) Ready(ctx context.Context) error {
	if _, err := m.client.GetBucketLocation(ctx, m.bucket); err != nil {
		return fmt.Errorf("check artifact bucket: %w", err)
	}
	return nil
}

// uploadPartSize exceeds MaxSize+1, so minio-go sends every V2 artifact as one
// conditional PUT (a stream that ends inside the first part is never split
// into a multipart upload). One in-flight upload buffers at most this much
// (HOR-602 sizing); the API bounds concurrent uploads.
const uploadPartSize = 32 << 20

func putOptions(contentType string) minio.PutObjectOptions {
	opts := minio.PutObjectOptions{ContentType: contentType, PartSize: uploadPartSize}
	// Create-only: the server refuses to overwrite an existing key, so the
	// write-only credential needs no read right to keep objects immutable.
	opts.SetMatchETagExcept("*")
	return opts
}

func (m *minioStore) PutNew(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if _, err := m.client.PutObject(ctx, m.bucket, key, r, size, putOptions(contentType)); err != nil {
		if minio.ToErrorResponse(err).StatusCode == 412 {
			return fmt.Errorf("%w: %s", ErrObjectExists, key)
		}
		return fmt.Errorf("put artifact object: %w", err)
	}
	return nil
}

func (m *minioStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get artifact object: %w", err)
	}
	// GetObject is lazy; force the first request now so authorization/not-found
	// errors are returned before a response status is committed.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("stat artifact object: %w", err)
	}
	return obj, nil
}

func (m *minioStore) Copy(ctx context.Context, srcKey, dstKey string) error {
	dst := minio.CopyDestOptions{Bucket: m.bucket, Object: dstKey}
	src := minio.CopySrcOptions{Bucket: m.bucket, Object: srcKey}
	if _, err := m.client.CopyObject(ctx, dst, src); err != nil {
		return fmt.Errorf("copy artifact object: %w", err)
	}
	return nil
}

func (m *minioStore) Delete(ctx context.Context, key string) error {
	if err := m.client.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete artifact object: %w", err)
	}
	return nil
}
