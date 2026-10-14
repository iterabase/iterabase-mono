package dashboardui_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dashboardui "github.com/nunocgoncalves/iterabase-mono/control-plane/ui"
)

const testIndex = `<!doctype html><html lang="en"><head><meta charset="UTF-8" /><title>Iterabase</title></head>` +
	`<body><header>x</header><div id="root"></div><script type="module" src="/assets/app.js"></script></body></html>`

var (
	metaNonce   = regexp.MustCompile(`<head><meta property="csp-nonce" content="([A-Za-z0-9+/=]+)"><meta charset`)
	headerNonce = regexp.MustCompile(`style-src 'self' 'nonce-([A-Za-z0-9+/=]+)';`)
)

func builtApp() http.Handler {
	return dashboardui.NewHandler(fstest.MapFS{
		"index.html":    {Data: []byte(testIndex)},
		"assets/app.js": {Data: []byte("console.log('app')")},
	})
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestDocumentCarriesAFreshStyleNonce(t *testing.T) {
	app := builtApp()
	seen := map[string]bool{}
	for _, path := range []string{"/", "/work/W-1", "/team/quotes", "/settings/profile", "/auth/verify?token=abc"} {
		response := get(app, path)
		require.Equal(t, http.StatusOK, response.Code, path)
		assert.Equal(t, "text/html; charset=utf-8", response.Header().Get("Content-Type"), path)
		assert.Equal(t, "no-cache", response.Header().Get("Cache-Control"), path)

		meta := metaNonce.FindStringSubmatch(response.Body.String())
		require.Len(t, meta, 2, "%s: the nonce meta is the first element of <head>: %s", path, response.Body.String())
		policy := response.Header().Get("Content-Security-Policy")
		header := headerNonce.FindStringSubmatch(policy)
		require.Len(t, header, 2, policy)
		assert.Equal(t, meta[1], header[1], "%s: the header nonce is the document nonce", path)
		raw, err := base64.StdEncoding.DecodeString(meta[1])
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(raw)*8, 128, "at least 128 random bits")
		assert.False(t, seen[meta[1]], "every response gets a fresh nonce")
		seen[meta[1]] = true

		assert.Equal(t, "default-src 'self'; connect-src 'self'; font-src 'self'; img-src 'self' data:; "+
			"style-src 'self' 'nonce-"+meta[1]+"'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'", policy)
		assert.Contains(t, response.Body.String(), `<header>x</header>`, "only the opening <head> tag is rewritten")
	}
}

func TestAssetsAreImmutableAndMissingAssetsAre404(t *testing.T) {
	app := builtApp()
	asset := get(app, "/assets/app.js")
	require.Equal(t, http.StatusOK, asset.Code)
	assert.Equal(t, "public, max-age=31536000, immutable", asset.Header().Get("Cache-Control"))
	assert.NotContains(t, asset.Header().Get("Content-Security-Policy"), "nonce-")
	assert.Equal(t, http.StatusNotFound, get(app, "/assets/missing.js").Code)
	assert.Equal(t, http.StatusNotFound, get(app, "/assets/").Code, "no directory listing")
}

func TestPlaceholderBuildAnswers503(t *testing.T) {
	app := dashboardui.NewHandler(fstest.MapFS{"placeholder.txt": {Data: []byte("placeholder")}})
	for _, path := range []string{"/", "/work/W-1"} {
		response := get(app, path)
		assert.Equal(t, http.StatusServiceUnavailable, response.Code, path)
		assert.NotContains(t, response.Header().Get("Content-Security-Policy"), "nonce-")
	}
	assert.Equal(t, http.StatusNotFound, get(app, "/assets/app.js").Code)
	assert.Equal(t, http.StatusServiceUnavailable, get(app, "/placeholder.txt").Code,
		"a non-asset path is a document request, never a raw dist file")
}
