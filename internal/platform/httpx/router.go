package httpx

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter constructs a router with request IDs, logging, and panic recovery.
func NewRouter(logger *slog.Logger) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogging(logger))
	r.Use(panicRecovery(logger))
	return r
}

// requestID returns the ID installed by chi's middleware.
func requestID(r *http.Request) string {
	return middleware.GetReqID(r.Context())
}
