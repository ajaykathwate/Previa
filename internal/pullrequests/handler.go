package pullrequests

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	buildpages "github.com/ajaykathwatee/previa/web/pages/builds"
	environmentpages "github.com/ajaykathwatee/previa/web/pages/environments"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}
type Policy interface {
	Set(context.Context, int64, int64, installations.BuildMode) (installations.BuildPolicy, error)
}
type Refresher interface {
	Request(context.Context, int64, int64) error
}

// LiveEvents is the realtime hub surface the workspace needs for live pages.
type LiveEvents interface {
	Cursor() realtime.Cursor
	Subscribe(topic string, since realtime.Cursor) *realtime.Subscription
}
type Handler struct {
	repo    *Repository
	sync    Refresher
	policy  Policy
	users   UserSession
	logger  *slog.Logger
	enabled bool
	live    LiveEvents
	actions *Actions
	logs    LogTail
}

func NewHandler(repo *Repository, sync Refresher, policy Policy, users UserSession, logger *slog.Logger, enabled bool) *Handler {
	return &Handler{repo: repo, sync: sync, policy: policy, users: users, logger: logger, enabled: enabled}
}

// WithLiveUpdates enables the repository event stream. Without it, pages stay
// fully functional and refresh on navigation or explicit actions.
func (h *Handler) WithLiveUpdates(live LiveEvents) *Handler {
	h.live = live
	return h
}

// RepositoryTopic names the realtime topic raised by the repository triggers
// in db/migrations/0011_realtime_events.sql.
func RepositoryTopic(repoID int64) string { return "repository:" + strconv.FormatInt(repoID, 10) }
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/repositories/{repoID}", h.repository)
		r.Get("/repositories/{repoID}/pull-requests/{number}", h.detail)
		r.Get("/repositories/{repoID}/events", h.events)
		r.Post("/repositories/{repoID}/refresh", h.refresh)
		r.Post("/repositories/{repoID}/policy", h.setPolicy)
		r.Post("/repositories/{repoID}/pull-requests/{number}/build", h.build)
		r.Post("/repositories/{repoID}/pull-requests/{number}/teardown", h.teardown)
		r.Get("/builds", h.activity)
		r.Get("/environments", h.activity)
	})
}
func (h *Handler) identity(r *http.Request) (int64, int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, ErrUnavailable
	}
	user, _, ok := h.users.UserIDs(r.Context())
	if !ok {
		return 0, 0, ErrUnavailable
	}
	return user, id, nil
}
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnavailable) || errors.Is(err, installations.ErrPolicyUnavailable) {
		http.NotFound(w, r)
		return
	}
	h.logger.ErrorContext(r.Context(), "PR workspace request failed")
	http.Error(w, "Workspace temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
}
func parseFilter(r *http.Request) (Filter, error) {
	f := Filter{Page: 1, State: r.URL.Query().Get("state"), Search: strings.TrimSpace(r.URL.Query().Get("q"))}
	if f.State == "" {
		f.State = "open"
	}
	if f.State != "open" && f.State != "closed" && f.State != "merged" && f.State != "all" || len([]rune(f.Search)) > 100 {
		return f, errors.New("invalid filters")
	}
	if v := r.URL.Query().Get("page"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return f, errors.New("invalid page")
		}
		f.Page = n
	}
	return f, nil
}
func timestamp(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

func (h *Handler) repository(w http.ResponseWriter, r *http.Request) { h.servePage(w, r, false) }
func (h *Handler) detail(w http.ResponseWriter, r *http.Request)     { h.servePage(w, r, true) }
func (h *Handler) servePage(w http.ResponseWriter, r *http.Request, detail bool) {
	user, id, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	params := r.URL.Query()
	// Preserve older repository bookmarks which explicitly used state/q/page.
	if params.Has("state") {
		f, err := parseFilter(r)
		if err != nil {
			http.Error(w, "Invalid pull request filters.", http.StatusBadRequest)
			return
		}
		params.Set("pr_q", f.Search)
		params.Set("pr_state", f.State)
		params.Set("pr_page", strconv.FormatInt(f.Page, 10))
		params.Del("q")
		params.Del("state")
		params.Del("page")
	}
	if detail {
		n, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 32)
		if err != nil || n < 1 {
			http.NotFound(w, r)
			return
		}
		params.Set("pr", strconv.FormatInt(n, 10))
	} else {
		params.Del("pr")
	}
	// Read the cursor before the page state, so any later commit produces an
	// event the browser's stream will replay from this position.
	var cursor realtime.Cursor
	if h.live != nil {
		cursor = h.live.Cursor()
	}
	page, err := h.page(r.Context(), user, id, params)
	if h.live != nil {
		page.LiveURL = pages.RepoURL(id) + "/events?since=" + url.QueryEscape(cursor.String())
	}
	if errors.Is(err, installations.ErrWorkspaceInput) {
		http.Error(w, "Invalid pull request filters.", http.StatusBadRequest)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	partial := r.Header.Get("HX-Request") == "true"
	w.Header().Add("Vary", "HX-Request, HX-Target")
	if requested := params.Get("pr_page"); requested != "" && requested != strconv.FormatInt(page.Number, 10) || r.URL.Query().Has("state") {
		location := page.CurrentURL()
		if partial {
			w.Header().Set("HX-Replace-Url", location)
		} else {
			http.Redirect(w, r, location, http.StatusSeeOther)
			return
		}
	}
	if partial && r.Header.Get("HX-Target") == "pull-request-results" {
		templ.Handler(pages.Results(page)).ServeHTTP(w, r)
		return
	}
	if partial && r.Header.Get("HX-Target") == "repository-detail" {
		templ.Handler(pages.Panel(page, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
		return
	}
	username, _ := h.users.Username(r.Context())
	templ.Handler(pages.Index(username, h.users.CSRFToken(r.Context()), page)).ServeHTTP(w, r)
}

// events streams invalidation signals for one owned repository. Browsers react
// by refetching through the ordinary page handlers; no data travels here.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		http.NotFound(w, r)
		return
	}
	user, id, err := h.identity(r)
	if err == nil {
		err = h.repo.Owns(r.Context(), user, id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sub := h.live.Subscribe(RepositoryTopic(id), realtime.RequestCursor(r))
	defer sub.Close()
	if err := realtime.Stream(w, r, sub, realtime.DefaultStreamOptions); err != nil && r.Context().Err() == nil {
		h.logger.WarnContext(r.Context(), "repository event stream ended", "error", err)
	}
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	user, id, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Authorization precedes capability disclosure.
	if _, err := h.repo.Page(r.Context(), user, id, Filter{Page: 1, State: "open"}); err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.enabled {
		http.Error(w, "Configure the GitHub App on the server and worker before discovery.", http.StatusServiceUnavailable)
		return
	}
	if err := h.sync.Request(r.Context(), user, id); err != nil {
		h.fail(w, r, err)
		return
	}
	h.finishMutation(w, r, user, id)
}
func (h *Handler) setPolicy(w http.ResponseWriter, r *http.Request) {
	user, id, err := h.identity(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	_, err = h.policy.Set(r.Context(), user, id, installations.BuildMode(r.PostForm.Get("mode")))
	if errors.Is(err, installations.ErrInvalidBuildMode) {
		http.Error(w, "Choose Manual or Automatic.", http.StatusBadRequest)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.finishMutation(w, r, user, id)
}
func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.users.UserIDs(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := parseFilter(r)
	if err != nil {
		http.Error(w, "Invalid page.", http.StatusBadRequest)
		return
	}
	var repoID int64
	if v := r.URL.Query().Get("repo_id"); v != "" {
		repoID, err = strconv.ParseInt(v, 10, 64)
		if err != nil || repoID <= 0 {
			http.Error(w, "Invalid repository.", http.StatusBadRequest)
			return
		}
	}
	isEnv := r.URL.Path == "/environments"
	data, err := h.repo.Activity(r.Context(), user, repoID, f.Page, isEnv)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page := pages.Activity{Number: data.Page, Pages: data.Pages, Total: data.Total, RepoID: repoID, Environments: isEnv}
	for _, row := range data.Rows {
		page.Rows = append(page.Rows, h.activityRow(row, isEnv))
	}
	if page.Number != f.Page {
		http.Redirect(w, r, page.PageURL(page.Number), http.StatusSeeOther)
		return
	}
	username, _ := h.users.Username(r.Context())
	if isEnv {
		templ.Handler(environmentpages.Index(username, h.users.CSRFToken(r.Context()), page)).ServeHTTP(w, r)
	} else {
		templ.Handler(buildpages.Index(username, h.users.CSRFToken(r.Context()), page)).ServeHTTP(w, r)
	}
}

// Highlights are the Overview's latest build requests and published previews,
// up to limit of each, mapped like the Builds and Environments rows.
func (h *Handler) Highlights(ctx context.Context, userID int64, limit int) (builds, previews []pages.ActivityRow, err error) {
	recent, err := h.repo.Activity(ctx, userID, 0, 1, false)
	if err != nil {
		return nil, nil, err
	}
	for _, row := range recent.Rows[:min(limit, len(recent.Rows))] {
		builds = append(builds, h.activityRow(row, false))
	}
	live, err := h.repo.Activity(ctx, userID, 0, 1, true)
	if err != nil {
		return nil, nil, err
	}
	for _, row := range live.Rows {
		if out := h.activityRow(row, true); out.Deployed != "" && out.State != "tearing_down" && len(previews) < limit {
			previews = append(previews, out)
		}
	}
	return builds, previews, nil
}

// activityRow maps one Builds or Environments row to display data.
func (h *Handler) activityRow(row dbgen.ActivityRow, deployments bool) pages.ActivityRow {
	available := row.Accessible && row.InstallationStatus == "active"
	out := pages.ActivityRow{RepoID: row.RepoID, Number: row.Number, Name: row.FullName, Title: row.Title, Latest: row.HeadSha,
		Requested: row.RequestedSha, Deployed: row.DeployedSha, State: row.BuildState, Deadline: timestamp(row.AvailableAt), Available: available,
		RequestCurrent: row.RequestCurrent, RequestSource: row.RequestSource,
		StartedAt: timestamp(row.StartedAt), CompletedAt: timestamp(row.CompletedAt), PublishedAt: timestamp(row.PublishedAt)}
	if deployments {
		out.State = row.EnvironmentStatus
	}
	live := previewDeployed(row.DeployedSha, row.EnvironmentStatus)
	if live {
		out.OlderPreview = row.DeployedSha != row.HeadSha
		out.PreviewURL = h.previewURL(row.Hostname)
		if row.ExpiresAt.Valid {
			out.ExpiresAt = row.ExpiresAt.Time.UTC().Format(time.RFC3339)
		}
	}
	if row.WaitingReason != "" && (row.BuildState == "pending" || row.BuildState == "queued" || row.BuildState == "running") {
		out.Waiting = environments.WaitingTitle(row.WaitingReason)
	}
	switch {
	case !deployments && row.BuildState == "failed":
		code := ""
		if row.FailureCurrent {
			code = row.FailureCode
		}
		out.Failure, out.FailureCode = environments.FailureMessage(code), code
	case deployments && row.EnvironmentStatus == "failed":
		out.Failure, out.FailureCode = environments.FailureMessage(row.FailureCode), row.FailureCode
	}
	// Environments offers teardown for published previews; an unfinished
	// build is cancelled from its pull request page.
	out.Teardown = deployments && live && h.actions != nil && available && row.PrState == "open"
	if deployments && !live {
		out.PublishedAt = ""
	}
	return out
}
