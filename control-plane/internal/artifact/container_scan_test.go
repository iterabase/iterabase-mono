package artifact_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanner"
)

// containsMarker reports the test marker in raw bytes or, as clamd's
// ScanMail does, inside any decoded MIME part of an email.
func containsMarker(data []byte) bool {
	marker := []byte(admissiontest.MalwareMarker)
	if bytes.Contains(data, marker) {
		return true
	}
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return false
	}
	return partContainsMarker(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body, marker, 0)
}

func partContainsMarker(contentType, encoding string, body io.Reader, marker []byte, depth int) bool {
	if depth > 8 {
		return false
	}
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				return false
			}
			if partContainsMarker(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, marker, depth+1) {
				return true
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, newlineStripper{body})
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	}
	decoded, err := io.ReadAll(io.LimitReader(body, 64<<20))
	return err == nil && bytes.Contains(decoded, marker)
}

// newlineStripper drops CR/LF so wrapped base64 decodes.
type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	k, err := n.r.Read(p)
	out := p[:0]
	for _, b := range p[:k] {
		if b != '\r' && b != '\n' {
			out = append(out, b)
		}
	}
	return len(out), err
}

// Regression: integrity verification must not end the HTTP/2 exchange while
// a large child is still streaming back from the scanner.
func TestLargeEmailChildStreamsThroughMTLS(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	large := []byte(strings.Repeat("Plain text line of a large attachment 0123456789.\n", 80_000)) // ~4 MiB
	uploaded := h.admit("large.eml", "message/rfc822", admissiontest.EML("Large", "See attached.", "",
		admissiontest.Attachment{Filename: "large.txt", ContentType: "text/plain", Data: large}))
	h.drain()
	require.True(t, h.get(uploaded.ID).Cleared(), "the container clears once every child frame is consumed")
	kids, err := h.store.Children(ctx, uploaded.ID)
	require.NoError(t, err)
	require.Len(t, kids, 2)
	var found bool
	for _, k := range kids {
		assert.True(t, h.get(k.ID).Cleared(), deref(k.DisplayName))
		if deref(k.DisplayName) == "large.txt" {
			found = true
			require.NotNil(t, k.SizeBytes)
			assert.Equal(t, int64(len(large)), *k.SizeBytes)
		}
	}
	assert.True(t, found, "the large attachment is admitted whole")
}

// The shipped clamd configuration (ScanMail yes) flags a message whose
// attachment matches a signature. Against real ClamAV, the message bytes are
// rejected while the malicious attachment is rejected alone and its clean
// siblings clear.
func TestRealClamdIsolatesMaliciousAttachment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real ClamAV integration test in short mode")
	}
	h := newHarnessWithEngine(t, startClamd(t))
	ctx := context.Background()
	message := admissiontest.EML("Order 43", "Body of the received email.", "",
		admissiontest.Attachment{Filename: "clean.pdf", ContentType: "application/pdf", Data: admissiontest.PDF()},
		admissiontest.Attachment{Filename: "marked.txt", ContentType: "text/plain", Data: admissiontest.WithMarker(admissiontest.TXT())},
	)
	root := h.admit("message.eml", "message/rfc822", message, func(in *artifactstore.AdmitInput) {
		in.SourceType, in.SourceRef, in.ContainerDepth = artifactstore.SourceMailbox, "mailbox/inbox/message-43", 0
		in.Attribution = artifactstore.Attribution{RequestActorID: h.actorID, Source: artifactstore.AuthSystem}
	})
	h.drain()
	got := h.get(root.ID)
	require.Equal(t, artifactstore.SecurityRejected, got.SecurityState, "clamd flags the message itself")
	require.False(t, got.Cleared())
	kids, err := h.store.Children(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, kids, 3, "body, clean attachment, marked attachment")
	// The rejected attachment's tombstone keeps no display name, so the clean
	// siblings are matched by name and the rest must be the one rejection.
	cleared, rejected := map[string]bool{}, 0
	for _, k := range kids {
		state := h.get(k.ID)
		if state.Cleared() {
			cleared[deref(state.DisplayName)] = true
			continue
		}
		assert.Equal(t, artifactstore.SecurityRejected, state.SecurityState)
		assert.Equal(t, "malware", deref(state.SecurityReason))
		rejected++
	}
	assert.Equal(t, map[string]bool{"message-body.txt": true, "clean.pdf": true}, cleared, "clean siblings clear")
	assert.Equal(t, 1, rejected, "the malicious attachment is rejected alone")
}

// startClamd runs the pinned ClamAV with the shipped clamd.conf and a test
// signature database: the harmless marker plus EICAR for the adapter's
// self-test.
func startClamd(t *testing.T) scanner.Engine {
	t.Helper()
	ctx := context.Background()
	eicar := `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`
	db := "Iterabase.Test.Marker:0:*:" + hex.EncodeToString([]byte(admissiontest.MalwareMarker)) + "\n" +
		"Eicar-Test-Signature:0:*:" + hex.EncodeToString([]byte(eicar)) + "\n"
	c, err := testcontainers.Run(ctx, shippedClamavImage(t),
		testcontainers.WithImagePlatform("linux/amd64"),
		testcontainers.WithExposedPorts("3310/tcp"),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{Reader: strings.NewReader(shippedClamdConf(t)), ContainerFilePath: "/db/clamd.conf", FileMode: 0o644},
			testcontainers.ContainerFile{Reader: strings.NewReader(db), ContainerFilePath: "/db/iterabase-test.ndb", FileMode: 0o644},
		),
		testcontainers.WithEntrypoint("clamd", "--foreground=true", "--config-file=/db/clamd.conf"),
		testcontainers.WithWaitStrategyAndDeadline(5*time.Minute, wait.ForListeningPort("3310/tcp")),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	endpoint, err := c.PortEndpoint(ctx, "3310/tcp", "")
	require.NoError(t, err)
	return scanner.Clamd{Addr: endpoint, Timeout: time.Minute}
}

// chartFile reads one file of the control-plane chart, so the test runs
// exactly what ships.
func chartFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(append([]string{filepath.Dir(file), "..", "..", "..", "charts", "charts", "control-plane"}, parts...)...))
	require.NoError(t, err)
	return b
}

// shippedClamavImage is the chart's digest-pinned scanner image.
func shippedClamavImage(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`repository: (clamav/clamav)\n\s+tag: (\S+)\n\s+digest: (sha256:[0-9a-f]{64})`).FindSubmatch(chartFile(t, "values.yaml"))
	require.NotNil(t, m, "pinned ClamAV image not found in chart values")
	return string(m[1]) + ":" + string(m[2]) + "@" + string(m[3])
}

// shippedClamdConf is the chart's clamd.conf with only the listen address and
// database directory adapted to the test container.
func shippedClamdConf(t *testing.T) string {
	t.Helper()
	b := chartFile(t, "templates", "artifact-scanner.yaml")
	m := regexp.MustCompile(`(?s)\{\{- define "control-plane.artifactClamdConf" -\}\}\n(.*?)\{\{- end -\}\}`).FindSubmatch(b)
	require.NotNil(t, m, "clamd.conf define not found in the chart")
	conf := string(m[1])
	require.Contains(t, conf, "ScanMail yes")
	conf = regexp.MustCompile(`(?m)^TCPAddr .*$`).ReplaceAllString(conf, "TCPAddr 0.0.0.0")
	conf = regexp.MustCompile(`(?m)^DatabaseDirectory .*$`).ReplaceAllString(conf, "DatabaseDirectory /db")
	return conf
}
