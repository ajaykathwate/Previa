package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ajaykathwatee/previa/internal/platform/queue"
	"github.com/go-chi/chi/v5"
)

// PingEnqueuer is the only queue behavior the diagnostic form needs.
type PingEnqueuer interface {
	EnqueuePing(context.Context, string) (int64, bool, error)
}

// MountPing adds an authenticated diagnostic POST, inside the CSRF-protected group.
func MountPing(r chi.Router, pings PingEnqueuer, requireUser func(http.Handler) http.Handler, logger *slog.Logger) {
	r.With(requireUser).Post("/queue/ping", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid ping form", http.StatusBadRequest)
			return
		}
		key := r.PostForm.Get("pr_key")
		if err := queue.ValidatePRKey(key); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, duplicate, err := pings.EnqueuePing(r.Context(), key)
		if err != nil {
			logger.ErrorContext(r.Context(), "enqueue ping", "error", err)
			http.Error(w, "Ping could not be queued. Check the key and database, then retry.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(struct {
			JobID     int64 `json:"job_id"`
			Duplicate bool  `json:"duplicate"`
		}{JobID: id, Duplicate: duplicate}); err != nil {
			logger.ErrorContext(r.Context(), "write ping response", "error", err)
		}
	})
}
