// Package scanproto is the versioned wire contract between the artifact
// coordinator and the isolated scanner adapter (DES-HOR-460-07,
// DES-HOR-461-01). One HTTP/2 mTLS request carries exactly one bounded byte
// stream; the response is a bounded sequence of frames:
//
//	frame   := uint32 big-endian header length, JSON header
//	verdict := frame(Verdict)
//	child   := frame(ChildHeader), Size raw bytes, SourceHTMLSize raw bytes
//	body    := verdict child*
//
// The adapter never receives an artifact id, owner, credential, storage key,
// or workflow context, and the coordinator never trusts a frame that does not
// match its request id, digest, and size.
package scanproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version both sides must agree on.
const Version = "1"

// Path is the single processing endpoint.
const Path = "/v1/admit"

// Request headers.
const (
	HeaderVersion        = "Iterabase-Scan-Protocol"
	HeaderRequestID      = "Iterabase-Scan-Request-Id"
	HeaderDigest         = "Iterabase-Scan-Digest"
	HeaderSize           = "Iterabase-Scan-Size"
	HeaderClaimedMIME    = "Iterabase-Scan-Claimed-Mime"
	HeaderFilename       = "Iterabase-Scan-Filename" // URL query-escaped
	HeaderGenerated      = "Iterabase-Scan-Generated-Name"
	HeaderContainerDepth = "Iterabase-Scan-Container-Depth"
)

// ContentType is the response media type.
const ContentType = "application/vnd.iterabase.scan.v1"

// Malware verdicts.
const (
	MalwareClean    = "clean"
	MalwareInfected = "infected"
)

// Verdict is the first frame of every successful response.
type Verdict struct {
	Version             string `json:"version"`
	RequestID           string `json:"requestId"`
	Digest              string `json:"digest"`
	Size                int64  `json:"size"`
	Malware             string `json:"malware"`
	Signature           string `json:"signature,omitempty"`
	EngineVersion       string `json:"engineVersion"`
	SignatureVersion    string `json:"signatureVersion"`
	SignatureAgeSeconds int64  `json:"signatureAgeSeconds"`
	PolicyVersion       string `json:"policyVersion"`
	Accepted            bool   `json:"accepted"`
	CanonicalMIME       string `json:"canonicalMime,omitempty"`
	Reason              string `json:"reason,omitempty"`
	Children            int    `json:"children"`
}

// ChildHeader precedes one container child's bytes.
type ChildHeader struct {
	Index          int    `json:"index"`
	Kind           string `json:"kind"`
	Filename       string `json:"filename"`
	ClaimedMIME    string `json:"claimedMime,omitempty"`
	Size           int64  `json:"size"`
	Digest         string `json:"digest"`
	SourceHTMLSize int64  `json:"sourceHtmlSize,omitempty"`
}

// Bounds the coordinator enforces on every response.
const (
	MaxHeaderBytes   = 16 << 10
	MaxChildren      = 50
	MaxChildBytes    = 26_214_400
	MaxExpandedBytes = 256 << 20
	maxSignatureLen  = 128
)

// WriteFrame writes one length-prefixed JSON header.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxHeaderBytes {
		return errors.New("scanproto: header too large")
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b))) //nolint:gosec // len(b) <= MaxHeaderBytes, checked above.
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one bounded length-prefixed JSON header.
func ReadFrame(r io.Reader, v any) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > MaxHeaderBytes {
		return fmt.Errorf("scanproto: invalid header length %d", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Validate checks a verdict against the exact request.
//
//nolint:gocyclo // Fail-closed protocol validation enumerates every field of an untrusted frame.
func (v Verdict) Validate(requestID, digest string, size int64) error {
	switch {
	case v.Version != Version:
		return fmt.Errorf("scanproto: version %q", v.Version)
	case v.RequestID != requestID:
		return errors.New("scanproto: request id mismatch")
	case v.Digest != digest || v.Size != size:
		return errors.New("scanproto: input digest/size mismatch")
	case v.Malware != MalwareClean && v.Malware != MalwareInfected:
		return fmt.Errorf("scanproto: malware verdict %q", v.Malware)
	case v.Malware == MalwareInfected && v.Accepted:
		return errors.New("scanproto: infected input cannot be accepted")
	case v.Accepted && v.CanonicalMIME == "":
		return errors.New("scanproto: accepted verdict without canonical type")
	case !v.Accepted && v.Reason == "":
		return errors.New("scanproto: rejection without reason")
	// An infected email container still expands (DES-HOR-461-03): its own
	// bytes are rejected, and every child is scanned independently, so a
	// malicious attachment never blocks its siblings. A policy rejection
	// never carries children.
	case v.Children < 0 || v.Children > MaxChildren || (!v.Accepted && v.Malware != MalwareInfected && v.Children != 0):
		return fmt.Errorf("scanproto: invalid child count %d", v.Children)
	case len(v.Signature) > maxSignatureLen || len(v.Reason) > 64:
		return errors.New("scanproto: unbounded evidence")
	}
	return nil
}

// Validate checks one child header against the expansion bounds.
func (c ChildHeader) Validate(index int) error {
	switch {
	case c.Index != index:
		return errors.New("scanproto: child order mismatch")
	case c.Kind != "body" && c.Kind != "attachment":
		return fmt.Errorf("scanproto: child kind %q", c.Kind)
	case c.Size <= 0 || c.Size > MaxChildBytes:
		return fmt.Errorf("scanproto: child size %d", c.Size)
	case c.SourceHTMLSize < 0 || c.SourceHTMLSize > MaxChildBytes || (c.SourceHTMLSize > 0 && c.Kind != "body"):
		return errors.New("scanproto: invalid html source size")
	case c.Filename == "" || len(c.Filename) > 255:
		return errors.New("scanproto: invalid child filename")
	case len(c.Digest) != len("sha256:")+64:
		return errors.New("scanproto: invalid child digest")
	}
	return nil
}
