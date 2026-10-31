package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	stateCookieName = "previa_oauth_state"
	// Over HTTPS the state cookie is __Host- prefixed (which requires path /),
	// so a preview on a sibling subdomain cannot plant OAuth state.
	secureStateCookieName = "__Host-previa_oauth_state"
	// signedOutCookieName marks a browser whose person signed out, so the next
	// GitHub sign-in shows GitHub's account picker instead of silently reusing
	// the GitHub account the browser is still signed in to.
	signedOutCookieName       = "previa_signed_out"
	secureSignedOutCookieName = "__Host-previa_signed_out"
)
const (
	stateLifetime     = 10 * time.Minute
	signedOutLifetime = 30 * 24 * time.Hour
)

func randomToken() string { return rand.Text() + rand.Text() }

func (h *Handler) signState(state string) string {
	payload := state + "." + strconv.FormatInt(time.Now().Add(stateLifetime).Unix(), 10)
	mac := hmac.New(sha256.New, h.stateKey)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *Handler) validState(cookie, state string) bool {
	parts := strings.Split(cookie, ".")
	if len(parts) != 3 || state == "" || !hmac.Equal([]byte(parts[0]), []byte(state)) {
		return false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || expires <= time.Now().Unix() || expires > time.Now().Add(stateLifetime).Unix() {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, h.stateKey)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	return hmac.Equal(sig, mac.Sum(nil))
}

func (h *Handler) stateCookieName() string {
	if h.secure {
		return secureStateCookieName
	}
	return stateCookieName
}

func (h *Handler) stateCookie(value string, maxAge int) *http.Cookie {
	return h.authCookie(h.stateCookieName(), value, maxAge)
}

func (h *Handler) signedOutCookieName() string {
	if h.secure {
		return secureSignedOutCookieName
	}
	return signedOutCookieName
}

// signedOutCookie is set by sign-out and cleared by the next completed sign-in.
func (h *Handler) signedOutCookie(maxAge int) *http.Cookie {
	value := "1"
	if maxAge < 0 {
		value = ""
	}
	return h.authCookie(h.signedOutCookieName(), value, maxAge)
}

func (h *Handler) signedOut(r *http.Request) bool {
	_, err := r.Cookie(h.signedOutCookieName())
	return err == nil
}

func (h *Handler) authCookie(name, value string, maxAge int) *http.Cookie {
	path := "/auth"
	if h.secure {
		path = "/"
	}
	return &http.Cookie{
		Name: name, Value: value, Path: path,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
		MaxAge: maxAge,
	}
}

// withAccountPicker asks GitHub to show its account picker, where the person
// can continue as the signed-in account or use a different one.
func withAccountPicker(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	q := u.Query()
	q.Set("prompt", "select_account")
	u.RawQuery = q.Encode()
	return u.String()
}
