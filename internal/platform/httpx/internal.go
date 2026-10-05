package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/logging"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
)

// Check is one readiness dependency.
type Check struct {
	Name string
	Run  func(context.Context) error
}

// Internal serves a process's operational endpoints, never on the public
// listener: /livez answers while the process runs, /readyz runs every check
// (each bounded by two seconds) and adds info (such as a worker's
// capabilities), and /metrics serves Prometheus.
func Internal(checks []Check, info func() map[string]any) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		results := make(map[string]string, len(checks))
		var mu sync.Mutex
		var wg sync.WaitGroup
		ready := true
		for _, c := range checks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				result := "ok"
				if err := c.Run(ctx); err != nil {
					result = logging.RedactString(err.Error())
				}
				mu.Lock()
				defer mu.Unlock()
				results[c.Name] = result
				if result != "ok" {
					ready = false
				}
			}()
		}
		wg.Wait()
		body := map[string]any{"status": "ready", "checks": results}
		if info != nil {
			for k, v := range info() {
				body[k] = v
			}
		}
		status := http.StatusOK
		if !ready {
			body["status"], status = "not_ready", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.Handle("GET /metrics", metrics.Handler())
	return mux
}

// ServeInternal listens on addr and serves h until stop is called.
func ServeInternal(addr string, h http.Handler, logger *slog.Logger) (stop func(context.Context) error, err error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on the internal address %s: %w", addr, err)
	}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("internal listener stopped", "error", err)
		}
	}()
	logger.Info("internal listener serving /livez, /readyz and /metrics", "address", listener.Addr().String())
	return server.Shutdown, nil
}

// Probe asks the process listening on addr whether it is ready: the
// container health check, for images without curl.
func Probe(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("internal address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Checks map[string]string `json:"checks"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return fmt.Errorf("not ready (%d): %v", resp.StatusCode, body.Checks)
	}
	return nil
}
