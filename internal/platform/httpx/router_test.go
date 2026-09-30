package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterLogsRequestAndRecoversPanic(t *testing.T) {
	var logs bytes.Buffer
	r := NewRouter(slog.New(slog.NewJSONHandler(&logs, nil)))
	r.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("test failure") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "test failure") {
		t.Fatal("response exposed panic details")
	}
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("missing request ID")
	}
	var panicLog, requestLog struct {
		Message   string `json:"msg"`
		RequestID string `json:"request_id"`
		Status    int    `json:"status"`
	}
	decoder := json.NewDecoder(&logs)
	if err := decoder.Decode(&panicLog); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&requestLog); err != nil {
		t.Fatal(err)
	}
	if panicLog.RequestID != id || requestLog.RequestID != id || requestLog.Status != 500 {
		t.Fatalf("logs did not preserve request ID and recovered status: %+v %+v", panicLog, requestLog)
	}
}
