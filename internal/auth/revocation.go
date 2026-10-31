package auth

import (
	"context"
	"fmt"
	"net/http"
)

func (r *Repository) AdvanceSessionEpoch(ctx context.Context, user int64) (int64, error) {
	epoch, err := r.queries.AdvanceSessionEpoch(ctx, user)
	if err != nil {
		return 0, fmt.Errorf("revoke account sessions: %w", err)
	}
	return epoch, nil
}

func (h *Handler) endOtherSessions(w http.ResponseWriter, r *http.Request) {
	u, ok := CurrentUser(r.Context())
	store, supported := h.users.(interface {
		AdvanceSessionEpoch(context.Context, int64) (int64, error)
	})
	if !ok || !supported {
		h.fail(w, r, http.StatusServiceUnavailable, "Sessions cannot be revoked right now.")
		return
	}
	epoch, err := store.AdvanceSessionEpoch(r.Context(), u.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "revoke other sessions", "error", err)
		h.fail(w, r, http.StatusServiceUnavailable, "Other sessions could not be revoked. Please try again.")
		return
	}
	if err := h.sessions.RenewToken(r.Context()); err != nil {
		h.logger.ErrorContext(r.Context(), "rotate current session after revocation", "error", err)
		h.fail(w, r, http.StatusServiceUnavailable, "Sessions were revoked. Please sign in again.")
		return
	}
	h.sessions.Put(r.Context(), "session_epoch", epoch)
	h.sessions.Put(r.Context(), "csrf_token", randomToken())
	http.Redirect(w, r, "/protected?saved=sessions", http.StatusSeeOther)
}
