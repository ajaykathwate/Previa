package reviews

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/internal/platform/realtime"
	pages "github.com/ajaykathwatee/previa/web/pages/reviews"
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

// Handler serves review pages and their forms. Every form is an ordinary POST
// that redirects back on success, so the review works without JavaScript; a
// problem shows the page again with the person's draft.
type Handler struct {
	service *Service
	users   UserSession
	live    Cursor
	logger  *slog.Logger
	baseURL string // the dashboard's origin, for Copy review link
}

// NewHandler serves reviews. live may be nil: pages then refresh on
// navigation only. baseURL is APP_BASE_URL.
func NewHandler(service *Service, users UserSession, live Cursor, logger *slog.Logger, baseURL string) *Handler {
	return &Handler{service: service, users: users, live: live, logger: logger, baseURL: strings.TrimRight(baseURL, "/")}
}

// Mount registers the routes for signed-in people. The workspace middleware
// authorizes repository paths first; every query checks access again.
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		const base = "/repositories/{repoID}/pull-requests/{number}/review"
		r.Get(base, h.page)
		r.Post(base+"/instructions", h.saveInstructions)
		r.Post(base+"/findings", h.createFinding)
		r.Post(base+"/findings/{finding}/replies", h.reply)
		r.Post(base+"/findings/{finding}/messages/{position}/edit", h.edit)
		r.Post(base+"/findings/{finding}/messages/{position}/remove", h.removeMessage)
		r.Post(base+"/findings/{finding}/resolve", h.resolve)
		r.Post(base+"/findings/{finding}/reopen", h.reopen)
		r.Post(base+"/findings/{finding}/remove", h.removeFinding)
	})
}

// actor reads who is changing which pull request's review.
func (h *Handler) actor(r *http.Request) (Actor, bool) {
	repo, err := publicid.Param(r, "repoID", "repository")
	if err != nil || repo <= 0 {
		return Actor{}, false
	}
	number, err := publicid.PRNumber(r, repo)
	if err != nil || number <= 0 || number > 1<<31-1 {
		return Actor{}, false
	}
	user, _, ok := h.users.UserIDs(r.Context())
	return Actor{UserID: user, RepoID: repo, Number: int32(number)}, ok && user > 0
}

// pathNumber reads a positive small number from the route.
func pathNumber(r *http.Request, name string) int32 {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	if err != nil || n <= 0 {
		return 0
	}
	return int32(n)
}

// parseQuery reads the list a page shows from an address query.
func parseQuery(r *http.Request, v url.Values) (Query, bool) {
	q := Query{State: v.Get("state"), Search: strings.TrimSpace(v.Get("q")), Page: 1}
	switch q.State {
	case "", pages.StateOpen, pages.StateResolved, pages.StateAll:
	default:
		return q, false
	}
	if utf8.RuneCountInString(q.Search) > MaxSearchRunes {
		return q, false
	}
	if key := v.Get("author"); key != "" {
		id, err := publicid.Value(r, "user", key, 0)
		if err != nil || id <= 0 {
			return q, false
		}
		q.Author, q.AuthorKey = id, key
	}
	for name, dst := range map[string]*int32{"deployment": &q.Deployment, "finding": &q.Finding} {
		if raw := v.Get(name); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 32)
			if err != nil || n <= 0 {
				return q, false
			}
			*dst = int32(n)
		}
	}
	if raw := v.Get("page"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n <= 0 {
			return q, false
		}
		q.Page = n
	}
	return q, true
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	query, ok := parseQuery(r, r.URL.Query())
	if !ok {
		http.Error(w, "Invalid review filters.", http.StatusBadRequest)
		return
	}
	// Read the cursor before the state, so a change committed in between is
	// replayed by the page's stream.
	var cursor realtime.Cursor
	if h.live != nil {
		cursor = h.live.Cursor()
	}
	p, err := h.service.Page(r.Context(), a.UserID, a.RepoID, a.Number, query)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if h.live != nil {
		p.LiveURL = "/repositories/" + strconv.FormatInt(a.RepoID, 10) + "/events?since=" + url.QueryEscape(cursor.String())
	}
	p.Toast, p.Notice = outcome(r.URL.Query())
	p.Edit = editTarget(p, r.URL.Query().Get("edit"))
	p.Remove = removeTarget(p, r.URL.Query().Get("remove"))
	h.render(w, r, http.StatusOK, p)
}

// render finishes and writes a review page: the live region alone for a live
// refresh, otherwise the whole page.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, p pages.Page) {
	p.CSRF = h.users.CSRFToken(r.Context())
	if address, err := publicid.URL(r, p.URL()); err == nil && h.baseURL != "" {
		p.AbsoluteURL = h.baseURL + address
	}
	w.Header().Add("Vary", "HX-Request, HX-Target")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "review-detail" {
		publicid.Handler(r, pages.Detail(p), p).ServeHTTP(w, r)
		return
	}
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, pages.Index(username, p), p).ServeHTTP(w, r)
}

// again shows the page again after a failed form, with the draft and the
// list the form came from.
func (h *Handler) again(w http.ResponseWriter, r *http.Request, a Actor, status int, draft *pages.Draft) {
	values, _ := url.ParseQuery(r.PostFormValue("return_query"))
	query, ok := parseQuery(r, values)
	if !ok {
		query = Query{}
	}
	p, err := h.service.Page(r.Context(), a.UserID, a.RepoID, a.Number, query)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.Draft = draft
	h.render(w, r, status, p)
}

// back returns to the review after a successful change.
func (h *Handler) back(w http.ResponseWriter, r *http.Request, a Actor, saved, anchor string, keepList bool) {
	values := url.Values{}
	if keepList {
		values, _ = url.ParseQuery(r.PostFormValue("return_query"))
		if _, ok := parseQuery(r, values); !ok {
			values = url.Values{}
		}
	}
	values.Set("saved", saved)
	location := pages.URL(a.RepoID, a.Number) + "?" + values.Encode()
	if anchor != "" {
		location += "#" + anchor
	}
	publicid.Redirect(w, r, location, http.StatusSeeOther)
}

// notice returns to the review with an explanation instead of the change.
func (h *Handler) notice(w http.ResponseWriter, r *http.Request, a Actor, code string, extra url.Values) {
	values, _ := url.ParseQuery(r.PostFormValue("return_query"))
	if _, ok := parseQuery(r, values); !ok {
		values = url.Values{}
	}
	values.Set("notice", code)
	for k, v := range extra {
		values[k] = v
	}
	publicid.Redirect(w, r, pages.URL(a.RepoID, a.Number)+"?"+values.Encode(), http.StatusSeeOther)
}

// fail answers errors that leave nothing to show again.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	h.logger.ErrorContext(r.Context(), "review request failed", "error", err)
	http.Error(w, "The review is temporarily unavailable. Please try again.", http.StatusServiceUnavailable)
}

// refused handles a change the service refused; it reports false when err
// needs the caller's own handling (a form problem or conflict).
func (h *Handler) refused(w http.ResponseWriter, r *http.Request, a Actor, err error) bool {
	var limit *LimitError
	switch {
	case errors.Is(err, ErrClosed):
		h.notice(w, r, a, "closed", nil)
	case errors.Is(err, ErrGone):
		h.notice(w, r, a, "gone", nil)
	case errors.Is(err, ErrStale):
		h.notice(w, r, a, "stale", nil)
	case errors.As(err, &limit):
		h.notice(w, r, a, "limit", url.Values{"until": {limit.ResetsAt.UTC().Format(time.RFC3339)}})
	default:
		var input *InputError
		var conflict *ConflictError
		if errors.As(err, &input) || errors.As(err, &conflict) {
			return false
		}
		h.fail(w, r, err)
	}
	return true
}

// draftFailure shows the page again for a form problem, a conflict or a
// spent allowance, keeping the draft; other errors are refused as usual.
func (h *Handler) draftFailure(w http.ResponseWriter, r *http.Request, a Actor, draft *pages.Draft, err error) {
	var input *InputError
	var conflict *ConflictError
	var limit *LimitError
	switch {
	case errors.As(err, &input):
		draft.Errors, draft.Error = input.Fields, input.Message
		h.again(w, r, a, http.StatusUnprocessableEntity, draft)
	case errors.As(err, &conflict):
		draft.Conflict = &pages.Conflict{Kind: conflict.Kind, Tested: conflict.Tested, Current: conflict.Current, Saved: conflict.Saved}
		if conflict.Revision > 0 {
			draft.Revision = conflict.Revision
		}
		h.again(w, r, a, http.StatusConflict, draft)
	case errors.As(err, &limit):
		draft.Error = "You made many review changes in the last hour. Try again after " + limit.ResetsAt.UTC().Format("15:04") + " UTC; your text is kept here."
		h.again(w, r, a, http.StatusTooManyRequests, draft)
	default:
		h.refused(w, r, a, err)
	}
}

func (h *Handler) saveInstructions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	revision, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	in := InstructionsInput{
		Summary: r.PostFormValue("summary"), Steps: r.PostFormValue("steps"), Expected: r.PostFormValue("expected"),
		StartPath: r.PostFormValue("start_path"), DocsPath: r.PostFormValue("docs_path"), Revision: revision,
	}
	if err := h.service.SaveInstructions(r.Context(), a, in); err != nil {
		h.draftFailure(w, r, a, &pages.Draft{Form: "instructions", Revision: revision, Instructions: pages.Instructions{
			Summary: in.Summary, Steps: in.Steps, Expected: in.Expected, StartPath: in.StartPath, DocsPath: in.DocsPath,
		}}, err)
		return
	}
	h.back(w, r, a, "instructions", "instructions", true)
}

func (h *Handler) createFinding(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// After a version conflict the person picks a deployment: the newest
	// (deployment) or, on purpose, the one they were testing (older_deployment).
	shown, older := formNumber(r, "deployment"), false
	if r.PostForm.Has("older_deployment") {
		shown, older = formNumber(r, "older_deployment"), true
	}
	in := FindingInput{
		Body: r.PostFormValue("body"), Path: r.PostFormValue("path"), Details: r.PostFormValue("details"),
		RequestKey: r.PostFormValue("request_key"), Deployment: shown, AcknowledgeOlder: older,
	}
	number, err := h.service.CreateFinding(r.Context(), a, in)
	if err != nil {
		h.draftFailure(w, r, a, &pages.Draft{Form: "finding", Body: in.Body, Path: in.Path, Details: in.Details,
			RequestKey: keyOrNew(in.RequestKey), Deployment: shown}, err)
		return
	}
	h.back(w, r, a, "finding", pages.FindingAnchor(number), false)
}

func (h *Handler) reply(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(r)
	finding := pathNumber(r, "finding")
	if !ok || finding == 0 {
		http.NotFound(w, r)
		return
	}
	in := ReplyInput{Body: r.PostFormValue("body"), RequestKey: r.PostFormValue("request_key"), Deployment: formNumber(r, "deployment")}
	position, err := h.service.Reply(r.Context(), a, finding, in)
	if err != nil {
		h.draftFailure(w, r, a, &pages.Draft{Form: "reply", Finding: finding, Body: in.Body, RequestKey: keyOrNew(in.RequestKey), Deployment: in.Deployment}, err)
		return
	}
	h.back(w, r, a, "reply", pages.MessageAnchor(finding, position), true)
}

func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(r)
	finding, position := pathNumber(r, "finding"), pathNumber(r, "position")
	if !ok || finding == 0 || position == 0 {
		http.NotFound(w, r)
		return
	}
	revision, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	in := EditInput{Body: r.PostFormValue("body"), Path: r.PostFormValue("path"), Details: r.PostFormValue("details"), Revision: revision}
	if err := h.service.EditMessage(r.Context(), a, finding, position, in); err != nil {
		h.draftFailure(w, r, a, &pages.Draft{Form: "edit", Finding: finding, Position: position, Body: in.Body, Path: in.Path, Details: in.Details, Revision: revision}, err)
		return
	}
	h.back(w, r, a, "edited", pages.MessageAnchor(finding, position), true)
}

func (h *Handler) removeMessage(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(r)
	finding, position := pathNumber(r, "finding"), pathNumber(r, "position")
	if !ok || finding == 0 || position == 0 {
		http.NotFound(w, r)
		return
	}
	whole, err := h.service.RemoveMessage(r.Context(), a, finding, position, r.PostFormValue("reason"))
	if err != nil {
		h.removalFailure(w, r, a, finding, position, err)
		return
	}
	if whole {
		h.back(w, r, a, "removed-finding", "", true)
		return
	}
	h.back(w, r, a, "removed", pages.MessageAnchor(finding, position), true)
}

func (h *Handler) removeFinding(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(r)
	finding := pathNumber(r, "finding")
	if !ok || finding == 0 {
		http.NotFound(w, r)
		return
	}
	if err := h.service.RemoveFinding(r.Context(), a, finding, r.PostFormValue("reason")); err != nil {
		h.removalFailure(w, r, a, finding, 0, err)
		return
	}
	h.back(w, r, a, "removed-finding", "", true)
}

// removalFailure shows the removal confirmation again when it lacked a
// moderation reason.
func (h *Handler) removalFailure(w http.ResponseWriter, r *http.Request, a Actor, finding, position int32, err error) {
	var input *InputError
	if !errors.As(err, &input) {
		h.refused(w, r, a, err)
		return
	}
	values, _ := url.ParseQuery(r.PostFormValue("return_query"))
	query, ok := parseQuery(r, values)
	if !ok {
		query = Query{}
	}
	p, err := h.service.Page(r.Context(), a.UserID, a.RepoID, a.Number, query)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	target := strconv.Itoa(int(finding))
	if position > 0 {
		target += "." + strconv.Itoa(int(position))
	}
	p.Remove = removeTarget(p, target)
	p.Notice = pages.Notice{Tone: "warn", Text: input.Message}
	h.render(w, r, http.StatusUnprocessableEntity, p)
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) { h.changeState(w, r, true) }
func (h *Handler) reopen(w http.ResponseWriter, r *http.Request)  { h.changeState(w, r, false) }

func (h *Handler) changeState(w http.ResponseWriter, r *http.Request, resolve bool) {
	a, ok := h.actor(r)
	finding := pathNumber(r, "finding")
	if !ok || finding == 0 {
		http.NotFound(w, r)
		return
	}
	revision, _ := strconv.ParseInt(r.PostFormValue("revision"), 10, 64)
	var err error
	saved := "reopened"
	if resolve {
		saved = "resolved"
		err = h.service.Resolve(r.Context(), a, finding, revision, formNumber(r, "deployment"))
	} else {
		err = h.service.Reopen(r.Context(), a, finding, revision)
	}
	if err != nil {
		h.refused(w, r, a, err)
		return
	}
	h.back(w, r, a, saved, pages.FindingAnchor(finding), true)
}

// outcome turns a redirect's query into what the page says happened.
func outcome(q url.Values) (string, pages.Notice) {
	toast := map[string]string{
		"instructions": "Testing instructions saved", "finding": "Finding added", "reply": "Reply added",
		"edited": "Message updated", "removed": "Message removed", "removed-finding": "Finding removed",
		"resolved": "Finding resolved", "reopened": "Finding reopened",
	}[q.Get("saved")]
	var notice pages.Notice
	switch q.Get("notice") {
	case "closed":
		notice = pages.Notice{Tone: "info", Text: "This pull request is closed, so its review is read-only. Nothing was changed."}
	case "gone":
		notice = pages.Notice{Tone: "info", Text: "That finding or message was removed meanwhile. Nothing was changed."}
	case "stale":
		notice = pages.Notice{Tone: "info", Text: "Someone changed that finding meanwhile. This is its latest state; nothing was changed."}
	case "limit":
		text := "You made many review changes in the last hour. Try again later."
		if until, err := time.Parse(time.RFC3339, q.Get("until")); err == nil {
			text = "You made many review changes in the last hour. Try again after " + until.UTC().Format("15:04") + " UTC."
		}
		notice = pages.Notice{Tone: "warn", Text: text}
	}
	return toast, notice
}

// editTarget keeps a no-JavaScript edit request only for something the
// viewer may edit.
func editTarget(p pages.Page, target string) string {
	if target == "instructions" {
		if p.CanOperate && !p.Closed() {
			return target
		}
		return ""
	}
	for _, t := range p.Findings.Threads {
		for _, m := range t.Messages {
			if m.CanEdit && target == strconv.Itoa(int(t.Number))+"."+strconv.Itoa(int(m.Position)) {
				return target
			}
		}
	}
	return ""
}

// removeTarget keeps a no-JavaScript removal request only for something the
// viewer may remove: "finding" or "finding.position".
func removeTarget(p pages.Page, target string) *pages.RemoveTarget {
	number, position, _ := strings.Cut(target, ".")
	for _, t := range p.Findings.Threads {
		if strconv.Itoa(int(t.Number)) != number {
			continue
		}
		if position == "" {
			if t.CanRemove {
				return &pages.RemoveTarget{Finding: t.Number, Moderate: t.Moderate, Action: p.FindingAction(t.Number, "/remove")}
			}
			return nil
		}
		for _, m := range t.Messages {
			if strconv.Itoa(int(m.Position)) == position && m.CanRemove {
				return &pages.RemoveTarget{Finding: t.Number, Position: m.Position, Moderate: m.Moderate, Action: p.MessageAction(t.Number, m.Position, "/remove")}
			}
		}
	}
	return nil
}

func formNumber(r *http.Request, name string) int32 {
	n, err := strconv.ParseInt(r.PostFormValue(name), 10, 32)
	if err != nil || n < 0 {
		return 0
	}
	return int32(n)
}

// NewRequestKey is a form's idempotency key: a repeated submission of the
// same form creates one finding or reply.
func NewRequestKey() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func keyOrNew(key string) string {
	if validRequestKey(key) {
		return key
	}
	return NewRequestKey()
}
