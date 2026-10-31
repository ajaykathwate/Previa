package auth

import (
	"context"
	"crypto/hmac"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/http"

	"github.com/a-h/templ"
	pages "github.com/ajaykathwatee/previa/web/pages/auth"
	"golang.org/x/oauth2"
)

type linkedUsers interface {
	SaveGoogle(context.Context, Identity, int64) (User, error)
	LinkGitHub(context.Context, Identity, int64) (User, error)
}

func (h *Handler) WithGoogle(provider googleProvider) *Handler { h.google = provider; return h }
func (h *Handler) loginOptions(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !LocalPath(next) {
		next = "/dashboard"
	}
	templ.Handler(pages.LoginOptions(next, h.google != nil)).ServeHTTP(w, r)
}
func (h *Handler) startLink(w http.ResponseWriter, r *http.Request) {
	u, ok := CurrentUser(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	provider := "github"
	if r.URL.Path == "/auth/google/link" {
		provider = "google"
	}
	if provider == "google" && h.google == nil {
		http.NotFound(w, r)
		return
	}
	h.beginOAuth(w, r, provider, u.ID)
}
func (h *Handler) googleLogin(w http.ResponseWriter, r *http.Request) {
	if h.google == nil {
		http.NotFound(w, r)
		return
	}
	h.beginOAuth(w, r, "google", 0)
}
func (h *Handler) beginOAuth(w http.ResponseWriter, r *http.Request, provider string, link int64) {
	state := randomToken()
	h.sessions.Put(r.Context(), "oauth_state", state)
	h.sessions.Put(r.Context(), "oauth_provider", provider)
	h.sessions.Remove(r.Context(), "oauth_link_user")
	h.sessions.Remove(r.Context(), "after_login")
	if link > 0 {
		h.sessions.Put(r.Context(), "oauth_link_user", link)
		h.sessions.Put(r.Context(), "after_login", "/protected")
	} else if next := r.URL.Query().Get("next"); LocalPath(next) {
		h.sessions.Put(r.Context(), "after_login", next)
	}
	http.SetCookie(w, h.stateCookie(h.signState(state), int(stateLifetime.Seconds())))
	var target string
	if provider == "google" {
		// Google always shows its account chooser.
		nonce, verifier := randomToken(), oauth2.GenerateVerifier()
		h.sessions.Put(r.Context(), "google_nonce", nonce)
		h.sessions.Put(r.Context(), "google_verifier", verifier)
		target = h.google.AuthCodeURL(state, nonce, verifier)
	} else {
		target = h.provider.AuthCodeURL(state)
		// After signing out, or when connecting GitHub to an account, let the
		// person choose: GitHub would otherwise reuse the account the browser
		// is signed in to without asking.
		if link > 0 || h.signedOut(r) {
			target = withAccountPicker(target)
		}
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
func (h *Handler) consumeState(w http.ResponseWriter, r *http.Request, provider string) bool {
	http.SetCookie(w, h.stateCookie("", -1))
	state, expected := r.URL.Query().Get("state"), h.sessions.PopString(r.Context(), "oauth_state")
	actual := h.sessions.PopString(r.Context(), "oauth_provider")
	cookie, err := r.Cookie(h.stateCookieName())
	return err == nil && actual == provider && expected != "" && hmac.Equal([]byte(expected), []byte(state)) && h.validState(cookie.Value, state)
}
func (h *Handler) saveOAuthIdentity(w http.ResponseWriter, r *http.Request, provider string, i Identity) {
	link := h.sessions.GetInt64(r.Context(), "oauth_link_user")
	h.sessions.Remove(r.Context(), "oauth_link_user")
	var u User
	var err error
	if link > 0 {
		current, ok := CurrentUser(r.Context())
		if !ok || current.ID != link {
			h.fail(w, r, http.StatusForbidden, "Sign in again before connecting a login method.")
			return
		}
	}
	if provider == "google" || link > 0 {
		store, ok := h.users.(linkedUsers)
		if !ok {
			h.fail(w, r, http.StatusServiceUnavailable, "Account linking is unavailable.")
			return
		}
		if provider == "google" {
			u, err = store.SaveGoogle(r.Context(), i, link)
		} else {
			u, err = store.LinkGitHub(r.Context(), i, link)
		}
	} else {
		u, err = h.users.UpsertUser(r.Context(), i)
	}
	if err != nil {
		h.logger.WarnContext(r.Context(), "provider identity could not be saved", "provider", provider)
		var constraint *pgconn.PgError
		if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &constraint) && constraint.Code == "23505") {
			h.fail(w, r, http.StatusConflict, "This identity could not be connected. It may already belong to another account. Sign in to that account and connect your other login method from your profile.")
		} else {
			h.fail(w, r, http.StatusServiceUnavailable, "Your sign-in could not be saved. Please try again.")
		}
		return
	}
	h.finishSignIn(w, r, u)
}
func (h *Handler) finishSignIn(w http.ResponseWriter, r *http.Request, u User) {
	if err := h.sessions.RenewToken(r.Context()); err != nil {
		h.fail(w, r, http.StatusServiceUnavailable, "Your session could not be started. Please try again.")
		return
	}
	h.sessions.Put(r.Context(), "user_id", u.ID)
	h.sessions.Put(r.Context(), "session_epoch", u.SessionEpoch)
	h.sessions.Put(r.Context(), "csrf_token", randomToken())
	if h.signedOut(r) {
		http.SetCookie(w, h.signedOutCookie(-1))
	}
	next := h.sessions.PopString(r.Context(), "after_login")
	if !LocalPath(next) {
		next = "/dashboard"
		if u.GitHubID == 0 {
			next = "/workspaces"
		}
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
func (h *Handler) googleCallback(w http.ResponseWriter, r *http.Request) {
	if h.google == nil {
		http.NotFound(w, r)
		return
	}
	nonce, verifier := h.sessions.PopString(r.Context(), "google_nonce"), h.sessions.PopString(r.Context(), "google_verifier")
	if !h.consumeState(w, r, "google") {
		h.fail(w, r, http.StatusBadRequest, "Sign-in could not be verified. Please start again.")
		return
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		h.fail(w, r, http.StatusBadRequest, "Google sign-in was interrupted. Please try again.")
		return
	}
	i, err := h.google.Authenticate(r.Context(), r.URL.Query().Get("code"), nonce, verifier)
	if err != nil {
		h.logger.WarnContext(r.Context(), "Google identity authentication failed")
		h.fail(w, r, http.StatusBadGateway, "Google sign-in is unavailable. Please try again.")
		return
	}
	h.saveOAuthIdentity(w, r, "google", i)
}

// SignInPath lets private-preview authentication offer the configured methods.
func (h *Handler) SignInPath() string {
	if h.google != nil {
		return "/auth/login"
	}
	return "/auth/github/login"
}
