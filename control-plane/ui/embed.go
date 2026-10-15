// Package dashboardui embeds the customer app in the API binary so the browser
// surface and the durable work APIs share one origin.
package dashboardui

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist is populated by npm run build. placeholder.txt keeps ordinary Go
// tooling compilable before the frontend build runs.
//
//go:embed dist/*
var dist embed.FS

// policyBeforeStyle and policyAfterStyle surround the style-src directive of
// the app's Content Security Policy. Every document response adds its own
// per-request style nonce (DES-HOR-459-04); script-src stays 'self'.
const (
	policyBeforeStyle = "default-src 'self'; connect-src 'self'; font-src 'self'; img-src 'self' data:; "
	policyAfterStyle  = "; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
	// basePolicy is the policy for responses that are not the app document.
	basePolicy = policyBeforeStyle + "style-src 'self'" + policyAfterStyle
)

// rootFiles are the unhashed files public/ copies to the dist root and
// index.html links by fixed name. They are revalidated, never immutable,
// because their name does not change with their content.
var rootFiles = map[string]bool{"/favicon.svg": true}

// nonceBytes is the random nonce size: 144 bits, base64 without padding.
const nonceBytes = 18

// Handler serves the embedded app.
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return NewHandler(root)
}

// NewHandler serves the app from a dist file system: immutable /assets/ files,
// the revalidated root files index.html links by name (the favicon),
// and index.html for every other path it is given, so browser deep links
// (/work/W-1, /settings/profile, /auth/verify?token=…) reach the client
// router. Each document gets a fresh CSP style nonce, published to the app as
// <meta property="csp-nonce">. When the dist has no index.html (the
// placeholder build that keeps Go tooling compiling), documents answer 503:
// the server is running without its app, which is an availability problem
// rather than a missing page.
func NewHandler(root fs.FS) http.Handler {
	h := &handler{root: root}
	if index, err := fs.ReadFile(root, "index.html"); err == nil {
		h.head, h.rest, h.ready = splitAfterHead(index)
	}
	return h
}

type handler struct {
	root  fs.FS
	ready bool
	// head is index.html up to and including the opening <head> tag and rest
	// is everything after it; the nonce meta tag is written between them.
	head, rest []byte
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		h.serveAsset(w, r, "public, max-age=31536000, immutable")
		return
	}
	if rootFiles[r.URL.Path] {
		h.serveAsset(w, r, "no-cache")
		return
	}
	h.serveDocument(w)
}

func (h *handler) serveAsset(w http.ResponseWriter, r *http.Request, cacheControl string) {
	w.Header().Set("Content-Security-Policy", basePolicy)
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	info, err := fs.Stat(h.root, name)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", cacheControl)
	http.ServeFileFS(w, r, h.root, name)
}

func (h *handler) serveDocument(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache")
	if !h.ready {
		w.Header().Set("Content-Security-Policy", basePolicy)
		http.Error(w, "The app is not built into this server.", http.StatusServiceUnavailable)
		return
	}
	nonce, err := newNonce()
	if err != nil {
		w.Header().Set("Content-Security-Policy", basePolicy)
		http.Error(w, "The app is temporarily unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Security-Policy", policyBeforeStyle+"style-src 'self' 'nonce-"+nonce+"'"+policyAfterStyle)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var body bytes.Buffer
	body.Grow(len(h.head) + len(h.rest) + 64)
	body.Write(h.head)
	body.WriteString(`<meta property="csp-nonce" content="` + nonce + `">`)
	body.Write(h.rest)
	_, _ = w.Write(body.Bytes())
}

// newNonce returns a fresh cryptographically random base64 nonce.
func newNonce() (string, error) {
	raw := make([]byte, nonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// splitAfterHead splits the document right after its opening <head> tag
// (<head> or <head ...>, never <header>).
func splitAfterHead(index []byte) (head, rest []byte, ok bool) {
	lower := bytes.ToLower(index)
	for offset := 0; ; {
		at := bytes.Index(lower[offset:], []byte("<head"))
		if at < 0 {
			return nil, nil, false
		}
		next := offset + at + len("<head")
		if next < len(lower) && strings.IndexByte("> \t\r\n", lower[next]) >= 0 {
			end := bytes.IndexByte(lower[next:], '>')
			if end < 0 {
				return nil, nil, false
			}
			cut := next + end + 1
			return index[:cut], index[cut:], true
		}
		offset = next
	}
}
