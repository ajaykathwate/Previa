package starterkits

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ajaykathwatee/previa/internal/platform/workers"
)

type fakeUsers struct{}

func (fakeUsers) Username(context.Context) (string, bool) { return "octo", true }
func (fakeUsers) CSRFToken(context.Context) string        { return "token" }

type fakeCaps struct {
	caps workers.Capabilities
	err  error
}

func (f fakeCaps) Current(context.Context) (workers.Capabilities, error) { return f.caps, f.err }

func serve(t *testing.T, caps Capabilities, validate Validator, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	h := NewHandler(fakeUsers{}, caps, defaults, validate, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Mount(r, func(next http.Handler) http.Handler { return next })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestPageShowsTheFirstStacksKit(t *testing.T) {
	rec := serve(t, nil, nil, "/starter-kits")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Starter kits") || !strings.Contains(body, "EXPOSE 3000") {
		t.Fatalf("status %d, body without the Next.js kit:\n%.400s", rec.Code, body)
	}
}

func TestDownloadsSendTheKitsFiles(t *testing.T) {
	c := Fresh("django")
	c.Port = 8100
	q := c.Query()
	for file, name := range map[string]string{"dockerfile": "Dockerfile", "previewrc": ".previewrc.yaml"} {
		rec := serve(t, nil, nil, "/starter-kits/download/"+file+"?"+q.Encode())
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Disposition") != `attachment; filename="`+name+`"` {
			t.Fatalf("%s: status %d, disposition %q", file, rec.Code, rec.Header().Get("Content-Disposition"))
		}
		if !strings.Contains(rec.Body.String(), "8100") {
			t.Fatalf("%s must follow the query:\n%s", file, rec.Body.String())
		}
	}
	if rec := serve(t, nil, nil, "/starter-kits/download/secrets?"+q.Encode()); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown file is not found, got %d", rec.Code)
	}
	own := Fresh("custom").Query()
	if rec := serve(t, nil, nil, "/starter-kits/download/dockerfile?"+own.Encode()); rec.Code != http.StatusNotFound {
		t.Fatalf("a repository with its own Dockerfile gets none, got %d", rec.Code)
	}
}

func TestSwitchingStackKeepsServicesAndTakesNewDefaults(t *testing.T) {
	q := url.Values{"from": {"nextjs"}, "stack": {"rails"}, "port": {"3001"}, "redis": {"on"}, "redis_version": {"7"}, "setup": {"on"}, "tier": {"large"}}
	c := choicesFrom(q)
	if c.Stack != "rails" || c.Port != 3000 || c.Health != "/up" || c.Redis != "7" || !c.Setup || c.SetupWith != "prepare" || c.Tier != "large" {
		t.Fatalf("switching keeps services, setup and tier and takes Rails' defaults, got %+v", c)
	}
	q.Set("from", "rails")
	if c := choicesFrom(q); c.Port != 3001 {
		t.Fatalf("on the same stack the form is read as typed, got port %d", c.Port)
	}
	q.Set("port", "eighty")
	if kit := Generate(choicesFrom(q), defaults); !hasProblem(kit, "port") {
		t.Fatal("a port that is not a number is reported")
	}
}

func TestOfferFollowsTheRunningWorker(t *testing.T) {
	h := NewHandler(fakeUsers{}, fakeCaps{caps: workers.Capabilities{Execution: true, Services: []string{"postgres 16"}}}, defaults, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if o := h.offer(context.Background()); !o.Known || !slices.Equal(o.Postgres, []string{"16"}) || len(o.Redis) != 0 {
		t.Fatalf("a building worker's catalog is the offer, got %+v", o)
	}
	for _, caps := range []Capabilities{fakeCaps{caps: workers.Capabilities{}}, fakeCaps{err: errors.New("down")}} {
		h.caps = caps
		if o := h.offer(context.Background()); o.Known || !slices.Equal(o.Postgres, defaults.Postgres) {
			t.Fatalf("without a building worker Previa's defaults are offered, got %+v", o)
		}
	}
}

func TestValidatorProblemsAreShown(t *testing.T) {
	validate := func([]byte, Offer) []Problem {
		return []Problem{{Field: "config", Message: "Port is not allowed here."}}
	}
	rec := serve(t, nil, validate, "/starter-kits")
	if body := rec.Body.String(); !strings.Contains(body, "Port is not allowed here.") || !strings.Contains(body, "1 to fix") {
		t.Fatal("problems found by Previa's config rules are listed")
	}
}
