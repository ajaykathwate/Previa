package workspaces

import (
	"errors"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
	"net/http"
)

func (h *Handler) invitationFailure(w http.ResponseWriter, r *http.Request, err error, repos []int64) {
	if errors.Is(err, ErrUnavailable) {
		h.fail(w, r, err)
		return
	}
	u, id := h.identity(r)
	tab := "members"
	if r.PostFormValue("return") == "setup" {
		tab = "setup"
	}
	p, loadErr := h.service.Page(r.Context(), u, id, tab, PageQuery{Page: 1})
	if loadErr != nil {
		h.fail(w, r, loadErr)
		return
	}
	form := pages.InviteForm{Failed: true, Login: r.PostFormValue("login"), Role: r.PostFormValue("role"), Repositories: repos}
	status := http.StatusUnprocessableEntity
	switch {
	case errors.Is(err, ErrEmailInvitation):
		form.LoginError = ErrEmailInvitation.Error()
	case errors.Is(err, ErrInvalidLogin):
		form.LoginError = ErrInvalidLogin.Error()
	case errors.Is(err, ErrGitHubAccountNotFound):
		form.LoginError = ErrGitHubAccountNotFound.Error()
	case errors.Is(err, ErrAlreadyMember):
		form.LoginError = ErrAlreadyMember.Error()
	case errors.Is(err, ErrPendingInvitation):
		form.LoginError = ErrPendingInvitation.Error()
	case errors.Is(err, ErrSourceUnavailable):
		status = http.StatusServiceUnavailable
		form.LoginError = "GitHub could not check this account right now. Your selections are saved here; please try again."
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
		form.FormError = "Workspace access changed. Review your choices and submit again."
	case errors.Is(err, ErrInput):
		form.FormError = "Choose a valid role and repository access, then submit again."
	default:
		status = http.StatusServiceUnavailable
		form.FormError = "The invitation could not be saved. Your selections are still here; please try again."
		h.logger.ErrorContext(r.Context(), "create workspace invitation", "error", err)
	}
	p.Invite = form
	h.renderWorkspace(w, r, p, status)
}
