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

	"github.com/ajaykathwatee/previa/internal/platform/publicid"

	"github.com/ajaykathwatee/previa/internal/platform/database"
	"github.com/ajaykathwatee/previa/internal/platform/workers"
	"github.com/ajaykathwatee/previa/internal/previewaccess/dbgen"
	"github.com/ajaykathwatee/previa/internal/routing"
	accesspages "github.com/ajaykathwatee/previa/web/pages/previewaccess"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

// SessionAuthority binds preview grants to the dashboard session that issued
// them. Only a one-way store-token digest crosses the preview boundary.
type SessionAuthority interface {
	PreviewSessionID(context.Context) string
	PreviewSessionValid(context.Context, string) (bool, error)
}

// Permissions reads a user's role on a repository as the GitHub App sees it
// ("read", "write", ... or "none").
type Permissions interface {
	RepositoryUserPermission(ctx context.Context, installationID int64, fullName, login string, userID int64) (string, error)
}

// Enqueuer asks a worker to rewrite the router's file.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Capabilities reports whether the running routing workers have auth configured.
type Capabilities interface {
	Current(context.Context) (workers.Capabilities, error)
}

type Handler struct {
	pool         *pgxpool.Pool
	users        UserSession
	sessions     SessionAuthority
	permissions  Permissions // nil without the GitHub App: only owners get in
	jobs         Enqueuer
	capabilities Capabilities
	signer       signer
	baseDomain   string
	appBaseURL   string
	logger       *slog.Logger
	now          func() time.Time
}

func NewHandler(pool *pgxpool.Pool, users UserSession, permissions Permissions, jobs Enqueuer, sessionSecret, baseDomain, appBaseURL string, logger *slog.Logger) *Handler {
	sessions, _ := users.(SessionAuthority)
	return &Handler{pool: pool, users: users, sessions: sessions, permissions: permissions, jobs: jobs, signer: newSigner(sessionSecret),
		baseDomain: baseDomain, appBaseURL: strings.TrimSuffix(appBaseURL, "/"), logger: logger, now: time.Now}
}

func (h *Handler) WithCapabilities(c Capabilities) *Handler {
	h.capabilities = c
	return h
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
		if !ok || !h.validGrant(r.Context(), token) {
			http.Error(w, "This access link expired. Open the preview again.", http.StatusForbidden)
			return
		}
		expires := now.Add(cookieLifetime)
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: h.signer.sign("cookie", claims{Host: host, User: token.User, Epoch: token.Epoch, Session: token.Session, Revision: token.Revision, Local: token.Local, Expires: expires.Unix()}),
			Path: "/", Expires: expires, MaxAge: int(cookieLifetime.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		publicid.Redirect(w, r, token.Path, http.StatusFound)
		return
	}
	if cookie, err := r.Cookie(cookieName); err == nil {
		if grant, ok := h.signer.verify("cookie", cookie.Value, host, now); ok && h.validGrant(r.Context(), grant) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	next := url.Values{"host": {host}, "path": {forwarded.RequestURI()}}
	publicid.Redirect(w, r, h.appBaseURL+accessPage+"?"+next.Encode(), http.StatusFound)
}

// access signs the visitor in, checks they can see the repository and sends
// them back to the preview with a one-minute token.
func (h *Handler) access(w http.ResponseWriter, r *http.Request) {
	user, _, signedIn := h.users.UserIDs(r.Context())
	if !signedIn {
		publicid.Redirect(w, r, "/auth/github/login?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(), http.StatusSeeOther)
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
		publicid.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	allowed, local, err := h.allowed(r.Context(), user, label, preview)
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
	var epoch int64
	if err := h.pool.QueryRow(r.Context(), `SELECT session_epoch FROM users WHERE id=$1 AND deleted_at IS NULL`, user).Scan(&epoch); err != nil {
		h.fail(w, r, fmt.Errorf("read preview session authority: %w", err))
		return
	}
	session := ""
	if h.sessions != nil {
		session = h.sessions.PreviewSessionID(r.Context())
	}
	token := h.signer.sign("token", claims{Host: host, User: user, Epoch: epoch, Session: session, Revision: preview.AuthorizationRevision, Local: local, Path: path, Expires: h.now().Add(tokenLifetime).Unix()})
	publicid.Redirect(w, r, "https://"+host+returnPath+"?"+url.Values{"t": {token}}.Encode(), http.StatusSeeOther)
}

// validGrant checks current local authority for every protected preview request.
// A removed member cannot fall back to GitHub outsider access.
func (h *Handler) validGrant(ctx context.Context, c claims) bool {
	if h.sessions != nil {
		valid, err := h.sessions.PreviewSessionValid(ctx, c.Session)
		if err != nil {
			h.logger.ErrorContext(ctx, "check preview session", "error", err)
			return false
		}
		if !valid {
			return false
		}
	}
	var epoch int64
	if err := h.pool.QueryRow(ctx, `SELECT session_epoch FROM users WHERE id=$1 AND deleted_at IS NULL`, c.User).Scan(&epoch); err != nil || epoch != c.Epoch {
		return false
	}
	label, ok := h.previewHost(c.Host)
	if !ok || c.Revision <= 0 {
		return false
	}
	v, err := dbgen.New(h.pool).PreviewAuthorization(ctx, dbgen.PreviewAuthorizationParams{Subdomain: label, UserID: c.User})
	if err != nil || v.AuthorizationRevision != c.Revision {
		return false
	}
	if c.Local {
		return v.Allowed
	}
	return !v.MembershipExists
}

func (h *Handler) allowed(ctx context.Context, user int64, label string, p dbgen.PreviewForHostRow) (bool, bool, error) {
	v, err := dbgen.New(h.pool).PreviewAuthorization(ctx, dbgen.PreviewAuthorizationParams{Subdomain: label, UserID: user})
	if err != nil {
		return false, false, fmt.Errorf("read preview grant: %w", err)
	}
	if v.MembershipExists {
		return v.Allowed, true, nil
	}
	if h.permissions == nil {
		return false, false, nil
	}
	login, ok := h.users.Username(ctx)
	if provider, exists := h.users.(interface {
		GitHubUsername(context.Context) (string, bool)
	}); exists {
		login, ok = provider.GitHubUsername(ctx)
	}
	_, githubID, signedIn := h.users.UserIDs(ctx)
	if !ok || !signedIn || login == "" {
		return false, false, nil
	}
	permission, err := h.permissions.RepositoryUserPermission(ctx, p.GithubInstallationID, p.FullName, login, githubID)
	if err != nil {
		return false, false, fmt.Errorf("read repository permission: %w", err)
	}
	return readable[permission], false, nil
}

// set makes the owner's repository's previews private or public, and asks
// a worker to route them that way.
func (h *Handler) set(w http.ResponseWriter, r *http.Request) {
	user, _, ok := h.users.UserIDs(r.Context())
	repoID, err := publicid.Param(r, "repoID", "repository")
	if !ok || err != nil || repoID <= 0 {
		http.NotFound(w, r)
		return
	}
	private := r.PostFormValue("private") == "true"
	// Read worker metadata before acquiring a transaction connection. Ownership
	// is still checked first inside the transaction, including on guard failures.
	var capabilityErr error
	if private {
		capabilityErr = h.canMakePrivate(r.Context())
	}
	changed := false
	err = database.InTransaction(r.Context(), h.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockPreviewInstallation(r.Context(), repoID); errors.Is(err, pgx.ErrNoRows) {
			return errNotOwned
		} else if err != nil {
			return fmt.Errorf("lock preview installation: %w", err)
		}
		owns, err := q.OwnsRepository(r.Context(), dbgen.OwnsRepositoryParams{RepoID: repoID, OwnerUserID: pgtype.Int8{Int64: user, Valid: user > 0}})
		if err != nil || !owns {
			return errors.Join(err, errNotOwned)
		}
		if capabilityErr != nil {
			return capabilityErr
		}
		n, err := q.SetPrivatePreviews(r.Context(), dbgen.SetPrivatePreviewsParams{RepoID: repoID, OwnerUserID: pgtype.Int8{Int64: user, Valid: user > 0}, Private: private})
		if err != nil || n == 0 {
			return err
		}
		changed = true
		version, err := q.AdvanceRouteTable(r.Context())
		if err != nil {
			return fmt.Errorf("advance preview route table: %w", err)
		}
		args := routing.SyncArgs{Version: version}
		opts := args.InsertOpts()
		if _, err := h.jobs.InsertTx(r.Context(), tx, args, &opts); err != nil {
			return fmt.Errorf("queue preview access routes: %w", err)
		}
		return nil
	})
	if errors.Is(err, errNotOwned) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, errPrivateUnavailable) {
		publicid.Redirect(w, r, "/repositories/"+strconv.FormatInt(repoID, 10)+"?preview_access=unavailable", http.StatusSeeOther)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if changed {
		h.logger.InfoContext(r.Context(), "preview access changed", "repo_id", repoID, "private", private)
	}
	publicid.Redirect(w, r, "/repositories/"+strconv.FormatInt(repoID, 10), http.StatusSeeOther)
}

var errNotOwned = errors.New("repository not owned")
var errPrivateUnavailable = errors.New("private preview authentication is not configured")

func (h *Handler) canMakePrivate(ctx context.Context) error {
	if h.capabilities == nil {
		return errPrivateUnavailable
	}
	caps, err := h.capabilities.Current(ctx)
	if err != nil {
		return fmt.Errorf("read private preview capability: %w", err)
	}
	if !caps.PrivatePreviews {
		return errPrivateUnavailable
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "preview access request failed", "error", err)
	http.Error(w, "Preview access is temporarily unavailable. Please retry.", http.StatusServiceUnavailable)
}
