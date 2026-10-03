package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type processorFunc func(context.Context, string, string, string, any) error

func (f processorFunc) Process(c context.Context, id, k, h string, e any) error {
	return f(c, id, k, h, e)
}
func TestSignatureAndEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind, signature string
		want                        int
	}{
		{"valid unsupported", `{}`, "ping", "valid", 204},
		{"invalid json", `{`, "ping", "valid", 400},
		{"unsigned", `{}`, "ping", "", 401},
		{"sha1 rejected", `{}`, "ping", "sha1=abcd", 401},
		{"wrong signature", `{}`, "ping", "sha256=abcd", 401},
		{"oversize", strings.Repeat("x", MaxBodyBytes+1), "ping", "valid", 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler("test-secret", processorFunc(func(context.Context, string, string, string, any) error {
				t.Fatal("unsupported/invalid event processed")
				return nil
			}), slog.New(slog.NewTextHandler(io.Discard, nil)))
			req := httptest.NewRequest("POST", "/webhooks/github", strings.NewReader(tc.body))
			req.Header.Set("X-GitHub-Delivery", "test-delivery")
			req.Header.Set("X-GitHub-Event", tc.kind)
			sig := tc.signature
			if sig == "valid" {
				mac := hmac.New(sha256.New, []byte("test-secret"))
				mac.Write([]byte(tc.body))
				sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
			}
			req.Header.Set("X-Hub-Signature-256", sig)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
	h := NewHandler("", nil, slog.Default())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader(`{}`)))
	if w.Code != 503 {
		t.Fatal("unconfigured receiver accepted")
	}
}
