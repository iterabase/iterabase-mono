package artifacttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanproto"
)

// Verdict scripts one fake scan outcome.
type Verdict struct {
	Malware       bool
	Reject        string // admission reason; empty accepts
	CanonicalMIME string // defaults to the claimed type
	Transient     bool   // scanner unavailable
	Children      []Child
}

// Child is one scripted container child.
type Child struct {
	Kind, Filename, ClaimedMIME string
	Data, SourceHTML            []byte
}

// Scanner is a scripted artifact.Scanner that speaks the real wire format,
// so the coordinator's frame parsing and validation are exercised.
type Scanner struct {
	mu sync.Mutex
	// Decide returns the verdict for one request; nil accepts everything as
	// its claimed type.
	Decide func(req artifactstore.ScanRequest, data []byte) Verdict
	Calls  int
}

var _ artifactstore.Scanner = (*Scanner)(nil)

func (s *Scanner) Scan(_ context.Context, req artifactstore.ScanRequest, body io.Reader) (*artifactstore.ScanResponse, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.Calls++
	decide := s.Decide
	s.mu.Unlock()
	var v Verdict
	if decide != nil {
		v = decide(req, data)
	}
	if v.Transient {
		return nil, &artifactstore.TransientError{Reason: "scanner_unavailable", Err: errors.New("scripted outage")}
	}
	out := scanproto.Verdict{Version: scanproto.Version, RequestID: req.RequestID, Digest: req.Digest, Size: req.Size,
		Malware: scanproto.MalwareClean, EngineVersion: "ClamAV 1.4.6/test", SignatureVersion: "test-1",
		PolicyVersion: "test", Accepted: true, CanonicalMIME: v.CanonicalMIME, Children: len(v.Children)}
	switch {
	case v.Malware:
		out.Malware, out.Accepted, out.CanonicalMIME, out.Children, out.Signature = scanproto.MalwareInfected, false, "", 0, "Test.Malware"
		out.Reason = "malware"
		v.Children = nil
	case v.Reject != "":
		out.Accepted, out.CanonicalMIME, out.Reason, out.Children = false, "", v.Reject, 0
		v.Children = nil
	case out.CanonicalMIME == "":
		base, _, _ := mime.ParseMediaType(req.ClaimedMIME)
		if base == "" {
			base = "text/plain"
		}
		out.CanonicalMIME = strings.ToLower(base)
	}
	var buf bytes.Buffer
	if err := scanproto.WriteFrame(&buf, out); err != nil {
		return nil, err
	}
	for i, c := range v.Children {
		sum := sha256.Sum256(c.Data)
		h := scanproto.ChildHeader{Index: i, Kind: c.Kind, Filename: c.Filename, ClaimedMIME: c.ClaimedMIME,
			Size: int64(len(c.Data)), Digest: "sha256:" + hex.EncodeToString(sum[:]), SourceHTMLSize: int64(len(c.SourceHTML))}
		if err := scanproto.WriteFrame(&buf, h); err != nil {
			return nil, err
		}
		buf.Write(c.Data)
		buf.Write(c.SourceHTML)
	}
	return artifactstore.NewScanResponse(io.NopCloser(&buf), req)
}

// Drain runs a coordinator until no security attempt is due. Automatic
// retries are scheduled in the future, so a transient outcome ends the drain.
func Drain(ctx context.Context, coord *artifactstore.Coordinator) error {
	for i := 0; i < 1000; i++ {
		worked, err := coord.ProcessOne(ctx)
		if err != nil {
			return err
		}
		if !worked {
			return nil
		}
	}
	return errors.New("artifacttest: coordinator did not settle")
}

// NewCoordinator builds a coordinator over the given store and objects.
func NewCoordinator(pool *pgxpool.Pool, objects artifactstore.LifecycleStore, scanner artifactstore.Scanner) *artifactstore.Coordinator {
	return artifactstore.NewCoordinator(artifactstore.NewStore(pool), objects, scanner,
		artifactstore.CoordinatorConfig{Owner: "test", LeaseTTL: time.Minute, ScanTimeout: 10 * time.Second}, nil, nil)
}

// AdmitCleared admits bytes and drives them to clearance with an accept-all
// scanner: the common precondition for consumers of cleared artifacts.
func AdmitCleared(ctx context.Context, pool *pgxpool.Pool, svc *artifactstore.Service, objects artifactstore.LifecycleStore, in artifactstore.AdmitInput, body []byte) (artifactstore.Artifact, error) {
	a, err := svc.Admit(ctx, in, bytes.NewReader(body))
	if err != nil {
		return artifactstore.Artifact{}, err
	}
	if err := Drain(ctx, NewCoordinator(pool, objects, &Scanner{})); err != nil {
		return artifactstore.Artifact{}, err
	}
	return svc.Stat(ctx, a.ID)
}
