package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/identity"
)

// RouteKind is the authority class of one HTTP route (architecture 9.3,
// HOR-582). Every route is registered through Handler.route with exactly one
// class, which composes the only authentication and authorization the route
// accepts; TestEveryRouteIsClassified fails on a route registered any other way
// or whose class differs from the reviewed table.
type RouteKind string

const (
	// RouteProbe is the browser shell, static assets, and health/readiness.
	RouteProbe RouteKind = "probe"
	// RoutePublic is the public browser journey: each endpoint validates only
	// its own submitted credential or token.
	RoutePublic RouteKind = "public"
	// RouteSession accepts only a cookie browser session; bearer is refused even
	// alongside a valid cookie.
	RouteSession RouteKind = "session"
	// RouteAction accepts a browser session or a V2 bearer credential holding
	// the route's action.
	RouteAction RouteKind = "action"
	// RouteBearerAction accepts only a V2 bearer credential holding the action.
	RouteBearerAction RouteKind = "bearer_action"
)

// RouteClass is one route's authority contract.
type RouteClass struct {
	Kind RouteKind
	// Action is the single customer action for RouteAction/RouteBearerAction.
	Action string
	// Admin requires a current Admin browser session (RouteSession only).
	Admin bool
	// RecentAuth requires bounded recent-password proof (RouteSession only).
	RecentAuth bool
}

func probe() RouteClass        { return RouteClass{Kind: RouteProbe} }
func public() RouteClass       { return RouteClass{Kind: RoutePublic} }
func session() RouteClass      { return RouteClass{Kind: RouteSession} }
func adminSession() RouteClass { return RouteClass{Kind: RouteSession, Admin: true} }
func recentAdminSession() RouteClass {
	return RouteClass{Kind: RouteSession, Admin: true, RecentAuth: true}
}
func action(name string) RouteClass       { return RouteClass{Kind: RouteAction, Action: name} }
func bearerAction(name string) RouteClass { return RouteClass{Kind: RouteBearerAction, Action: name} }

// RouteKey is the classification key for one method and chi pattern.
func RouteKey(method, pattern string) string { return method + " " + pattern }

// route registers one classified route.
func (h *Handler) route(r chi.Router, method, pattern string, class RouteClass, handler http.HandlerFunc) {
	key := RouteKey(method, pattern)
	if (class.Kind == RouteAction || class.Kind == RouteBearerAction) && !identity.KnownAction(class.Action) {
		panic("server: route " + key + " names unknown action " + class.Action)
	}
	if _, dup := h.classes[key]; dup {
		panic("server: route registered twice: " + key)
	}
	h.classes[key] = class
	r.With(h.authorityFor(class)...).Method(method, pattern, handler)
}

func (h *Handler) authorityFor(class RouteClass) []func(http.Handler) http.Handler {
	switch class.Kind {
	case RouteProbe:
		return nil
	case RoutePublic:
		return []func(http.Handler) http.Handler{h.requireOrigin}
	case RouteSession:
		// requireBrowser runs first so a bearer request is refused as
		// bearer_not_accepted whether or not it carries a browser Origin.
		chain := []func(http.Handler) http.Handler{h.requireBrowser, h.requireOrigin, h.requireCSRF}
		if class.Admin {
			chain = append(chain, h.requireAdmin)
		}
		if class.RecentAuth {
			chain = append(chain, h.requireRecentAuth)
		}
		return chain
	case RouteAction:
		return []func(http.Handler) http.Handler{h.requireAction(class.Action, true)}
	case RouteBearerAction:
		return []func(http.Handler) http.Handler{h.requireAction(class.Action, false)}
	default:
		panic("server: unknown route kind " + string(class.Kind))
	}
}

// Principal is the authenticated request boundary: who the request is
// accountable to, who acted, through which credential, and which customer
// actions it currently holds. Callers never supply any of it.
type Principal struct {
	InitiatingHumanID string
	ActorID           string
	Source            string // browser | api
	SessionID         string
	APIKeyID          string
	actions           []string
}

// Principal sources persisted as work authorization evidence (architecture 7.15).
const (
	SourceBrowser = "browser"
	SourceAPI     = "api"
)

type principalKey struct{}

func principalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Can reports whether the principal currently holds the action, including its
// catalogue prerequisites.
func (p Principal) Can(action string) bool {
	return identity.ActionsSatisfied(p.actions, action)
}

// sessionActions is the personal action set a browser session holds for the
// account's *current* role: an Operator holds every non-Admin personal action
// and an Admin holds the complete personal catalogue. The role is re-read from
// the database on every request, so demotion or disablement takes effect on the
// next request.
func sessionActions(role string) []string {
	var out []string
	for _, candidate := range identity.CatalogueActions(identity.CredentialKindPersonal) {
		switch {
		case role == RoleAdmin:
			out = append(out, candidate)
		case role == RoleOperator && !identity.ActionAdminOnly(candidate):
			out = append(out, candidate)
		}
	}
	return out
}

// requireAction authorizes a customer request through exactly one credential:
// a V2 bearer credential when an Authorization header is present (never
// falling back to a cookie), otherwise the browser session when allowSession.
// Either way the current account, role, and credential state are re-read, so
// revocation, expiry, disablement, demotion, and suspension take effect on the
// next request. No wildcard or unknown action can authorize anything.
func (h *Handler) requireAction(required string, allowSession bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !h.authReady(w) {
				return
			}
			var principal Principal
			var ok bool
			switch {
			case r.Header.Get("Authorization") != "":
				principal, ok = h.bearerPrincipal(w, r, required)
			case allowSession:
				principal, ok = h.sessionPrincipal(w, r)
			default:
				authError(w, http.StatusUnauthorized, "api_credential_required",
					"This endpoint requires an API credential.")
				return
			}
			if !ok {
				return
			}
			if !principal.Can(required) {
				authError(w, http.StatusForbidden, "forbidden", "This is not available with your current access.")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
		})
	}
}

// bearerPrincipal resolves the live V2 credential projection.
func (h *Handler) bearerPrincipal(w http.ResponseWriter, r *http.Request, required string) (Principal, bool) {
	token, ok := identity.ParseBearer(r.Header.Get("Authorization"))
	if !ok {
		authError(w, http.StatusUnauthorized, "invalid_credential", "The API credential is missing or invalid.")
		return Principal{}, false
	}
	cred, _, _, err := h.store.ResolveCredential(r.Context(), token, h.authCfg.now())
	switch {
	case errors.Is(err, identity.ErrInvalidAPIKey):
		authError(w, http.StatusUnauthorized, "invalid_credential", "The API credential is missing or invalid.")
		return Principal{}, false
	case errors.Is(err, identity.ErrCredentialNotEligible):
		authError(w, http.StatusUnauthorized, "credential_not_eligible", "This API credential is not currently active.")
		return Principal{}, false
	case err != nil:
		authError(w, http.StatusServiceUnavailable, "unavailable", "This is temporarily unavailable. Try again shortly.")
		return Principal{}, false
	}
	// Admin-only actions also require the owner to be an Admin right now. The
	// projection already suspends such keys on demotion; this re-check keeps the
	// boundary independent of that lifecycle write.
	if identity.ActionAdminOnly(required) && cred.OwnerRole != identity.RoleAdmin {
		authError(w, http.StatusForbidden, "forbidden", "This is not available with your current access.")
		return Principal{}, false
	}
	return Principal{
		InitiatingHumanID: cred.OwnerIdentityID,
		ActorID:           cred.ActorIdentityID,
		Source:            SourceAPI,
		APIKeyID:          cred.ID,
		actions:           cred.Actions,
	}, true
}

// sessionPrincipal resolves the browser session and, for unsafe methods,
// enforces the exact origin and session-bound CSRF proof.
func (h *Handler) sessionPrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	token, err := h.authCfg.sessionCookie(r)
	if err != nil {
		h.writeSessionError(r, w, err)
		return Principal{}, false
	}
	current, err := h.store.ResolveBrowserSessionWithNetwork(r.Context(), token, h.authCfg.now(), h.sessionNetwork(r))
	if err != nil {
		h.writeSessionError(r, w, err)
		return Principal{}, false
	}
	if !safeMethod(r.Method) {
		if !h.authCfg.OriginAllowed(r) {
			authError(w, http.StatusForbidden, "origin", "This request did not come from the configured application origin.")
			return Principal{}, false
		}
		if !current.VerifyCSRF(r.Header.Get(csrfHeaderName)) {
			authError(w, http.StatusForbidden, "csrf", "The request could not be verified. Reload the page and try again.")
			return Principal{}, false
		}
	}
	return principalForSession(current), true
}

// sessionPrincipalFromContext derives the principal for a cookie-session-only
// route, after requireBrowser has resolved the session.
func sessionPrincipalFromContext(ctx context.Context) (Principal, bool) {
	current, ok := sessionFromContext(ctx)
	if !ok {
		return Principal{}, false
	}
	return principalForSession(current), true
}

// principalForSession is the browser principal: the signed-in human is both the
// initiating human and the request actor.
func principalForSession(current identity.BrowserSession) Principal {
	return Principal{
		InitiatingHumanID: current.IdentityID,
		ActorID:           current.IdentityID,
		Source:            SourceBrowser,
		SessionID:         current.ID,
		actions:           sessionActions(current.Role),
	}
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}
