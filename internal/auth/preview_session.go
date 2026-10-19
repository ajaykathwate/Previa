package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PreviewSessionID returns a digest, never the dashboard's bearer token. It is
// safe to bind into a signed claim sent to an independently hosted preview.
func (h *Handler) PreviewSessionID(ctx context.Context) string {
	if !h.sessions.HashTokenInStore {
		return ""
	}
	raw := h.sessions.Token(ctx)
	if raw == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}
func (h *Handler) PreviewSessionValid(ctx context.Context, id string) (bool, error) {
	if !h.sessions.HashTokenInStore || len(id) != 43 {
		return false, nil
	}
	var found bool
	var err error
	if store, ok := h.sessions.Store.(interface {
		FindCtx(context.Context, string) ([]byte, bool, error)
	}); ok {
		_, found, err = store.FindCtx(ctx, id)
	} else {
		_, found, err = h.sessions.Store.Find(id)
	}
	if err != nil {
		return false, fmt.Errorf("read preview issuing session: %w", err)
	}
	return found, nil
}
