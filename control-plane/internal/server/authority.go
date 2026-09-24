package server

import (
	"net/http"
)

// requireAuthorityV2 fails closed on cookie-session security mutations while the
// installation is still pre-epoch, so a V2-only authority surface can never run
// against pre-epoch state. Every V2 bearer credential is additionally invisible
// until the epoch flips, because the live credential projection only exposes
// rows written by the cutover.
func (h *Handler) requireAuthorityV2(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.store == nil {
			authError(w, http.StatusServiceUnavailable, "unavailable", "This is temporarily unavailable. Try again shortly.")
			return
		}
		if err := h.store.RequireAuthorityV2(r.Context()); err != nil {
			authError(w, http.StatusServiceUnavailable, "authority_migration_pending",
				"Security administration is temporarily unavailable while the installation is being prepared.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
