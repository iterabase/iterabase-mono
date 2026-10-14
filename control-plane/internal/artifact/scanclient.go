package artifact

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"golang.org/x/net/http2"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanproto"
)

// ScanRequest is everything the isolated adapter may learn about one stream:
// no artifact id, owner, credential, storage key, or work context.
type ScanRequest struct {
	RequestID      string
	Digest         string
	Size           int64
	ClaimedMIME    string
	Filename       string
	Generated      bool
	ContainerDepth int
}

// Scanner sends one bounded stream to the scanner adapter.
type Scanner interface {
	Scan(ctx context.Context, req ScanRequest, body io.Reader) (*ScanResponse, error)
}

// TransientError is a retryable processing failure. It never carries a
// verdict, so it can never clear bytes.
type TransientError struct {
	Reason string
	Err    error
}

func (e *TransientError) Error() string {
	return "artifact: transient " + e.Reason + ": " + e.Err.Error()
}
func (e *TransientError) Unwrap() error { return e.Err }

func transient(reason string, err error) error { return &TransientError{Reason: reason, Err: err} }

// ScanResponse is a validated verdict followed by its child frames.
type ScanResponse struct {
	Verdict scanproto.Verdict
	body    io.ReadCloser
	next    int
	pending io.Reader
}

// NewScanResponse reads and validates the verdict frame of a response body.
func NewScanResponse(body io.ReadCloser, req ScanRequest) (*ScanResponse, error) {
	var v scanproto.Verdict
	if err := scanproto.ReadFrame(body, &v); err != nil {
		_ = body.Close()
		return nil, transient("protocol_error", err)
	}
	if err := v.Validate(req.RequestID, req.Digest, req.Size); err != nil {
		_ = body.Close()
		return nil, transient("protocol_error", err)
	}
	return &ScanResponse{Verdict: v, body: body}, nil
}

// NextChild returns the next child header and readers for its bytes and HTML
// source. Callers must consume both readers fully before the next call.
func (r *ScanResponse) NextChild() (scanproto.ChildHeader, io.Reader, io.Reader, error) {
	if r.pending != nil {
		if _, err := io.Copy(io.Discard, r.pending); err != nil {
			return scanproto.ChildHeader{}, nil, nil, transient("protocol_error", err)
		}
	}
	if r.next >= r.Verdict.Children {
		return scanproto.ChildHeader{}, nil, nil, io.EOF
	}
	var h scanproto.ChildHeader
	if err := scanproto.ReadFrame(r.body, &h); err != nil {
		return h, nil, nil, transient("protocol_error", err)
	}
	if err := h.Validate(r.next); err != nil {
		return h, nil, nil, transient("protocol_error", err)
	}
	r.next++
	data := io.LimitReader(r.body, h.Size)
	html := io.LimitReader(r.body, h.SourceHTMLSize)
	r.pending = io.MultiReader(data, html)
	return h, data, html, nil
}

func (r *ScanResponse) Close() error { return r.body.Close() }

// MTLSScannerConfig is the coordinator's exact scanner identity contract.
type MTLSScannerConfig struct {
	URL            string
	ServerSPIFFEID string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
}

type mtlsScanner struct {
	url    string
	client *http.Client
}

// NewMTLSScanner builds an HTTP/2-only mTLS client that presents the
// coordinator identity (re-read on every handshake so cert-manager rotation
// needs no restart) and accepts only the exact scanner SPIFFE identity.
func NewMTLSScanner(cfg MTLSScannerConfig) (Scanner, error) {
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read scanner CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("scanner CA bundle contains no certificate")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("scanner URL must be https://host[:port]")
	}
	want := cfg.ServerSPIFFEID
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2"},
		RootCAs:    roots,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
			return &cert, err
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("scanner presented no certificate")
			}
			for _, uri := range cs.PeerCertificates[0].URIs {
				if uri.String() == want {
					return nil
				}
			}
			return fmt.Errorf("scanner identity is not %s", want)
		},
	}
	return &mtlsScanner{url: strings.TrimRight(cfg.URL, "/") + scanproto.Path,
		client: &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}}, nil
}

func (s *mtlsScanner) Scan(ctx context.Context, req ScanRequest, body io.Reader) (*ScanResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, body)
	if err != nil {
		return nil, transient("protocol_error", err)
	}
	httpReq.ContentLength = req.Size
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	httpReq.Header.Set(scanproto.HeaderVersion, scanproto.Version)
	httpReq.Header.Set(scanproto.HeaderRequestID, req.RequestID)
	httpReq.Header.Set(scanproto.HeaderDigest, req.Digest)
	httpReq.Header.Set(scanproto.HeaderSize, strconv.FormatInt(req.Size, 10))
	httpReq.Header.Set(scanproto.HeaderClaimedMIME, req.ClaimedMIME)
	httpReq.Header.Set(scanproto.HeaderFilename, url.QueryEscape(req.Filename))
	httpReq.Header.Set(scanproto.HeaderGenerated, strconv.FormatBool(req.Generated))
	httpReq.Header.Set(scanproto.HeaderContainerDepth, strconv.Itoa(req.ContainerDepth))
	resp, err := s.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, transient("scanner_timeout", err)
		}
		return nil, transient("scanner_unavailable", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			return nil, transient("scanner_unavailable", fmt.Errorf("scanner not ready"))
		}
		return nil, transient("protocol_error", fmt.Errorf("scanner status %d", resp.StatusCode))
	}
	return NewScanResponse(resp.Body, req)
}
