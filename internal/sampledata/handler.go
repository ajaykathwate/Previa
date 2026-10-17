package sampledata

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	"github.com/ajaykathwatee/previa/internal/sampledata/dbgen"
	"github.com/ajaykathwatee/previa/web/components/logviewer"
	pages "github.com/ajaykathwatee/previa/web/pages/sampledata"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// UserSession is the signed-in person; the auth feature implements it.
type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// Cursor marks the realtime position a page was read at; *realtime.Hub
// implements it.
type Cursor interface {
	Cursor() realtime.Cursor
}

// LogLine is one stored line of a refresh's build log.
type LogLine struct {
	Sequence int64
	System   bool
	Text     string
	At       time.Time
}

// Logs reads refresh build logs; internal/app adapts the build log store.
// source is "", "all", "previa" or "application"; query is literal text.
type Logs interface {
	LinesBefore(ctx context.Context, refreshID, before int64, limit int, source, query string) ([]LogLine, error)
	LinesFrom(ctx context.Context, refreshID, from int64, limit int) ([]LogLine, error)
}

// Handler serves the Test data tab, its forms and refresh logs. Forms are
// CSRF-protected by the session middleware; destructive ones are confirmed in
// modals.
type Handler struct {
	service *Service
	logs    Logs
	users   UserSession
	live    Cursor
	logger  *slog.Logger
	now     func() time.Time
}

// NewHandler serves the Test data tab. live may be nil: the tab then
// refreshes on navigation only.
func NewHandler(service *Service, logs Logs, users UserSession, live Cursor, logger *slog.Logger) *Handler {
	return &Handler{service: service, logs: logs, users: users, live: live, logger: logger, now: time.Now}
}

// Mount registers the routes for signed-in people. The workspace middleware
// authorizes repository paths first; every query checks access again.
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/repositories/{repoID}/test-data", h.page)
		r.Post("/repositories/{repoID}/test-data/settings", h.save)
		r.Post("/repositories/{repoID}/test-data/off", h.turnOff)
		r.Post("/repositories/{repoID}/test-data/refresh", h.refresh)
		r.Post("/repositories/{repoID}/test-data/delete", h.delete)
		r.Get("/repositories/{repoID}/test-data/refreshes/{number}/logs", h.log)
		r.Get("/repositories/{repoID}/test-data/refreshes/{number}/logs/download", h.download)
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

func (h *Handler) failed(w http.ResponseWriter, r *http.Request, err error, what string) {
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	h.logger.ErrorContext(r.Context(), what, "error", err)
	http.Error(w, "Test data is temporarily unavailable. Please try again.", http.StatusServiceUnavailable)
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := pages.Page{}
	p.Toast, p.Notice = outcome(r.URL.Query())
	h.render(w, r, user, repo, http.StatusOK, p)
}

// render reads the tab and writes it, or only its live part for a live
// refresh.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, user, repo int64, status int, p pages.Page) {
	// Read the cursor before the state, so a change committed in between is
	// replayed by the page's stream.
	var cursor realtime.Cursor
	if h.live != nil {
		cursor = h.live.Cursor()
	}
	o, err := h.service.Overview(r.Context(), user, repo)
	if err != nil {
		h.failed(w, r, err, "load test data")
		return
	}
	h.fill(&p, o)
	p.CSRF = h.users.CSRFToken(r.Context())
	if h.live != nil {
		p.LiveURL = "/repositories/" + strconv.FormatInt(repo, 10) + "/events?since=" + url.QueryEscape(cursor.String())
	}
	w.Header().Add("Vary", "HX-Request, HX-Target")
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "test-data-detail" {
		publicid.Handler(r, pages.Detail(p), p, templ.WithStatus(status)).ServeHTTP(w, r)
		return
	}
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, pages.Index(username, p), p, templ.WithStatus(status)).ServeHTTP(w, r)
}

// fill turns the overview into display data.
func (h *Handler) fill(p *pages.Page, o Overview) {
	now := h.now()
	s := o.Settings
	p.RepoID, p.Name = o.Scope.RepoID, o.Scope.FullName
	p.Manager, p.Operator, p.Available = o.Scope.CanManage, true, o.Scope.Available
	p.Databases, p.Baselines = o.Offer.Databases, o.Offer.Baselines
	p.Settings = pages.Settings{
		Enabled: s.Enabled, KeepData: s.KeepData, UseBaseline: s.UseBaseline, AutoRefresh: s.AutoRefresh,
		Branch: s.Branch, Revision: s.Revision,
	}
	if s.Branch == "" {
		p.Settings.DefaultBranch = s.CheckedBranch
	}
	if !s.UpdatedAt.IsZero() {
		p.Settings.UpdatedAt = s.UpdatedAt.UTC().Format(time.RFC3339)
	}
	var current int64
	for _, b := range o.Baselines {
		current = b.Sequence
		p.Current = append(p.Current, pages.Baseline{
			Service: pages.ServiceTitle(b.Kind, b.Version), Branch: b.Branch, Commit: b.CommitSHA,
			BuiltAt: b.BuiltAt.UTC().Format(time.RFC3339), Size: pages.Size(b.Size),
		})
	}
	if s.Enabled && s.UseBaseline {
		p.Moved = moved(s, o.Baselines)
	}
	for _, r := range o.Refreshes {
		p.Refreshes = append(p.Refreshes, refreshView(r, current, now))
	}
}

// moved says when the baseline no longer matches its branch.
func moved(s Settings, baselines []Baseline) string {
	hint := ""
	if !s.AutoRefresh {
		hint = " Refresh the baseline to use it."
	}
	switch {
	case len(baselines) == 0:
		return ""
	case s.Branch != "" && s.Branch != baselines[0].Branch:
		return "This baseline was built from " + baselines[0].Branch + "; the baseline branch is now " + s.Branch + "." + hint
	case s.CheckedBranch == "" || s.CheckedAt.Before(baselines[0].BuiltAt):
		return ""
	case s.CheckedSHA == "":
		return "The branch " + s.CheckedBranch + " was not found on GitHub at the last check. Choose another baseline branch."
	case s.CheckedBranch != baselines[0].Branch:
		return "This baseline was built from " + baselines[0].Branch + "; the default branch is now " + s.CheckedBranch + "." + hint
	case s.CheckedSHA != baselines[0].CommitSHA:
		return s.CheckedBranch + " moved to " + pages.Short(s.CheckedSHA) + " since this baseline." + hint
	}
	return ""
}

func refreshView(r Refresh, current int64, now time.Time) pages.Refresh {
	v := pages.Refresh{Number: r.Sequence, Trigger: pages.TriggerLabel(r.Trigger), Branch: r.Branch, Commit: r.CommitSHA, HasLog: r.HasLog}
	at := func(label string, t time.Time) {
		if !t.IsZero() {
			v.WhenLabel, v.When = label, t.UTC().Format(time.RFC3339)
		}
	}
	switch r.State {
	case StatePending:
		v.Label, v.Tone = pages.LabelWaiting, "tone-warn"
		v.Detail = pages.WaitingText(r.WaitingReason, r.AvailableAt, r.Attempts, now)
		at("Requested", r.RequestedAt)
	case StateRunning:
		v.Label, v.Tone = pages.LabelBuilding, "tone-info"
		at("Started", r.StartedAt)
	case StateSucceeded:
		v.Label, v.Tone = pages.LabelReady, "tone-ok"
		if r.Sequence != current {
			v.Label, v.Tone = "Replaced", "tone-neutral"
		}
		at("Finished", r.FinishedAt)
		v.Took = pages.Took(r.BuildSeconds)
	case StateFailed:
		v.Label, v.Tone = pages.LabelFailed, "tone-danger"
		v.Detail = pages.FailureTitle(r.FailureCode)
		if r.FailureDetail != "" {
			v.Detail += ": " + r.FailureDetail
		}
		at("Finished", r.FinishedAt)
		v.Took = pages.Took(r.BuildSeconds)
	default:
		v.Label, v.Tone = pages.LabelCancelled, "tone-neutral"
		v.Detail = "Not used: Test data, its baseline or the branch changed meanwhile."
		at("Finished", r.FinishedAt)
	}
	return v
}

// outcome turns a redirect's query into what the tab says happened.
func outcome(q url.Values) (string, pages.Notice) {
	toast := map[string]string{
		"settings": "Test data settings saved", "on": "Test data is on", "off": "Test data is off and its baselines are deleted",
		"refresh": "Baseline refresh requested", "deleted": "Baseline deleted",
	}[q.Get("saved")]
	var notice pages.Notice
	switch q.Get("notice") {
	case "conflict":
		notice = pages.Notice{Tone: "info", Text: "These settings changed meanwhile. Review them and try again."}
	case "baseline":
		notice = pages.Notice{Tone: "info", Text: "Refresh baseline needs Test data with Use a baseline on."}
	case "nothing":
		notice = pages.Notice{Tone: "info", Text: "There was no baseline to delete."}
	case "access":
		notice = pages.Notice{Tone: "warn", Text: "Previa's GitHub access to this repository was removed, so nothing was changed."}
	case "manage":
		notice = pages.Notice{Tone: "warn", Text: "Only Owners and Admins change Test data settings or delete baselines."}
	}
	return toast, notice
}

func (h *Handler) back(w http.ResponseWriter, r *http.Request, repo int64, key, value string) {
	publicid.Redirect(w, r, pages.URL(repo)+"?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}

// settled redirects after an action that could not apply, or reports a
// failure.
func (h *Handler) settled(w http.ResponseWriter, r *http.Request, repo int64, err error, what string) {
	switch {
	case errors.Is(err, ErrConflict):
		h.back(w, r, repo, "notice", "conflict")
	case errors.Is(err, ErrNoBaseline):
		h.back(w, r, repo, "notice", "baseline")
	case errors.Is(err, ErrAccessRemoved):
		h.back(w, r, repo, "notice", "access")
	case errors.Is(err, ErrForbidden):
		h.back(w, r, repo, "notice", "manage")
	default:
		h.failed(w, r, err, what)
	}
}

func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	revision, err := strconv.ParseInt(r.PostForm.Get("revision"), 10, 64)
	if err != nil || revision < 0 {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	on := func(name string) bool { return r.PostForm.Get(name) == "on" }
	form := Form{Revision: revision, Enabled: on("enabled"), KeepData: on("keep_data"), UseBaseline: on("use_baseline"), AutoRefresh: on("auto_refresh"),
		Branch: r.PostForm.Get("baseline_branch")}
	before, err := h.service.store.Settings(r.Context(), repo)
	if err != nil {
		h.failed(w, r, err, "read test data settings")
		return
	}
	saved, err := h.service.Save(r.Context(), user, repo, form)
	if httpx.WantsJSON(r) {
		// The in-place branch field saves without leaving the page.
		h.savedJSON(w, r, repo, saved, err)
		return
	}
	var invalid *InvalidError
	switch {
	case errors.As(err, &invalid):
		h.render(w, r, user, repo, http.StatusUnprocessableEntity, pages.Page{Error: pages.Field{Name: invalid.Field, Message: invalid.Message, Value: form.Branch}})
		return
	case err != nil:
		h.settled(w, r, repo, err, "save test data settings")
		return
	}
	h.logger.InfoContext(r.Context(), "test data settings saved", "repo_id", repo, "enabled", saved.Enabled, "use_baseline", saved.UseBaseline)
	result := "settings"
	if saved.Enabled && !before.Enabled {
		result = "on"
	}
	h.back(w, r, repo, "saved", result)
}

// savedJSON answers an in-place save; the page's live update then refreshes
// the forms with the new revision.
func (h *Handler) savedJSON(w http.ResponseWriter, r *http.Request, repo int64, saved Settings, err error) {
	var invalid *InvalidError
	switch {
	case err == nil:
		h.logger.InfoContext(r.Context(), "test data settings saved", "repo_id", repo, "enabled", saved.Enabled, "use_baseline", saved.UseBaseline)
		httpx.WriteJSON(w, http.StatusOK, httpx.FormResult{Message: "Test data settings saved", Revision: saved.Revision,
			Fields: map[string]string{"baseline_branch": saved.Branch}})
	case errors.As(err, &invalid):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.FormResult{Error: invalid.Message})
	case errors.Is(err, ErrConflict):
		httpx.WriteJSON(w, http.StatusConflict, httpx.FormResult{Error: "These settings changed meanwhile. Reload the page, then try again."})
	case errors.Is(err, ErrAccessRemoved):
		httpx.WriteJSON(w, http.StatusConflict, httpx.FormResult{Error: "Previa's GitHub access to this repository was removed."})
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrUnavailable):
		httpx.WriteJSON(w, http.StatusForbidden, httpx.FormResult{Error: "Only Owners and Admins change Test data settings."})
	default:
		h.logger.ErrorContext(r.Context(), "save test data settings", "error", err)
		httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.FormResult{Error: "The setting could not be saved. Please try again."})
	}
}

func (h *Handler) turnOff(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	revision, err := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	if err != nil || revision < 0 {
		http.Error(w, "Invalid form.", http.StatusBadRequest)
		return
	}
	retired, err := h.service.TurnOff(r.Context(), user, repo, revision)
	if err != nil {
		h.settled(w, r, repo, err, "turn test data off")
		return
	}
	h.logger.InfoContext(r.Context(), "test data turned off", "repo_id", repo, "baselines", retired)
	h.back(w, r, repo, "saved", "off")
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.service.Refresh(r.Context(), user, repo); err != nil {
		h.settled(w, r, repo, err, "request a baseline refresh")
		return
	}
	h.logger.InfoContext(r.Context(), "baseline refresh requested", "repo_id", repo)
	h.back(w, r, repo, "saved", "refresh")
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	user, repo, ok := h.identity(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	retired, err := h.service.Delete(r.Context(), user, repo)
	switch {
	case err != nil:
		h.settled(w, r, repo, err, "delete baselines")
	case retired == 0:
		h.back(w, r, repo, "notice", "nothing")
	default:
		h.logger.InfoContext(r.Context(), "baselines deleted", "repo_id", repo, "baselines", retired)
		h.back(w, r, repo, "saved", "deleted")
	}
}

// Log pages show at most logPage lines at a time; downloads read in batches.
const (
	logPage       = 500
	downloadBatch = 1000
)

// refreshFor returns the refresh a log request names, for someone who may
// build in the repository.
func (h *Handler) refreshFor(r *http.Request) (Scope, dbgen.BaselineRefresh, error) {
	user, repo, ok := h.identity(r)
	number, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if !ok || err != nil || number <= 0 {
		return Scope{}, dbgen.BaselineRefresh{}, ErrUnavailable
	}
	q := dbgen.New(h.service.store.pool)
	scope, err := h.service.scope(r.Context(), q, user, repo)
	if err != nil {
		return Scope{}, dbgen.BaselineRefresh{}, err
	}
	refresh, err := q.RefreshByNumber(r.Context(), dbgen.RefreshByNumberParams{RepoID: repo, Sequence: number})
	if errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, dbgen.BaselineRefresh{}, ErrUnavailable
	}
	if err != nil {
		return Scope{}, dbgen.BaselineRefresh{}, fmt.Errorf("read the refresh: %w", err)
	}
	return scope, refresh, nil
}

func logFilter(q url.Values) (source, query string, ok bool) {
	source, query = q.Get("source"), q.Get("q")
	switch source {
	case "", "all", "previa", "application":
	default:
		return "", "", false
	}
	return source, query, utf8.RuneCountInString(query) <= 200 && !strings.ContainsRune(query, 0)
}

func (h *Handler) log(w http.ResponseWriter, r *http.Request) {
	scope, refresh, err := h.refreshFor(r)
	if err != nil {
		h.failed(w, r, err, "load a baseline refresh log")
		return
	}
	source, query, ok := logFilter(r.URL.Query())
	if !ok {
		http.Error(w, "Invalid log filter.", http.StatusBadRequest)
		return
	}
	before := refresh.LogLines
	if value := r.URL.Query().Get("before"); value != "" {
		if before, err = strconv.ParseInt(value, 10, 64); err != nil || before < 0 {
			http.Error(w, "Invalid log position.", http.StatusBadRequest)
			return
		}
	}
	lines, err := h.logs.LinesBefore(r.Context(), refresh.ID, before, logPage, source, query)
	if err != nil {
		h.failed(w, r, err, "read a baseline refresh log")
		return
	}
	base := pages.LogURL(scope.RepoID, refresh.Sequence)
	current, err := h.service.store.Current(r.Context(), scope.RepoID)
	if err != nil {
		h.failed(w, r, err, "read the current baseline")
		return
	}
	var currentSequence int64
	for _, b := range current {
		currentSequence = b.Sequence
	}
	view := refreshView(refreshOf(refresh), currentSequence, h.now())
	p := pages.LogPage{RepoID: scope.RepoID, Name: scope.FullName, Number: refresh.Sequence, Label: view.Label, Tone: view.Tone,
		Branch: refresh.Branch.String, Commit: refresh.CommitSha.String}
	p.Summary = logSummary(refresh, view)
	if refresh.LogTruncated {
		p.Notices = append(p.Notices, "The log reached its limit; later output was not stored.")
	}
	if refresh.LogLost > 0 {
		p.Notices = append(p.Notices, strconv.FormatInt(refresh.LogLost, 10)+" lines could not be stored because log storage was unavailable.")
	}
	if refresh.LogOpen {
		p.Notices = append(p.Notices, "The refresh is still writing this log. Use Refresh for new lines.")
	}
	panel := logviewer.Panel{Source: source, Query: query, BaseURL: base, LatestURL: logviewer.FilterURL(base, source, query),
		DownloadURL: base + "/download", HasOutput: refresh.LogLines > 0, Empty: "No log lines were stored for this refresh."}
	for _, l := range lines {
		panel.Lines = append(panel.Lines, logviewer.Line{Sequence: l.Sequence, System: l.System, Text: l.Text, At: l.At})
	}
	if len(lines) == 0 && refresh.LogLines > 0 && (source == "previa" || source == "application" || query != "") {
		panel.Empty = "No log lines match these filters."
	}
	if panel.EarlierURL, err = h.earlierURL(r.Context(), refresh.ID, lines, base, source, query); err != nil {
		h.failed(w, r, err, "read a baseline refresh log")
		return
	}
	p.Panel = panel
	w.Header().Set("Cache-Control", "no-store")
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, pages.Log(username, h.users.CSRFToken(r.Context()), p), p).ServeHTTP(w, r)
}

// earlierURL pages back from the first line shown, when earlier lines match.
func (h *Handler) earlierURL(ctx context.Context, refreshID int64, lines []LogLine, base, source, query string) (string, error) {
	if len(lines) == 0 || lines[0].Sequence == 0 {
		return "", nil
	}
	earlier, err := h.logs.LinesBefore(ctx, refreshID, lines[0].Sequence, 1, source, query)
	if err != nil || len(earlier) == 0 {
		return "", err
	}
	v := url.Values{"before": {strconv.FormatInt(lines[0].Sequence, 10)}}
	if source != "" && source != "all" {
		v.Set("source", source)
	}
	if query != "" {
		v.Set("q", query)
	}
	return base + "?" + v.Encode(), nil
}

func logSummary(refresh dbgen.BaselineRefresh, view pages.Refresh) string {
	target := ""
	if refresh.CommitSha.Valid {
		target = " " + refresh.Branch.String + "@" + pages.Short(refresh.CommitSha.String)
	}
	switch refresh.State {
	case StatePending:
		return view.Detail
	case StateRunning:
		return "Building the baseline from" + cmp.Or(target, " its branch") + "."
	case StateSucceeded:
		return "Built and captured the baseline from" + cmp.Or(target, " its branch") + "."
	case StateFailed:
		return view.Detail
	}
	return view.Detail
}

// download sends a refresh's whole log as a text file, in bounded reads.
// Lines were redacted before they were stored.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	scope, refresh, err := h.refreshFor(r)
	if err != nil {
		h.failed(w, r, err, "download a baseline refresh log")
		return
	}
	_, name, _ := strings.Cut(scope.FullName, "/")
	file := fmt.Sprintf("%s-baseline-refresh-%d.log", downloadName.ReplaceAllString(name, "-"), refresh.Sequence)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for next := int64(0); next < refresh.LogLines; {
		lines, err := h.logs.LinesFrom(r.Context(), refresh.ID, next, downloadBatch)
		if err != nil {
			h.logger.WarnContext(r.Context(), "baseline log download ended early", "error", err)
			return
		}
		if len(lines) == 0 {
			return
		}
		var b strings.Builder
		for _, l := range lines {
			display := logviewer.Line{Text: l.Text, At: l.At}
			b.WriteString(display.Timestamp())
			b.WriteByte(' ')
			b.WriteString(display.Message())
			b.WriteByte('\n')
		}
		if _, err := w.Write([]byte(b.String())); err != nil {
			return
		}
		next = lines[len(lines)-1].Sequence + 1
	}
}

var downloadName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
