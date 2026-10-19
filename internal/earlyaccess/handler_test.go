package earlyaccess

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type signedInUser struct{}

func (signedInUser) UserIDs(context.Context) (int64, int64, bool) { return 1, 101, true }
func (signedInUser) Username(context.Context) (string, bool)      { return "owner", true }
func (signedInUser) CSRFToken(context.Context) string             { return "csrf" }

func TestOAuthCallbacksKeepTheirOriginalWorkspaceAuthority(t *testing.T) {
	// No pool is supplied: callbacks must reach the state-validating connection
	// handler, whose persistence checks the original workspace, without querying
	// admission for the workspace selected by another browser tab.
	h := NewHandler(NewService(nil, true, nil), signedInUser{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, path := range []string{"/installations/callback", "/installations/authorize/callback"} {
		t.Run(path, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) })
			w := httptest.NewRecorder()
			h.Guard(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if !called || w.Code != http.StatusNoContent {
				t.Fatal("callback incorrectly checked the newly selected workspace")
			}
		})
	}
}
