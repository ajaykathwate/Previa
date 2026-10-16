package onboarding

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	pages "github.com/ajaykathwatee/previa/web/pages/onboarding"
	"github.com/go-chi/chi/v5"
)

// UserSession is the signed-in person; the auth feature implements it.
type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// Cursor marks the realtime position a page was read at, so its live stream
// replays anything committed after it; *realtime.Hub implements it.
type Cursor interface {
	Cursor() realtime.Cursor
}

// Handler serves the Setup tab, check requests and the Open preview action.
type Handler struct {
	service *Service
	users   UserSession
	live    Cursor
	logger  *slog.Logger
}

// NewHandler serves repository setup. live may be nil: the page then
// refreshes on navigation only.
func NewHandler(service *Service, users UserSession, live Cursor, logger *slog.Logger) *Handler {
	return &Handler{service: service, users: users, live: live, logger: logger}
}

// Mount registers the routes for signed-in people. The workspace middleware
// authorizes repository paths first; every query checks access again.
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/repositories/{repoID}/setup", h.page)
		r.Post("/repositories/{repoID}/setup/check", h.check)
		r.Get("/repositories/{repoID}/pull-requests/{number}/open", h.open)
	})
}

func (h *Handler) identity(r *http.Request) (user, repo int64, ok bool) {
	repo, err := publicid.Param(r, "repoID", "repository")
	if err != nil || repo <= 0 {
		return 0, 0, false
	}
	user, _, ok = h.users.UserIDs(r.Context())
	return user, repo, ok && user > 0
}

// selection reads ?pr=, a pull request's public address or (for old HTML
// bookmarks) its number; 0 when absent.
func selection(r *http.Request, repo int64) (int32, bool) {
	value := r.URL.Query().Get("pr")
	if value == "" {
		return 0, true
	}
	n, err := publicid.Value(r, "pull-request", value, repo)
	if err != nil || n <= 0 || n > 1<<31-1 {
		return 0, false
	}
	return int32(n), true
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	number, ok := selection(r, repo)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Read the cursor before the state, so a change committed in between is
	// replayed by the page's stream.
	var cursor realtime.Cursor
	if h.live != nil {
		cursor = h.live.Cursor()
	}
	p, err := h.service.Page(r.Context(), user, repo, number)
	switch {
	case errors.Is(err, ErrClosed):
		publicid.Redirect(w, r, pages.URL(repo, 0)+"?notice=closed", http.StatusSeeOther)
		return
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "load repository setup", "error", err)
		http.Error(w, "Repository setup is temporarily unavailable. Please try again.", http.StatusServiceUnavailable)
		return
	}
	p.CSRF = h.users.CSRFToken(r.Context())
	if h.live != nil {
		p.LiveURL = "/repositories/" + strconv.FormatInt(repo, 10) + "/events?since=" + url.QueryEscape(cursor.String())
	}
	p.Toast, p.Notice = outcome(r.URL.Query())
	w.Header().Add("Vary", "HX-Request, HX-Target")
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "setup-detail" {
		publicid.Handler(r, pages.Detail(p), p).ServeHTTP(w, r)
		return
	}
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, pages.Index(username, p), p).ServeHTTP(w, r)
}

// outcome turns the redirect's query into what the page says happened.
func outcome(q url.Values) (toast string, notice pages.Notice) {
	switch q.Get("saved") {
	case "check":
		toast = "Setup check started"
	case "build":
		toast = "Build requested"
	case "sync":
		toast = "Syncing pull requests"
	}
	switch q.Get("notice") {
	case "running":
		notice = pages.Notice{Tone: "info", Text: "A check of this commit is already on its way; the checklist updates when it finishes."}
	case "recent":
		notice = pages.Notice{Tone: "info", Text: "This commit was checked moments ago. Push a change, then check again."}
	case "changed":
		notice = pages.Notice{Tone: "info", Text: "The pull request has a newer commit. This is its latest one; check it when you are ready."}
	case "closed":
		notice = pages.Notice{Tone: "info", Text: "That pull request is closed. Choose an open one."}
	case "access":
		notice = pages.Notice{Tone: "warn", Text: "GitHub access to this repository is unavailable, so its setup cannot be checked."}
	case "build-closed":
		notice = pages.Notice{Tone: "info", Text: "The pull request was closed, so nothing was built."}
	case "build-blocked":
		notice = pages.Notice{Tone: "warn", Text: "Building is blocked right now; the checklist says why. Nothing was requested."}
	case "build-unavailable":
		notice = pages.Notice{Tone: "warn", Text: "Builds cannot start right now. Nothing was requested; try again in a few minutes."}
	case "build-changed":
		notice = pages.Notice{Tone: "info", Text: "The pull request or its build changed meanwhile. This is its latest state; build again if it still applies."}
	case "build-github":
		notice = pages.Notice{Tone: "warn", Text: "GitHub could not confirm the pull request right now. Nothing was requested; try again shortly."}
	case "build-access":
		notice = pages.Notice{Tone: "warn", Text: "GitHub access to this pull request is no longer available. Nothing was requested."}
	case "limit":
		text := "This workspace used its setup checks for this hour. Try again later."
		if until, err := time.Parse(time.RFC3339, q.Get("until")); err == nil {
			text = "This workspace used its setup checks for this hour. Try again after " + until.UTC().Format("15:04") + " UTC."
		}
		notice = pages.Notice{Tone: "warn", Text: text}
	}
	return toast, notice
}

// check asks a worker to check the selected commit, then returns to the page.
func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	number, valid := selection(r, repo)
	if !ok || !valid || number <= 0 {
		http.NotFound(w, r)
		return
	}
	back := pages.URL(repo, number)
	result, err := h.service.RequestCheck(r.Context(), user, repo, number, r.PostFormValue("expected_sha"))
	var limit *LimitError
	switch {
	case errors.As(err, &limit):
		publicid.Redirect(w, r, back+"&notice=limit&until="+url.QueryEscape(limit.ResetsAt.UTC().Format(time.RFC3339)), http.StatusSeeOther)
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
	case errors.Is(err, ErrClosed):
		publicid.Redirect(w, r, pages.URL(repo, 0)+"?notice=closed", http.StatusSeeOther)
	case errors.Is(err, ErrChanged):
		publicid.Redirect(w, r, back+"&notice=changed", http.StatusSeeOther)
	case errors.Is(err, ErrAccess):
		publicid.Redirect(w, r, back+"&notice=access", http.StatusSeeOther)
	case errors.Is(err, ErrNoGitHub):
		http.Error(w, "This server cannot read GitHub repositories, so setup cannot be checked.", http.StatusServiceUnavailable)
	case err != nil:
		h.logger.ErrorContext(r.Context(), "request setup check", "error", err)
		http.Error(w, "The setup check could not be requested. Please try again.", http.StatusServiceUnavailable)
	case result == CheckRequested:
		publicid.Redirect(w, r, back+"&saved=check", http.StatusSeeOther)
	default:
		publicid.Redirect(w, r, back+"&notice="+string(result), http.StatusSeeOther)
	}
}

// open sends a person to a pull request's preview. Only an Open preview
// action taken inside Previa is recorded: a link from another site, or one
// prefetched, still opens the preview but counts for nothing.
func (h *Handler) open(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	number, err := publicid.PRNumber(r, repo)
	if err != nil || number <= 0 || number > 1<<31-1 {
		http.NotFound(w, r)
		return
	}
	address, err := h.service.Open(r.Context(), user, repo, int32(number), fromPrevia(r))
	switch {
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrNotDeployed):
		publicid.Redirect(w, r, "/repositories/"+strconv.FormatInt(repo, 10)+"/pull-requests/"+strconv.FormatInt(number, 10), http.StatusSeeOther)
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "open preview", "error", err)
		http.Error(w, "The preview could not be opened. Please try again.", http.StatusServiceUnavailable)
		return
	}
	// The preview never learns which Previa page sent the visitor.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, address, http.StatusSeeOther)
}

// fromPrevia reports whether a navigation started on Previa's own pages (or
// was typed by the person); speculative prefetches never count.
func fromPrevia(r *http.Request) bool {
	if r.Header.Get("Sec-Purpose") != "" || r.Header.Get("Purpose") == "prefetch" {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}
