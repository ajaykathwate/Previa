package httpx

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

func requestLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			ww.Header().Set("X-Request-ID", requestID(r))
			defer func() {
				status := ww.Status()
				if status == 0 {
					status = http.StatusOK
				}
				logger.InfoContext(r.Context(), "HTTP request",
					"request_id", requestID(r), "method", r.Method,
					"path", r.URL.Path, "status", status,
					"bytes", ww.BytesWritten(), "duration", time.Since(start))
			}()
			next.ServeHTTP(ww, r)
		})
	}
}

func panicRecovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if recovered == http.ErrAbortHandler {
						panic(recovered)
					}
					logger.ErrorContext(r.Context(), "HTTP handler panic",
						"request_id", requestID(r), "panic", fmt.Sprint(recovered),
						"stack", string(debug.Stack()))
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
