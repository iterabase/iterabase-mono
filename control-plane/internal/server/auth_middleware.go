package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// Browser-authentication defaults fixed by DES-HOR-451-06/07.
const (
	DefaultSessionCookie      = "__Host-iterabase_session"
	DefaultSessionIdleTTL     = 12 * time.Hour
	DefaultSessionAbsoluteTTL = 30 * 24 * time.Hour
	DefaultRecentAuthTTL      = 15 * time.Minute
	csrfHeaderName            = "X-CSRF-Token"
)

// Browser role constants (exactly two customer roles).
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
)

// AuthServices configures the browser-authentication surface. Sign-in is
// always on; a missing configuration fails every authenticated route closed.
type AuthServices struct {
	PublicOrigin    string
	CookieName      string
	IdleTTL         time.Duration
	AbsoluteTTL     time.Duration
	RecentAuthTTL   time.Duration
	TrustedProxies  []*net.IPNet
	ForwardedHeader string
	Store           *identity.Store
	Location        identity.LocationResolver
	Logger          *slog.Logger
	Now             func() time.Time
}

type authContextKey string

const (
	ctxBrowserSession authContextKey = "browser_session"
	ctxSessionToken   authContextKey = "browser_session_token"
)

// ErrBearerNotAccepted is returned when a browser-only route receives an
// Authorization header, even alongside a valid cookie.
var ErrBearerNotAccepted = errors.New("server: bearer authentication is not accepted on this route")

func (a *AuthServices) cookieName() string {
	if a != nil && a.CookieName != "" {
		return a.CookieName
	}
	return DefaultSessionCookie
}

func (a *AuthServices) idleTTL() time.Duration {
	if a != nil && a.IdleTTL > 0 {
		return a.IdleTTL
	}
	return DefaultSessionIdleTTL
}

func (a *AuthServices) absoluteTTL() time.Duration {
	if a != nil && a.AbsoluteTTL > 0 {
		return a.AbsoluteTTL
	}
	return DefaultSessionAbsoluteTTL
}

func (a *AuthServices) recentAuthTTL() time.Duration {
	if a != nil && a.RecentAuthTTL > 0 {
		return a.RecentAuthTTL
	}
	return DefaultRecentAuthTTL
}

func (a *AuthServices) now() time.Time {
	if a != nil && a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// OriginAllowed reports whether an unsafe browser request carries the exact
// configured public origin.
func (a *AuthServices) OriginAllowed(r *http.Request) bool {
	if a == nil || a.PublicOrigin == "" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	return strings.TrimRight(origin, "/") == strings.TrimRight(a.PublicOrigin, "/")
}

// ClientIP derives the request source IP with trusted-proxy chain validation.
// It trusts the direct peer unless that peer is a configured trusted proxy, and
// then walks the forwarded chain right-to-left to the first untrusted hop.
// Malformed input safely falls back to the direct peer.
func (a *AuthServices) ClientIP(r *http.Request) net.IP {
	peer := parseHostIP(r.RemoteAddr)
	if peer == nil || !a.trustedPeer(peer) {
		return peer
	}
	header := a.forwardedHeader()
	if header == "" {
		return peer
	}
	raw := r.Header.Get(header)
	if raw == "" {
		return peer
	}
	chain := strings.Split(raw, ",")
	for i := len(chain) - 1; i >= 0; i-- {
		candidate := parseHostIP(strings.TrimSpace(chain[i]))
		if candidate == nil {
			return peer
		}
		if !a.trustedPeer(candidate) {
			return candidate
		}
	}
	return peer
}

// Location resolves optional advisory coarse location. Failure never denies or
// widens a request.
func (a *AuthServices) LocationFor(ip net.IP) (identity.Location, bool) {
	if a == nil || a.Location == nil || ip == nil {
		return identity.Location{}, false
	}
	return a.Location.Lookup(ip)
}

func (a *AuthServices) forwardedHeader() string {
	if a != nil && a.ForwardedHeader != "" {
		return a.ForwardedHeader
	}
	return "X-Forwarded-For"
}

func (a *AuthServices) trustedPeer(ip net.IP) bool {
	if a == nil {
		return false
	}
	for _, network := range a.TrustedProxies {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func parseHostIP(value string) net.IP {
	if value == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(value)
}

// sessionCookie reads the opaque browser-session token. Browser-only routes
// reject bearer authentication even when a valid cookie is also presented.
func (a *AuthServices) sessionCookie(r *http.Request) (string, error) {
	if r.Header.Get("Authorization") != "" {
		return "", ErrBearerNotAccepted
	}
	cookie, err := r.Cookie(a.cookieName())
	if err != nil || cookie.Value == "" {
		return "", identity.ErrSessionInvalid
	}
	return cookie.Value, nil
}

func (a *AuthServices) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure/HttpOnly/NoDomain are contract-fixed
		Name:     a.cookieName(),
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *AuthServices) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure/HttpOnly/NoDomain are contract-fixed
		Name:     a.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// sessionFromContext returns the resolved browser session.
func sessionFromContext(ctx context.Context) (identity.BrowserSession, bool) {
	session, ok := ctx.Value(ctxBrowserSession).(identity.BrowserSession)
	return session, ok
}
