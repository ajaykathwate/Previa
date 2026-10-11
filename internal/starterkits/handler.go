package starterkits

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/ajaykathwatee/previa/internal/platform/workers"
	kitpages "github.com/ajaykathwatee/previa/web/pages/starterkits"
)

// Capabilities says what the running workers offer; *workers.Reader in
// production.
type Capabilities interface {
	Current(context.Context) (workers.Capabilities, error)
}

// UserSession is the signed-in user, for the page shell.
type UserSession interface {
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
}

// Validator checks a generated .previewrc.yaml with the rules a build
// applies, against the services offered; repoconfig in production.
type Validator func(config []byte, offer Offer) []Problem

// Handler serves the Starter kits page and its file downloads.
type Handler struct {
	users    UserSession
	caps     Capabilities
	fallback Offer
	validate Validator
	logger   *slog.Logger
}

// NewHandler builds the handler. fallback is the service versions to offer
// when no worker that builds is running.
func NewHandler(users UserSession, caps Capabilities, fallback Offer, validate Validator, logger *slog.Logger) *Handler {
	return &Handler{users: users, caps: caps, fallback: fallback, validate: validate, logger: logger}
}

// Mount registers the page for signed-in users. Downloads and checks are
// POSTs because they carry the editor's text, edits included.
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/starter-kits", h.page)
		r.Post("/starter-kits/download/{file}", h.download)
		r.Post("/starter-kits/check", h.check)
	})
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	offer := h.offer(r.Context())
	kit := h.generate(r.URL.Query(), offer)
	username, _ := h.users.Username(r.Context())
	p := view(kit, offer)
	p.CSRF = h.users.CSRFToken(r.Context())
	templ.Handler(kitpages.Index(username, p)).ServeHTTP(w, r)
}

// maxFileBytes bounds an edited file; a build reads at most this much of
// .previewrc.yaml, and no Dockerfile a kit starts from comes close.
const maxFileBytes = 64 * 1024

// fileNames are the files a kit has, by download path.
var fileNames = map[string]string{"dockerfile": "Dockerfile", "previewrc": ".previewrc.yaml"}

// download sends the editor's text back as the named file.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	name, ok := fileNames[chi.URLParam(r, "file")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	content, ok := posted(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(content))
}

// check validates an edited .previewrc.yaml as a build would and answers
// with the editor's status bar.
func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	content, ok := posted(w, r)
	if !ok {
		return
	}
	var problems []kitpages.Problem
	if h.validate != nil {
		for _, problem := range h.validate([]byte(content), h.offer(r.Context())) {
			// The key names the problem: an edited file has no form field.
			problems = append(problems, kitpages.Problem{Field: problem.Field, Label: problem.Field, Message: problem.Message, Line: problem.Line})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	templ.Handler(kitpages.CheckResult(problems)).ServeHTTP(w, r)
}

// posted reads the editor's text. Browsers send a text area's line breaks as
// CRLF; the files use LF.
func posted(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 2*maxFileBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The file could not be read.", http.StatusBadRequest)
		return "", false
	}
	content := strings.ReplaceAll(r.PostForm.Get("content"), "\r\n", "\n")
	if len(content) > maxFileBytes {
		http.Error(w, "The file is larger than 64 KiB.", http.StatusRequestEntityTooLarge)
		return "", false
	}
	return content, true
}

func (h *Handler) generate(q url.Values, offer Offer) Kit {
	kit := Generate(choicesFrom(q), offer)
	if h.validate != nil {
		kit.Problems = append(kit.Problems, h.validate([]byte(kit.Config), offer)...)
	}
	return kit
}

// offer is what the running worker offers, or Previa's defaults when none
// that builds is running.
func (h *Handler) offer(ctx context.Context) Offer {
	if h.caps == nil {
		return h.fallback
	}
	caps, err := h.caps.Current(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "read worker capabilities for starter kits", "error", err)
		return h.fallback
	}
	if !caps.Execution && !caps.Paused {
		return h.fallback
	}
	o := Offer{Known: true}
	for _, entry := range caps.Services { // "postgres 17, 16"
		kind, versions, _ := strings.Cut(entry, " ")
		list := strings.Split(versions, ", ")
		switch kind {
		case "postgres":
			o.Postgres = list
		case "redis":
			o.Redis = list
		}
	}
	return o
}

// choicesFrom reads the form. A request without a stack is a fresh kit;
// switching stack keeps the services and switches, and takes the new
// stack's own defaults for everything else.
func choicesFrom(q url.Values) Choices {
	id := q.Get("stack")
	if id == "" {
		return Fresh(catalog[0].ID)
	}
	on := func(key string) bool { return q.Get(key) == "on" }
	version := func(kind string) string {
		if on(kind) {
			return cmpOr(q.Get(kind+"_version"), "latest")
		}
		return ""
	}
	if from := q.Get("from"); from != "" && from != id {
		c := Fresh(id)
		c.Postgres, c.Redis = version("postgres"), version("redis")
		c.Setup = on("setup") && c.SetupWith != ""
		c.Worker = on("worker") && len(Find(id).Workers) > 0
		c.Tier = q.Get("tier")
		return c
	}
	port := 0
	if text := strings.TrimSpace(q.Get("port")); text != "" {
		port = -1 // reported unless it is a number in range
		if n, err := strconv.Atoi(text); err == nil {
			port = n
		}
	}
	return Choices{
		Stack: id, Postgres: version("postgres"), Redis: version("redis"),
		Setup: on("setup"), SetupWith: q.Get("setup_with"), SetupCommand: q.Get("setup_cmd"),
		Worker: on("worker"), WorkerWith: q.Get("worker_with"), WorkerCommand: q.Get("worker_cmd"),
		Docs: on("docs"), DocsPath: q.Get("docs_path"),
		Port: port, Health: q.Get("health"), Tier: q.Get("tier"), Detail: q.Get("detail"),
	}
}
