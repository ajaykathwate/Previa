package workspaces

import (
	"errors"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	pages "github.com/ajaykathwatee/previa/web/pages/workspaces"
	"github.com/go-chi/chi/v5"
)

type Handler struct {
	service *Service
	users   UserSession
	logger  *slog.Logger
	appURL  string
}

func NewHandler(s *Service, u UserSession, l *slog.Logger, appURL string) *Handler {
	return &Handler{s, u, l, strings.TrimSuffix(appURL, "/")}
}
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/workspaces", h.index)
		r.Post("/workspaces", h.create)
		r.Get("/workspaces/{workspaceID}", h.page)
		r.Get("/workspaces/{workspaceID}/members", h.page)
		r.Get("/workspaces/{workspaceID}/settings", h.page)
		r.Get("/workspaces/{workspaceID}/setup", h.page)
		r.Post("/workspaces/{workspaceID}/delete", h.deleteWorkspace)
		r.Get("/workspaces/{workspaceID}/members/{userID}", h.memberProfile)
		r.Get("/workspaces/{workspaceID}/previews", h.legacyPreviews)
		r.Post("/workspaces/{workspaceID}/settings", h.updateProfile)
		r.Get("/workspaces/{workspaceID}/logo", h.logo)
		r.Post("/workspaces/{workspaceID}/logo", h.uploadLogo)
		r.Post("/workspaces/{workspaceID}/logo/remove", h.removeLogo)
		r.Post("/workspaces/{workspaceID}/select", h.selectWorkspace)
		r.Post("/workspaces/{workspaceID}/members/{userID}", h.changeMember)
		r.Post("/workspaces/{workspaceID}/invitations", h.invite)
		r.Post("/workspaces/{workspaceID}/invitations/{invitationID}", h.changeInvitation)
		r.Post("/workspaces/{workspaceID}/ownership", h.ownership)
		r.Get("/workspace-invitations/{token}", h.invitation)
		r.Post("/workspace-invitations/{token}", h.respondInvitation)
		r.Post("/workspace-invitations/received/{invitationID}", h.respondReceived)
	})
}
func parseID(value string) int64 { n, _ := strconv.ParseInt(value, 10, 64); return n }
func (h *Handler) identity(r *http.Request) (int64, int64) {
	u, _, _ := h.users.UserIDs(r.Context())
	return u, publicid.PathID(r, "workspaceID", "workspace")
}
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := http.StatusServiceUnavailable, "Workspace temporarily unavailable. Please try again."
	switch {
	case errors.Is(err, ErrUnavailable):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrInput):
		status, message = http.StatusBadRequest, "Check the workspace name, GitHub account, role, and selected repositories."
	case errors.Is(err, ErrLogo):
		status, message = http.StatusBadRequest, ErrLogo.Error()
	case errors.Is(err, ErrConflict):
		status, message = http.StatusConflict, "The workspace, member, or invitation changed. Reload to see its current state; no change was saved."
	case errors.Is(err, ErrOwner):
		status, message = http.StatusConflict, "Transfer ownership to an existing administrator before leaving or removing the owner."
	case errors.Is(err, ErrInvitation):
		status, message = http.StatusForbidden, "This invitation is expired, revoked, already answered, or addressed to another GitHub account. Ask an administrator for a new link."
	case errors.Is(err, ErrSourceUnavailable):
		message = "GitHub could not verify that account or source permission. Please try again."
	case errors.Is(err, ErrDeleteMembers), errors.Is(err, ErrDeleteInstallation), errors.Is(err, ErrDeleteConfirmation):
		status, message = http.StatusConflict, err.Error()
	default:
		h.logger.ErrorContext(r.Context(), "workspace request failed", "error", err)
	}
	_, id := h.identity(r)
	target, label := "/workspaces", "Return to workspaces"
	if id > 0 {
		target, label = "/workspaces/"+pages.ID(id)+"/members", "Return to members"
		if strings.HasSuffix(r.URL.Path, "/ownership") {
			target, label = "/workspaces/"+pages.ID(id)+"/settings", "Return to workspace settings"
		}
	}
	login, _ := h.users.Username(r.Context())
	publicid.Handler(r, pages.Failure(login, h.users.CSRFToken(r.Context()), message, target, label), nil, templ.WithStatus(status)).ServeHTTP(w, r)
}
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	items, err := h.service.List(r.Context(), u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p := pages.Page{ActorID: u, CSRF: h.users.CSRFToken(r.Context())}
	for _, v := range items {
		p.Workspaces = append(p.Workspaces, displayWorkspace(v))
	}
	_, githubID, _ := h.users.UserIDs(r.Context())
	if p.Received, err = h.service.ReceivedInvitations(r.Context(), githubID); err != nil {
		h.fail(w, r, err)
		return
	}
	p.Message = directorySaved[r.URL.Query().Get("saved")]
	login, _ := h.users.Username(r.Context())
	publicid.Handler(r, pages.Directory(login, p), p).ServeHTTP(w, r)
}
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, "")
}
func (h *Handler) renderPage(w http.ResponseWriter, r *http.Request, invitationToken string) {
	u, id := h.identity(r)
	tab := "members"
	switch {
	case strings.HasSuffix(r.URL.Path, "/settings"):
		tab = "settings"
	case strings.HasSuffix(r.URL.Path, "/setup") || r.PostFormValue("return") == "setup":
		tab = "setup"
	}
	view := r.URL.Query().Get("view")
	p, err := h.service.Page(r.Context(), u, id, tab, PageQuery{Page: parseID(r.URL.Query().Get("page")), Search: r.URL.Query().Get("q"), Invitations: view == "invitations", Deactivated: view == "deactivated"})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if subject := r.URL.Query().Get("reinvite"); subject != "" && tab == "members" {
		if userID, e := publicid.Value(r, "user", subject, 0); e == nil {
			if p.Invite, err = h.service.ReinviteForm(r.Context(), u, id, userID); err != nil {
				h.fail(w, r, err)
				return
			}
		}
	}
	p.CSRF = h.users.CSRFToken(r.Context())
	if invitationToken != "" {
		p.InvitationURL = h.appURL + "/workspace-invitations/" + invitationToken
	}
	if r.URL.Query().Get("saved") == "1" {
		p.Message = "Workspace updated"
	}
	switch r.URL.Query().Get("saved") {
	case "details":
		p.Message = "Workspace details saved"
	case "logo":
		p.Message = "Workspace logo updated"
	case "logo-removed":
		p.Message = "Workspace logo removed"
	case "joined":
		p.Message = "Welcome to " + p.Name
	}
	h.renderWorkspace(w, r, p, http.StatusOK)
}
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, id int64) {
	publicid.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/members?saved=1", http.StatusSeeOther)
}

// create opens the new workspace on its setup checklist: invite teammates,
// connect repositories, describe the team.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	u, _, _ := h.users.UserIDs(r.Context())
	id, err := h.service.Create(r.Context(), u, r.PostFormValue("name"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	publicid.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/setup", http.StatusSeeOther)
}
func (h *Handler) selectWorkspace(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	if err := h.service.Select(r.Context(), u, id); err != nil {
		h.fail(w, r, err)
		return
	}
	publicid.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}
func repositoryIDs(r *http.Request) ([]int64, error) {
	if err := r.ParseForm(); err != nil {
		return nil, ErrInput
	}
	values := r.PostForm["repository"]
	if len(values) > 500 {
		return nil, ErrInput
	}
	out := make([]int64, 0, len(values))
	for _, v := range values {
		id, err := publicid.Value(r, "repository", v, 0)
		if err != nil || id <= 0 {
			return nil, ErrInput
		}
		out = append(out, id)
	}
	return out, nil
}
func (h *Handler) changeMember(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	repos, err := repositoryIDs(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	c := MemberChange{ActorID: u, WorkspaceID: id, WorkspaceRevision: parseID(r.PostFormValue("workspace_revision")), UserID: publicid.PathID(r, "userID", "user"), Revision: parseID(r.PostFormValue("revision")), Role: r.PostFormValue("role"), Repositories: repos, Remove: r.PostFormValue("action") == "remove"}
	if err := h.service.ChangeMember(r.Context(), c); err != nil {
		h.memberFailure(w, r, err)
		return
	}
	if c.Remove && c.UserID == u {
		publicid.Redirect(w, r, "/workspaces", http.StatusSeeOther)
		return
	}
	h.redirect(w, r, id)
}
func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	repos, err := repositoryIDs(r)
	if err != nil {
		h.invitationFailure(w, r, err, nil)
		return
	}
	token, err := h.service.Invite(r.Context(), InvitationCommand{ActorID: u, WorkspaceID: id, Revision: parseID(r.PostFormValue("workspace_revision")), Login: r.PostFormValue("login"), Role: r.PostFormValue("role"), Repositories: repos})
	if err != nil {
		h.invitationFailure(w, r, err, repos)
		return
	}
	h.renderPage(w, r, token)
}

func (h *Handler) changeInvitation(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	token, err := h.service.ChangeInvitation(r.Context(), u, id, parseID(r.PostFormValue("workspace_revision")), publicid.PathID(r, "invitationID", "invitation"), parseID(r.PostFormValue("revision")), r.PostFormValue("action") == "regenerate")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if token != "" {
		h.renderPage(w, r, token)
		return
	}
	h.redirect(w, r, id)
}
func (h *Handler) ownership(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	revision := parseID(r.PostFormValue("workspace_revision"))
	var err error
	switch r.PostFormValue("action") {
	case "accept":
		err = h.service.AcceptOwnership(r.Context(), u, id, revision, parseID(r.PostFormValue("transfer_revision")))
	case "cancel":
		err = h.service.CancelOwnership(r.Context(), u, id, revision)
	case "nominate":
		err = h.service.NominateOwner(r.Context(), u, id, revision, publicid.FormID(r, "target", "user"))
	default:
		err = ErrInput
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	publicid.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/settings?saved=1", http.StatusSeeOther)
}
func (h *Handler) invitation(w http.ResponseWriter, r *http.Request) {
	u, githubID, _ := h.users.UserIDs(r.Context())
	token := chi.URLParam(r, "token")
	v, err := h.service.Invitation(r.Context(), githubID, token)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p := pages.Page{Workspace: pages.Workspace{ID: v.WorkspaceID, Name: v.Name, Role: v.Role}, ActorID: u, CSRF: h.users.CSRFToken(r.Context()), InvitationURL: "/workspace-invitations/" + token}
	repos, err := h.service.InvitationScope(r.Context(), githubID, token)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.Repositories = repos
	login, _ := h.users.Username(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	publicid.Handler(r, pages.AcceptInvitation(login, p), p).ServeHTTP(w, r)
}
func (h *Handler) respondInvitation(w http.ResponseWriter, r *http.Request) {
	u, githubID, _ := h.users.UserIDs(r.Context())
	action := r.PostFormValue("action")
	if action != "accept" && action != "decline" {
		h.fail(w, r, ErrInput)
		return
	}
	id, err := h.service.RespondInvitation(r.Context(), u, githubID, chi.URLParam(r, "token"), action == "accept")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if action == "decline" {
		publicid.Redirect(w, r, "/workspaces?saved=declined", http.StatusSeeOther)
		return
	}
	publicid.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/members?saved=joined", http.StatusSeeOther)
}

var directorySaved = map[string]string{"deleted": "Workspace deleted", "declined": "Invitation declined"}

// memberFailure keeps a refused member change on the Members page, with the
// reason shown above the list instead of a separate error page.
func (h *Handler) memberFailure(w http.ResponseWriter, r *http.Request, err error) {
	u, id := h.identity(r)
	p, loadErr := h.service.Page(r.Context(), u, id, "members", PageQuery{Page: 1})
	if errors.Is(err, ErrUnavailable) || loadErr != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusConflict
	switch {
	case errors.Is(err, ErrOwner):
		p.Error = "Transfer ownership to an administrator before the owner leaves or is removed."
	case errors.Is(err, ErrConflict):
		p.Error = "This member changed in another window. The list below is current; no change was saved."
	case errors.Is(err, ErrInput):
		status, p.Error = http.StatusBadRequest, "Choose a role and repositories, then try again."
	default:
		h.logger.ErrorContext(r.Context(), "change workspace member", "error", err)
		status, p.Error = http.StatusServiceUnavailable, "The change could not be saved, and nothing was changed. Try again; if it keeps failing, the server log has the cause."
	}
	h.renderWorkspace(w, r, p, status)
}

func (h *Handler) memberProfile(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	d, err := h.service.Member(r.Context(), u, id, publicid.PathID(r, "userID", "user"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d.CSRF = h.users.CSRFToken(r.Context())
	login, _ := h.users.Username(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	publicid.Handler(r, pages.MemberPage(login, d), d).ServeHTTP(w, r)
}

func (h *Handler) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	u, id := h.identity(r)
	if err := h.service.Delete(r.Context(), u, id, parseID(r.PostFormValue("workspace_revision")), r.PostFormValue("confirm")); err != nil {
		h.fail(w, r, err)
		return
	}
	publicid.Redirect(w, r, "/workspaces?saved=deleted", http.StatusSeeOther)
}

func (h *Handler) respondReceived(w http.ResponseWriter, r *http.Request) {
	u, githubID, _ := h.users.UserIDs(r.Context())
	action := r.PostFormValue("action")
	if action != "accept" && action != "decline" {
		h.fail(w, r, ErrInput)
		return
	}
	id, err := h.service.RespondReceivedInvitation(r.Context(), u, githubID, publicid.PathID(r, "invitationID", "invitation"), action == "accept")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if action == "decline" {
		publicid.Redirect(w, r, "/workspaces?saved=declined", http.StatusSeeOther)
		return
	}
	publicid.Redirect(w, r, "/workspaces/"+pages.ID(id)+"/members?saved=joined", http.StatusSeeOther)
}
