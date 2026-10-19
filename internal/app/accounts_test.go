package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/alexedwards/scs/pgxstore"
)

func TestAccountSessionsSupportAndPublicPolicies(t *testing.T) {
	pool := e2eSchema(t)
	store := pgxstore.NewWithCleanupInterval(pool, 0)
	t.Cleanup(store.StopCleanup)
	cfg := config.ServerConfig{AppBaseURL: "https://" + testAppHost, SessionSecret: strings.Repeat("s", 32)}
	cfg.SupportEmail = "support@example.test"
	cfg.SupportOperatorGitHubIDs = "101"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := realtime.NewHub()
	defer hub.Close()
	f, err := newServerFeatures(cfg, pool, store, hub, logger)
	if err != nil {
		t.Fatal(err)
	}
	router, err := f.routes(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	var user int64
	if err = pool.QueryRow(t.Context(), `INSERT INTO users(github_id,username) VALUES(101,'operator') RETURNING id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	session := func() *http.Cookie {
		t.Helper()
		ctx, err := f.sessions.Load(t.Context(), "")
		if err != nil {
			t.Fatal(err)
		}
		f.sessions.Put(ctx, "user_id", user)
		f.sessions.Put(ctx, "session_epoch", int64(0))
		f.sessions.Put(ctx, "csrf_token", "fixture-csrf")
		token, _, err := f.sessions.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: f.sessions.Cookie.Name, Value: token}
	}
	current, other := session(), session()
	send := func(method, path string, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "https://"+testAppHost+path, strings.NewReader(form.Encode()))
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/help", "/status", "/policies/privacy", "/policies/terms", "/policies/acceptable-use"} {
		w := send("GET", path, nil, nil)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "'nonce-") {
			t.Fatalf("public page %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/account/delete", "/auth/sessions/revoke", "/help/requests", "/operator/incidents"} {
		if w := send("POST", path, current, url.Values{"confirm": {"DELETE"}}); w.Code != 403 {
			t.Fatalf("unprotected mutation %s: %d", path, w.Code)
		}
	}
	if w := send("GET", "/account", current, nil); w.Code != 200 {
		t.Fatalf("account page: %d %s", w.Code, w.Body)
	}
	// Capture exactly the authorization closure used by live responses.
	var streamCtx context.Context
	capture := f.sessions.LoadAndSave(f.auth.LoadUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { streamCtx = r.Context(); w.WriteHeader(204) })))
	req := httptest.NewRequest("GET", "https://"+testAppHost+"/", nil)
	req.AddCookie(other)
	capture.ServeHTTP(httptest.NewRecorder(), req)
	if err = realtime.CheckSession(streamCtx); err != nil {
		t.Fatal("live session initially invalid", err)
	}
	w := send("POST", "/auth/sessions/revoke", current, url.Values{"csrf_token": {"fixture-csrf"}})
	if w.Code != 303 {
		t.Fatalf("revoke sessions: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == current.Name {
			current = c
		}
	}
	if current.Value == other.Value {
		t.Fatal("current token not renewed")
	}
	if w = send("GET", "/protected", other, nil); w.Code != 303 {
		t.Fatal("other session survived", w.Code)
	}
	if err = realtime.CheckSession(streamCtx); err == nil {
		t.Fatal("existing live stream remained authorized")
	}
	if w = send("GET", "/protected", current, nil); w.Code != 200 {
		t.Fatalf("current session lost: %d %s", w.Code, w.Body)
	}
	ctx, err := f.sessions.Load(t.Context(), current.Value)
	if err != nil {
		t.Fatal(err)
	}
	csrf := f.sessions.GetString(ctx, "csrf_token")
	previewSession := f.auth.PreviewSessionID(ctx)
	if valid, err := f.auth.PreviewSessionValid(ctx, previewSession); err != nil || !valid {
		t.Fatal("issuing session invalid", valid, err)
	}
	w = send("POST", "/help/requests", current, url.Values{"csrf_token": {csrf}, "category": {"other"}, "message": {"<script>secret</script>"}})
	if w.Code != 303 {
		t.Fatalf("support submit %d %s", w.Code, w.Body)
	}
	if w = send("GET", w.Header().Get("Location"), current, nil); w.Code != 200 || strings.Contains(w.Body.String(), "<script>secret</script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatalf("support escaping: %d %s", w.Code, w.Body)
	}
	w = send("GET", "/operator/support", current, nil)
	if w.Code != 200 {
		t.Fatalf("configured operator: %d %s", w.Code, w.Body)
	}
	if _, err = pool.Exec(t.Context(), `UPDATE users SET github_id=102 WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if w = send("GET", "/operator/support", current, nil); w.Code != 404 {
		t.Fatal("operator allowlist bypassed", w.Code)
	}
	// Ordinary logout closes a live stream too, without revoking other members.
	req = httptest.NewRequest("GET", "https://"+testAppHost+"/", nil)
	req.AddCookie(current)
	capture.ServeHTTP(httptest.NewRecorder(), req)
	if w = send("POST", "/auth/logout", current, url.Values{"csrf_token": {csrf}}); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if err = realtime.CheckSession(streamCtx); err == nil {
		t.Fatal("logout left stream authorized")
	}
	if valid, err := f.auth.PreviewSessionValid(t.Context(), previewSession); err != nil || valid {
		t.Fatal("logout left issuing session valid for preview grants", valid, err)
	}
}
