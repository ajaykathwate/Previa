// Package previewaccess lets an owner make a repository's previews private:
// the router asks this server before every request to one (forward
// authentication), and only people who sign in with GitHub and can see the
// repository get in. Their access is a cookie on that preview's host only,
// signed by the server, for 12 hours.
package previewaccess

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/previewaccess/dbgen"
	"github.com/ajaykathwatee/previa/internal/routing"
	accesspages "github.com/ajaykathwatee/previa/web/pages/previewaccess"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// cookieName is host-only (the __Host- prefix makes browsers refuse it
	// with a Domain, without Secure or outside /), so one preview's access
	// never reaches another.
	cookieName = "__Host-previa-access"
	// returnPath on a preview host turns a token into the cookie.
	returnPath     = "/__previa/access"
	cookieLifetime = 12 * time.Hour
	tokenLifetime  = time.Minute
	accessPage     = "/previews/access"
)

// readable are the GitHub roles that can see a repository.
var readable = map[string]bool{"admin": true, "maintain": true, "write": true, "triage": true, "read": true}

var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// UserSession is the signed-in user; the auth feature implements it.
type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
}

// Permissions reads a user's role on a repository as the GitHub App sees it
// ("read", "write", ... or "none").
type Permissions interface {
	RepositoryPermission(ctx context.Context, installationID int64, fullName, login string) (string, error)
}

// Enqueuer asks a worker to rewrite the router's file.
type Enqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

type Handler struct {
	pool        *pgxpool.Pool
	users       UserSession
	permissions Permissions // nil without the GitHub App: only owners get in
	jobs        Enqueuer
	signer      signer
	baseDomain  string
	appBaseURL  string
	logger      *slog.Logger
	now         func() time.Time
}

func NewHandler(pool *pgxpool.Pool, users UserSession, permissions Permissions, jobs Enqueuer, sessionSecret, baseDomain, appBaseURL string, logger *slog.Logger) *Handler {
	return &Handler{pool: pool, users: users, permissions: permissions, jobs: jobs, signer: newSigner(sessionSecret),
		baseDomain: baseDomain, appBaseURL: strings.TrimSuffix(appBaseURL, "/"), logger: logger, now: time.Now}
}

// MountPublic serves the router's forward-authentication check; it reads
// no session (the router calls it for every visitor).
func (h *Handler) MountPublic(r chi.Router) {
	r.HandleFunc(routing.AccessPath, h.check)
}

// Mount serves the sign-in step for visitors and the owner's setting.
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Get(accessPage, h.access)
	r.With(requireUser).Post("/repositories/{repoID}/preview-access", h.set)
}

// previewHost returns the preview's DNS label when host is one of ours.
func (h *Handler) previewHost(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	label, ok := strings.CutSuffix(host, "."+h.baseDomain)
	return label, h.baseDomain != "" && ok && labelPattern.MatchString(label)
}

// check answers the router: 200 lets the request through; a redirect
// (to sign in, or with the cookie) goes back to the visitor.
func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	host := strings.ToLower(r.Header.Get("X-Forwarded-Host"))
	if _, ok := h.previewHost(host); !ok {
		http.Error(w, "Not a preview.", http.StatusForbidden)
		return
	}
	forwarded, err := url.ParseRequestURI(r.Header.Get("X-Forwarded-Uri"))
	if err != nil {
		forwarded = &url.URL{Path: "/"}
	}
	now := h.now()
	if forwarded.Path == returnPath {
		token, ok := h.signer.verify("token", forwarded.Query().Get("t"), host, now)
		if !ok {
			http.Error(w, "This access link expired. Open the preview again.", http.StatusForbidden)
			return
		}
		expires := now.Add(cookieLifetime)
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: h.signer.sign("cookie", claims{Host: host, User: token.User, Expires: expires.Unix()}),
			Path: "/", Expires: expires, MaxAge: int(cookieLifetime.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, token.Path, http.StatusFound)
		return
	}
	if cookie, err := r.Cookie(cookieName); err == nil {
		if _, ok := h.signer.verify("cookie", cookie.Value, host, now); ok {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	next := url.Values{"host": {host}, "path": {forwarded.RequestURI()}}
	http.Redirect(w, r, h.appBaseURL+accessPage+"?"+next.Encode(), http.StatusFound)
}

// access signs the visitor in, checks they can see the repository and sends
// them back to the preview with a one-minute token.
func (h *Handler) access(w http.ResponseWriter, r *http.Request) {
	user, _, signedIn := h.users.UserIDs(r.Context())
	if !signedIn {
		http.Redirect(w, r, "/auth/github/login?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(), http.StatusSeeOther)
		return
	}
	host := strings.ToLower(r.URL.Query().Get("host"))
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || path == returnPath {
		path = "/"
	}
	label, ok := h.previewHost(host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	preview, err := dbgen.New(h.pool).PreviewForHost(r.Context(), label)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	target := "https://" + host + path
	if !preview.PrivatePreviews {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	allowed, err := h.allowed(r.Context(), user, preview)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !allowed {
		username, _ := h.users.Username(r.Context())
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		_ = accesspages.Denied(username).Render(r.Context(), w)
		return
	}
	token := h.signer.sign("token", claims{Host: host, User: user, Path: path, Expires: h.now().Add(tokenLifetime).Unix()})
	http.Redirect(w, r, "https://"+host+returnPath+"?"+url.Values{"t": {token}}.Encode(), http.StatusSeeOther)
}

// allowed: the repository's Previa owner, or a GitHub user who can see it.
func (h *Handler) allowed(ctx context.Context, user int64, p dbgen.PreviewForHostRow) (bool, error) {
	if p.OwnerUserID == user {
		return true, nil
	}
	if h.permissions == nil {
		return false, nil
	}
	login, ok := h.users.Username(ctx)
	if !ok || login == "" {
		return false, nil
	}
	permission, err := h.permissions.RepositoryPermission(ctx, p.GithubInstallationID, p.FullName, login)
	if err != nil {
		return false, fmt.Errorf("read repository permission: %w", err)
	}
	return readable[permission], nil
}

// set makes the owner's repository's previews private or public, and asks
// a worker to route them that way.
func (h *Handler) set(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.users.UserIDs(r.Context())
	repoID, err := strconv.ParseInt(chi.URLParam(r, "repoID"), 10, 64)
	if !ok || err != nil || repoID <= 0 {
		http.NotFound(w, r)
		return
	}
	private := r.PostFormValue("private") == "true"
	changed := false
	err = database.InTransaction(r.Context(), h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		owns, err := q.OwnsRepository(r.Context(), dbgen.OwnsRepositoryParams{RepoID: repoID, OwnerUserID: user})
		if err != nil || !owns {
			return errors.Join(err, errNotOwned)
		}
		n, err := q.SetPrivatePreviews(r.Context(), dbgen.SetPrivatePreviewsParams{RepoID: repoID, OwnerUserID: user, Private: private})
		if err != nil || n == 0 {
			return err
		}
		changed = true
		_, err = q.AdvanceRouteTable(r.Context())
		return err
	})
	if errors.Is(err, errNotOwned) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if changed {
		args := routing.SyncArgs{}
		opts := args.InsertOpts()
		if _, err := h.jobs.Insert(r.Context(), args, &opts); err != nil {
			h.fail(w, r, err)
			return
		}
		h.logger.InfoContext(r.Context(), "preview access changed", "repo_id", repoID, "private", private)
	}
	http.Redirect(w, r, "/repositories/"+strconv.FormatInt(repoID, 10), http.StatusSeeOther)
}

var errNotOwned = errors.New("repository not owned")

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "preview access request failed", "error", err)
	http.Error(w, "Preview access is temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
}
