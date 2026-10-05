package githubapp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/metrics"
)

func gauge(t *testing.T, name, label, value string) (float64, bool) {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					if m.GetGauge() != nil {
						return m.GetGauge().GetValue(), true
					}
					return m.GetCounter().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

func TestTransportRecordsRateLimitHeadroom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Resource", "core")
		if r.URL.Path == "/limited" {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	client := &http.Client{Transport: baseTransport}
	resp, err := client.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if v, ok := gauge(t, "previa_github_rate_limit_remaining", "resource", "core"); !ok || v != 4321 {
		t.Fatalf("remaining = %v (%v)", v, ok)
	}
	resp, err = client.Get(srv.URL + "/limited")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if v, _ := gauge(t, "previa_github_rate_limit_remaining", "resource", "core"); v != 0 {
		t.Fatalf("remaining after exhaustion = %v", v)
	}
	if v, ok := gauge(t, "previa_github_requests_total", "status", "4xx"); !ok || v < 1 {
		t.Fatalf("4xx responses = %v", v)
	}
	if _, err := client.Get("http://127.0.0.1:1/"); err == nil {
		t.Fatal("expected a connection error")
	}
	if v, ok := gauge(t, "previa_github_requests_total", "status", "error"); !ok || v < 1 {
		t.Fatalf("transport errors = %v", v)
	}
}
