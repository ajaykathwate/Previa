package workspaces

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/a-h/templ"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
)

func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	err := h.service.UpdateProfile(r.Context(), u, id, parseID(r.PostFormValue("profile_revision")), r.PostFormValue("name"), r.PostFormValue("description"))
	if err != nil {
		h.profileFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/settings?saved=details", http.StatusSeeOther)
}

func (h *Handler) uploadLogo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(256 << 10); err != nil {
		h.profileFailure(w, r, ErrLogo)
		return
	}
	defer func() {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			h.logger.WarnContext(r.Context(), "remove workspace upload files", "error", err)
		}
	}()
	file, _, err := r.FormFile("logo")
	if err != nil {
		h.profileFailure(w, r, ErrLogo)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			h.logger.WarnContext(r.Context(), "close workspace logo upload", "error", err)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, MaxLogoBytes+1))
	if err != nil {
		h.profileFailure(w, r, fmt.Errorf("read workspace logo: %w", err))
		return
	}
	u, id := h.identity(r)
	if err := h.service.SetLogo(r.Context(), u, id, parseID(r.PostFormValue("profile_revision")), data); err != nil {
		h.profileFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/settings?saved=logo", http.StatusSeeOther)
}

func (h *Handler) removeLogo(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	if err := h.service.RemoveLogo(r.Context(), u, id, parseID(r.PostFormValue("profile_revision"))); err != nil {
		h.profileFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/settings?saved=logo-removed", http.StatusSeeOther)
}

func (h *Handler) logo(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	data, version, err := h.service.Logo(r.Context(), u, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", fmt.Sprintf(`"workspace-%d-logo-%d"`, id, version))
	if _, err := w.Write(data); err != nil {
		h.logger.WarnContext(r.Context(), "write workspace logo", "error", err)
	}
}

func (h *Handler) profileFailure(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, ErrInput) && !errors.Is(err, ErrLogo) && !errors.Is(err, ErrConflict) {
		h.fail(w, r, err)
		return
	}
	u, id := h.identity(r)
	p, loadErr := h.service.Page(r.Context(), u, id, "settings", PageQuery{Page: 1})
	if loadErr != nil {
		h.fail(w, r, loadErr)
		return
	}
	status := http.StatusBadRequest
	p.Error = "Use a workspace name of 1–80 characters and a description of up to 240 characters."
	if errors.Is(err, ErrLogo) {
		p.Error = ErrLogo.Error()
	}
	if errors.Is(err, ErrConflict) {
		status = http.StatusConflict
		p.Error = "Workspace details changed in another window. Review the current details before saving again."
	}
	if r.PostForm.Has("name") && !errors.Is(err, ErrConflict) {
		p.FormName = r.PostForm.Get("name")
		p.FormDescription = r.PostForm.Get("description")
	}
	h.renderWorkspace(w, r, p, status)
}

func (h *Handler) renderWorkspace(w http.ResponseWriter, r *http.Request, p pages.Page, status int) {
	p.CSRF = h.users.CSRFToken(r.Context())
	login, _ := h.users.Username(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	templ.Handler(pages.Index(login, p), templ.WithStatus(status)).ServeHTTP(w, r)
}

func (h *Handler) legacyPreviews(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	if _, err := h.service.Read(r.Context(), u, id); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/previews", http.StatusSeeOther)
}
