package scanner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanproto"
	cpmetrics "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/metrics"
)

// PolicyVersion identifies the admission policy generation recorded on every
// security attempt. Change it whenever admission rules change.
const PolicyVersion = "hor-461.1"

// Engine is the malware engine boundary (clamd in production).
type Engine interface {
	Ping(ctx context.Context) error
	Version(ctx context.Context) (string, error)
	Reload(ctx context.Context) error
	Scan(ctx context.Context, data []byte) (infected bool, signature string, err error)
}

// Adapter serves the one processing endpoint and tracks readiness.
type Adapter struct {
	Engine        Engine
	StatePath     string
	AllowedClient string // exact coordinator SPIFFE id
	Metrics       *cpmetrics.Metrics
	Log           *slog.Logger
	Now           func() time.Time

	mu         sync.RWMutex
	ready      bool
	engineVer  string
	state      SignatureState
	loadedVer  string
	inFlight   chan struct{}
	initOnce   sync.Once
	readyCause string
}

func (a *Adapter) init() {
	a.initOnce.Do(func() {
		a.inFlight = make(chan struct{}, 1)
		if a.Now == nil {
			a.Now = time.Now
		}
		if a.Log == nil {
			a.Log = slog.Default()
		}
	})
}

// Refresh re-reads the signature state, reloads clamd when the validated
// bundle changed, and proves detection with the self-test before declaring
// readiness. Any failure makes the adapter unready (fail closed).
func (a *Adapter) Refresh(ctx context.Context) {
	a.init()
	ready, cause := a.refresh(ctx)
	a.mu.Lock()
	a.ready, a.readyCause = ready, cause
	age := a.Now().Sub(a.state.CheckedAt)
	a.mu.Unlock()
	if a.Metrics != nil {
		a.Metrics.ArtifactScannerReady.WithLabelValues().Set(boolGauge(ready))
		if !a.state.CheckedAt.IsZero() {
			a.Metrics.ArtifactSignatureAge.WithLabelValues().Set(age.Seconds())
		}
	}
	if !ready {
		a.Log.Warn("artifact scanner unready", "cause", cause)
	}
}

func (a *Adapter) refresh(ctx context.Context) (bool, string) {
	st, err := ReadSignatureState(a.StatePath)
	if err != nil {
		return false, "signatures_unavailable"
	}
	a.mu.Lock()
	a.state = st
	loaded := a.loadedVer
	a.mu.Unlock()
	if st.Age(a.Now()) > MaxSignatureAge {
		return false, "signatures_stale"
	}
	if loaded != "" && loaded != st.Version {
		if err := a.Engine.Reload(ctx); err != nil {
			return false, "engine_reload_failed"
		}
	}
	if err := a.waitEngine(ctx); err != nil {
		return false, "engine_unavailable"
	}
	ver, err := a.Engine.Version(ctx)
	if err != nil {
		return false, "engine_unavailable"
	}
	infected, _, err := a.Engine.Scan(ctx, selfTestSample())
	if err != nil || !infected {
		return false, "engine_self_test_failed"
	}
	a.mu.Lock()
	a.engineVer, a.loadedVer = ver, st.Version
	a.mu.Unlock()
	return true, ""
}

func (a *Adapter) waitEngine(ctx context.Context) error {
	var err error
	for i := 0; i < 30; i++ {
		if err = a.Engine.Ping(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}

// Ready reports whether a clean verdict may currently be issued.
func (a *Adapter) Ready() (bool, string) {
	a.init()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ready && a.state.Age(a.Now()) > MaxSignatureAge {
		return false, "signatures_stale"
	}
	return a.ready, a.readyCause
}

// ServeHTTP implements POST /v1/admit.
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.init()
	if r.Method != http.MethodPost || r.URL.Path != scanproto.Path {
		http.NotFound(w, r)
		return
	}
	if !a.authorized(r) {
		a.count("error")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Header.Get(scanproto.HeaderVersion) != scanproto.Version {
		http.Error(w, "unsupported protocol version", http.StatusBadRequest)
		return
	}
	if ok, _ := a.Ready(); !ok {
		a.count("unready")
		http.Error(w, "scanner not ready", http.StatusServiceUnavailable)
		return
	}
	select {
	case a.inFlight <- struct{}{}:
		defer func() { <-a.inFlight }()
	default:
		http.Error(w, "scanner busy", http.StatusServiceUnavailable)
		return
	}
	req, data, err := readRequest(r)
	if err != nil {
		a.count("error")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.RLock()
	engineVer, st := a.engineVer, a.state
	a.mu.RUnlock()
	infected, signature, err := a.Engine.Scan(r.Context(), data)
	if err != nil {
		a.count("error")
		http.Error(w, "engine unavailable", http.StatusServiceUnavailable)
		return
	}
	v := scanproto.Verdict{Version: scanproto.Version, RequestID: req.requestID, Digest: req.digest, Size: req.size,
		EngineVersion: engineVer, SignatureVersion: st.Version, SignatureAgeSeconds: int64(st.Age(a.Now()).Seconds()),
		PolicyVersion: PolicyVersion, Malware: scanproto.MalwareClean}
	var children []admission.Child
	switch {
	case infected:
		v.Malware, v.Signature, v.Reason = scanproto.MalwareInfected, signature, "malware"
		a.count("infected")
	default:
		res := admission.Evaluate(admission.Input{Data: data, ClaimedMIME: req.claimedMIME, Filename: req.filename,
			Generated: req.generated, ContainerDepth: req.depth})
		v.Accepted, v.CanonicalMIME, v.Reason = res.Accepted, res.CanonicalMIME, res.Reason
		if res.Accepted {
			for _, c := range res.Children {
				if len(c.Data) > 0 {
					children = append(children, c)
				}
			}
			v.Children = len(children)
			a.count("clean")
		} else {
			a.count("rejected")
		}
	}
	w.Header().Set("Content-Type", scanproto.ContentType)
	w.WriteHeader(http.StatusOK)
	if err := writeResponse(w, v, children); err != nil {
		a.Log.Warn("artifact scanner response write failed", "error", err)
	}
}

func (a *Adapter) authorized(r *http.Request) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	for _, uri := range r.TLS.PeerCertificates[0].URIs {
		if uri.String() == a.AllowedClient {
			return true
		}
	}
	return false
}

func (a *Adapter) count(verdict string) {
	if a.Metrics != nil {
		a.Metrics.ArtifactScans.WithLabelValues(verdict).Inc()
	}
}

type scanRequest struct {
	requestID   string
	digest      string
	size        int64
	claimedMIME string
	filename    string
	generated   bool
	depth       int
}

//nolint:gocyclo // Fail-closed validation enumerates each request header of the one bounded stream.
func readRequest(r *http.Request) (scanRequest, []byte, error) {
	var req scanRequest
	req.requestID = r.Header.Get(scanproto.HeaderRequestID)
	req.digest = r.Header.Get(scanproto.HeaderDigest)
	req.claimedMIME = r.Header.Get(scanproto.HeaderClaimedMIME)
	size, err := strconv.ParseInt(r.Header.Get(scanproto.HeaderSize), 10, 64)
	if err != nil || size <= 0 || size > admission.MaxFileBytes {
		return req, nil, errors.New("invalid size")
	}
	req.size = size
	if req.requestID == "" || len(req.requestID) > 128 || len(req.digest) != len("sha256:")+64 || len(req.claimedMIME) > 255 {
		return req, nil, errors.New("invalid request metadata")
	}
	if req.filename, err = url.QueryUnescape(r.Header.Get(scanproto.HeaderFilename)); err != nil || len(req.filename) > 1024 {
		return req, nil, errors.New("invalid filename")
	}
	req.generated, _ = strconv.ParseBool(r.Header.Get(scanproto.HeaderGenerated))
	if req.depth, err = strconv.Atoi(r.Header.Get(scanproto.HeaderContainerDepth)); err != nil || req.depth < 0 || req.depth > 16 {
		return req, nil, errors.New("invalid container depth")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, size+1))
	if err != nil {
		return req, nil, errors.New("read body")
	}
	if int64(len(data)) != size {
		return req, nil, errors.New("size mismatch")
	}
	if digest(data) != req.digest {
		return req, nil, errors.New("digest mismatch")
	}
	return req, data, nil
}

func writeResponse(w io.Writer, v scanproto.Verdict, children []admission.Child) error {
	if err := scanproto.WriteFrame(w, v); err != nil {
		return err
	}
	for i, c := range children {
		h := scanproto.ChildHeader{Index: i, Kind: string(c.Kind), Filename: c.Filename, ClaimedMIME: c.ClaimedMIME,
			Size: int64(len(c.Data)), Digest: digest(c.Data), SourceHTMLSize: int64(len(c.SourceHTML))}
		if h.Filename == "" {
			h.Filename = "attachment-" + strconv.Itoa(i)
		}
		if err := scanproto.WriteFrame(w, h); err != nil {
			return err
		}
		if _, err := io.Copy(w, bytes.NewReader(c.Data)); err != nil {
			return err
		}
		if _, err := w.Write(c.SourceHTML); err != nil {
			return err
		}
	}
	return nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
