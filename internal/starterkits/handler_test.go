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
	return send(t, caps, validate, httptest.NewRequest(http.MethodGet, target, nil))
}

// post sends the editor's text, with the CRLF line breaks a browser sends.
func post(t *testing.T, validate Validator, target, content string) *httptest.ResponseRecorder {
	t.Helper()
	body := url.Values{"content": {strings.ReplaceAll(content, "\n", "\r\n")}}.Encode()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(t, nil, validate, req)
}

func send(t *testing.T, caps Capabilities, validate Validator, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	h := NewHandler(fakeUsers{}, caps, defaults, validate, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Mount(r, func(next http.Handler) http.Handler { return next })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestPageShowsTheFirstStacksKit(t *testing.T) {
	rec := serve(t, nil, nil, "/starter-kits")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Starter kits") || !strings.Contains(body, "EXPOSE 3000") {
		t.Fatalf("status %d, body without the Next.js kit:\n%.400s", rec.Code, body)
	}
}

func TestDownloadsSendTheEditorsText(t *testing.T) {
	edited := "FROM python:3.13-slim\nEXPOSE 8100\n"
	for file, name := range map[string]string{"dockerfile": "Dockerfile", "previewrc": ".previewrc.yaml"} {
		rec := post(t, nil, "/starter-kits/download/"+file, edited)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Disposition") != `attachment; filename="`+name+`"` {
			t.Fatalf("%s: status %d, disposition %q", file, rec.Code, rec.Header().Get("Content-Disposition"))
		}
		if rec.Body.String() != edited {
			t.Fatalf("%s must be the posted text with LF line breaks, got %q", file, rec.Body.String())
		}
	}
	if rec := post(t, nil, "/starter-kits/download/secrets", edited); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown file is not found, got %d", rec.Code)
	}
	if rec := post(t, nil, "/starter-kits/download/dockerfile", strings.Repeat("a", maxFileBytes+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a file over 64 KiB is refused, got %d", rec.Code)
	}
}

func TestEditedConfigIsCheckedWithLines(t *testing.T) {
	validate := func(config []byte, _ Offer) []Problem {
		if strings.Contains(string(config), "\r") {
			t.Error("the check must see LF line breaks")
		}
		if strings.Contains(string(config), "port: 0") {
			return []Problem{{Field: "port", Message: "Port must be between 1 and 65535.", Line: 2}}
		}
		return nil
	}
	rec := post(t, validate, "/starter-kits/check", "# kit\nport: 0\n")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Line 2") || !strings.Contains(body, "1 to fix") {
		t.Fatalf("an edited file's problems are listed by line, got %d:\n%s", rec.Code, body)
	}
	if !strings.Contains(body, `id="kit-status" hx-swap-oob="innerHTML"`) {
		t.Fatal("the result is also announced to screen readers")
	}
	if body := post(t, validate, "/starter-kits/check", "port: 3000\n").Body.String(); !strings.Contains(body, "Your edited .previewrc.yaml passes Previa") {
		t.Fatalf("a valid edit says it passes, got:\n%s", body)
	}
}

func TestSwitchingStackKeepsServicesAndTakesNewDefaults(t *testing.T) {
	q := url.Values{"from": {"nextjs"}, "stack": {"rails"}, "port": {"3001"}, "redis": {"on"}, "redis_version": {"7"}, "setup": {"on"}, "tier": {"large"}}
	c := choicesFrom(q)
	if c.Stack != "rails" || c.Port != 3000 || c.Health != "/up" || c.Redis != "7" || !c.Setup || c.SetupWith != "migrate" || c.Tier != "large" {
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
