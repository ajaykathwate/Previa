package workspaces

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/platform/httpx"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"github.com/ajaykathwatee/previa/web/components"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
)

// profileSaved answers an in-place save with the new revision and saved values,
// or sends a plain HTML form back to settings with a confirmation.
func (h *Handler) profileSaved(w http.ResponseWriter, r *http.Request, id, revision int64, saved, message string, fields map[string]string) {
	if httpx.WantsJSON(r) {
		httpx.WriteJSON(w, http.StatusOK, httpx.FormResult{Message: message, Revision: revision, Fields: fields})
		return
	}
	publicid.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/settings?saved="+saved, http.StatusSeeOther)
}

func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	revision := parseID(r.PostFormValue("profile_revision"))
	err := h.service.UpdateProfile(r.Context(), u, id, revision, r.PostFormValue("name"), r.PostFormValue("description"))
	if err != nil {
		h.profileFailure(w, r, err)
		return
	}
	h.profileSaved(w, r, id, revision+1, "details", "Workspace details saved",
		map[string]string{"name": strings.TrimSpace(r.PostFormValue("name")), "description": strings.TrimSpace(r.PostFormValue("description"))})
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
	revision := parseID(r.PostFormValue("profile_revision"))
	if err := h.service.SetLogo(r.Context(), u, id, revision, data); err != nil {
		h.profileFailure(w, r, err)
		return
	}
	logo, err := publicid.URL(r, components.WorkspaceLogoURL(id, revision+1))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.profileSaved(w, r, id, revision+1, "logo", "Workspace logo updated", map[string]string{"logo": logo})
}

func (h *Handler) removeLogo(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	revision := parseID(r.PostFormValue("profile_revision"))
	if err := h.service.RemoveLogo(r.Context(), u, id, revision); err != nil {
		h.profileFailure(w, r, err)
		return
	}
	h.profileSaved(w, r, id, revision+1, "logo-removed", "Workspace logo removed", map[string]string{"logo": ""})
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
		if httpx.WantsJSON(r) && !errors.Is(err, ErrUnavailable) {
			h.logger.ErrorContext(r.Context(), "save workspace settings", "error", err)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.FormResult{Error: "Workspace settings could not be saved. Please try again."})
			return
		}
		h.fail(w, r, err)
		return
	}
	if httpx.WantsJSON(r) {
		status, message := http.StatusBadRequest, "Use a workspace name of 1–80 characters and a description of up to 240 characters."
		switch {
		case errors.Is(err, ErrLogo):
			message = "Choose a PNG or JPEG logo up to 192 KiB and 512 × 512 pixels."
		case errors.Is(err, ErrConflict):
			status, message = http.StatusConflict, "Workspace details changed in another window. Reload to see them, then try again."
		}
		httpx.WriteJSON(w, status, httpx.FormResult{Error: message})
		return
	}
	u, id := h.identity(r)
	p, loadErr := h.service.Page(r.Context(), u, id, "settings", PageQuery{Page: 1})
	if loadErr != nil {
		h.fail(w, r, loadErr)
		return
	}
	status := http.StatusBadRequest
	p.Error, p.ErrorField = "Use a workspace name of 1–80 characters and a description of up to 240 characters.", "details"
	if strings.Contains(r.URL.Path, "/logo") {
		p.ErrorField = "logo"
	}
	if errors.Is(err, ErrLogo) {
		p.Error = "Choose a PNG or JPEG logo up to 192 KiB and 512 × 512 pixels."
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
	publicid.Handler(r, pages.Index(login, p), p, templ.WithStatus(status)).ServeHTTP(w, r)
}

func (h *Handler) legacyPreviews(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	if _, err := h.service.Read(r.Context(), u, id); err != nil {
		h.fail(w, r, err)
		return
	}
	publicid.Redirect(w, r, "/previews", http.StatusSeeOther)
}
