package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/web/components"
	pages "github.com/ajaykathwatee/previa/web/pages/auth"
	"github.com/jackc/pgx/v5"
)

type personalProfileWriter interface {
	UpdatePersonalName(context.Context, int64, int64, string, string) error
}

type avatarStore interface {
	SaveAvatar(context.Context, int64, []byte) (int64, error)
	RemoveAvatar(context.Context, int64) error
	Avatar(context.Context, int64) ([]byte, int64, error)
}

// AvatarAudience decides who besides the member may see their uploaded picture.
type AvatarAudience interface {
	SharesWorkspace(ctx context.Context, viewer, subject int64) (bool, error)
}

// ActivityFeed supplies the signed-in person's recent actions for their profile.
type ActivityFeed interface {
	RecentActivity(ctx context.Context, user int64) ([]components.ActivityItem, error)
}

// WithActivity shows recent activity on the profile page.
func (h *Handler) WithActivity(a ActivityFeed) *Handler {
	h.activity = a
	return h
}

// WithAvatarAudience lets current teammates see uploaded profile pictures.
func (h *Handler) WithAvatarAudience(a AvatarAudience) *Handler {
	h.audience = a
	return h
}

var profileSaved = map[string]string{"name": "Name saved", "photo": "Profile photo updated", "photo-removed": "Profile photo removed", "sessions": "Other sessions ended"}

// ServeProfile is consumed by the dashboard's session route; auth owns account data.
func (h *Handler) ServeProfile(w http.ResponseWriter, r *http.Request) {
	h.renderProfile(w, r, http.StatusOK, pages.Profile{Toast: profileSaved[r.URL.Query().Get("saved")]}, false)
}
func profileTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
func (h *Handler) renderProfile(w http.ResponseWriter, r *http.Request, status int, p pages.Profile, keepInput bool) {
	u, ok := CurrentUser(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.GoogleEmail, p.GoogleConnected, p.GoogleEnabled = u.GoogleEmail, u.GoogleSubject != "", h.google != nil
	p.Name, p.Login, p.GitHubID, p.Avatar, p.CustomAvatar = u.DisplayName(), u.Username, u.GitHubID, u.AvatarSrc(), u.CustomAvatar
	p.PublicID, p.FirstName, p.LastName, p.GitHubName, p.Email, p.EmailSyncError = u.PublicID, u.FirstName, u.LastName, u.GitHubName, u.Email, u.EmailSyncError
	p.ProfileURL, p.Bio, p.Company, p.Location = u.ProfileURL, u.Bio, u.Company, u.Location
	p.Joined, p.GitHubJoined, p.Synced, p.Expires = profileTime(u.JoinedAt), profileTime(u.GitHubCreatedAt), profileTime(u.ProfileSyncedAt), profileTime(h.sessions.Deadline(r.Context()))
	p.CSRF, p.Revision = h.CSRFToken(r.Context()), u.ProfileRevision
	// Blank names show GitHub's split name as a hint of what a save would keep.
	if parts := strings.Fields(u.GitHubName); len(parts) > 0 {
		p.FirstHint, p.LastHint = parts[0], strings.Join(parts[1:], " ")
	}
	if keepInput {
		p.FirstName, p.LastName = r.PostFormValue("first_name"), r.PostFormValue("last_name")
	}
	if h.activity != nil {
		var err error
		if p.Activity, err = h.activity.RecentActivity(r.Context(), u.ID); err != nil {
			h.logger.ErrorContext(r.Context(), "load profile activity", "error", err)
			p.Activity, p.ActivityUnavailable = nil, true
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.MemberProfile(p), p, templ.WithStatus(status)).ServeHTTP(w, r)
}

// profileError answers an in-place save with JSON, or re-renders the page
// with the message next to the control that failed.
func (h *Handler) profileError(w http.ResponseWriter, r *http.Request, status int, p pages.Profile, keepInput bool) {
	if httpx.WantsJSON(r) {
		httpx.WriteJSON(w, status, httpx.FormResult{Error: p.NameError + p.AvatarError})
		return
	}
	h.renderProfile(w, r, status, p, keepInput)
}

func (h *Handler) updatePersonalName(w http.ResponseWriter, r *http.Request) {
	u, ok := CurrentUser(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	store, ok := h.users.(personalProfileWriter)
	if !ok {
		h.fail(w, r, http.StatusServiceUnavailable, "Your profile cannot be saved right now.")
		return
	}
	revision, _ := strconv.ParseInt(r.PostFormValue("profile_revision"), 10, 64)
	first, last := strings.TrimSpace(r.PostFormValue("first_name")), strings.TrimSpace(r.PostFormValue("last_name"))
	err := store.UpdatePersonalName(r.Context(), u.ID, revision, first, last)
	if err != nil {
		switch {
		case errors.Is(err, ErrName):
			h.profileError(w, r, http.StatusBadRequest, pages.Profile{NameError: "Use up to 80 characters for each name, without control characters."}, true)
		case errors.Is(err, ErrProfileConflict):
			h.profileError(w, r, http.StatusConflict, pages.Profile{NameError: "Your name changed in another window. Review it, then save again."}, false)
		default:
			h.logger.ErrorContext(r.Context(), "save member profile", "error", fmt.Errorf("update profile: %w", err))
			h.profileError(w, r, http.StatusServiceUnavailable, pages.Profile{NameError: "Your name could not be saved. Please try again."}, true)
		}
		return
	}
	if httpx.WantsJSON(r) {
		saved := User{FirstName: first, LastName: last, Username: u.Username}
		httpx.WriteJSON(w, http.StatusOK, httpx.FormResult{Message: profileSaved["name"], Revision: revision + 1,
			Fields: map[string]string{"first_name": first, "last_name": last, "display_name": saved.DisplayName()}})
		return
	}
	publicid.Redirect(w, r, "/protected?saved=name", http.StatusSeeOther)
}

func (h *Handler) uploadAvatar(w http.ResponseWriter, r *http.Request) {
	u, ok := CurrentUser(r.Context())
	store, stored := h.users.(avatarStore)
	if !ok || !stored {
		http.NotFound(w, r)
		return
	}
	data, err := avatarUpload(r)
	if err != nil {
		h.logger.WarnContext(r.Context(), "read profile picture upload", "error", err)
		h.profileError(w, r, http.StatusBadRequest, pages.Profile{AvatarError: ErrAvatar.Error()}, false)
		return
	}
	version, err := store.SaveAvatar(r.Context(), u.ID, data)
	if err != nil {
		if errors.Is(err, ErrAvatar) {
			h.profileError(w, r, http.StatusBadRequest, pages.Profile{AvatarError: ErrAvatar.Error()}, false)
			return
		}
		h.logger.ErrorContext(r.Context(), "save profile picture", "error", err)
		h.profileError(w, r, http.StatusServiceUnavailable, pages.Profile{AvatarError: "Your photo could not be saved. Please try again."}, false)
		return
	}
	if httpx.WantsJSON(r) {
		u.CustomAvatar, u.AvatarVersion = true, version
		httpx.WriteJSON(w, http.StatusOK, httpx.FormResult{Message: profileSaved["photo"], Fields: map[string]string{"avatar": u.AvatarSrc()}})
		return
	}
	publicid.Redirect(w, r, "/protected?saved=photo", http.StatusSeeOther)
}

func avatarUpload(r *http.Request) ([]byte, error) {
	if err := r.ParseMultipartForm(256 << 10); err != nil {
		return nil, fmt.Errorf("parse upload: %w", err)
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	file, _, err := r.FormFile("avatar")
	if err != nil {
		return nil, fmt.Errorf("open upload: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxAvatarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	return data, nil
}

func (h *Handler) removeAvatar(w http.ResponseWriter, r *http.Request) {
	u, ok := CurrentUser(r.Context())
	store, stored := h.users.(avatarStore)
	if !ok || !stored {
		http.NotFound(w, r)
		return
	}
	if err := store.RemoveAvatar(r.Context(), u.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "remove profile picture", "error", err)
		h.profileError(w, r, http.StatusServiceUnavailable, pages.Profile{AvatarError: "Your photo could not be removed. Please try again."}, false)
		return
	}
	if httpx.WantsJSON(r) {
		u.CustomAvatar = false
		httpx.WriteJSON(w, http.StatusOK, httpx.FormResult{Message: profileSaved["photo-removed"], Fields: map[string]string{"avatar": u.AvatarSrc()}})
		return
	}
	publicid.Redirect(w, r, "/protected?saved=photo-removed", http.StatusSeeOther)
}

// serveAvatar returns an uploaded picture to its member and their current
// teammates. Everyone else, and a removed picture, gets a plain 404.
func (h *Handler) serveAvatar(w http.ResponseWriter, r *http.Request) {
	viewer, ok := CurrentUser(r.Context())
	store, stored := h.users.(avatarStore)
	subject, err := publicid.Param(r, "userID", "user")
	if !ok || !stored || err != nil || subject <= 0 {
		http.NotFound(w, r)
		return
	}
	if subject != viewer.ID {
		shared := false
		if h.audience != nil {
			if shared, err = h.audience.SharesWorkspace(r.Context(), viewer.ID, subject); err != nil {
				h.logger.ErrorContext(r.Context(), "check profile picture audience", "error", err)
				http.Error(w, "Profile picture unavailable.", http.StatusServiceUnavailable)
				return
			}
		}
		if !shared {
			http.NotFound(w, r)
			return
		}
	}
	data, version, err := store.Avatar(r.Context(), subject)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "serve profile picture", "error", err)
		http.Error(w, "Profile picture unavailable.", http.StatusServiceUnavailable)
		return
	}
	etag := `"avatar-` + strconv.FormatInt(version, 10) + `"`
	w.Header().Set("Cache-Control", avatarCacheTime)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	if _, err := w.Write(data); err != nil {
		h.logger.WarnContext(r.Context(), "write profile picture", "error", err)
	}
}
