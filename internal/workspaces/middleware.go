package workspaces

import (
	"context"
	"errors"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ajaykathwatee/previa/internal/platform/scope"
	"github.com/ajaykathwatee/previa/web/components"
)

type UserSession interface {
	UserIDs(context.Context) (int64, int64, bool)
	Username(context.Context) (string, bool)
	CSRFToken(context.Context) string
	SelectedWorkspaceID(context.Context) int64
	RememberWorkspace(context.Context, int64)
}

type Middleware struct {
	service *Service
	users   UserSession
	logger  *slog.Logger
}

func NewMiddleware(s *Service, u UserSession, l *slog.Logger) *Middleware {
	return &Middleware{s, u, l}
}

func (m *Middleware) Load(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := m.users.UserIDs(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		items, err := m.service.List(r.Context(), user)
		if err != nil {
			m.fail(w, r, err)
			return
		}
		selection, err := m.selectWorkspace(r, user, items)
		if err != nil {
			m.fail(w, r, err)
			return
		}
		if selection.ID == 0 && needsWorkspace(r.URL.Path) {
			publicid.Redirect(w, r, "/workspaces", http.StatusSeeOther)
			return
		}
		shell := components.WorkspaceShell{ID: selection.ID, Name: selection.Name, Role: selection.Role, LogoVersion: selection.LogoVersion, CSRF: m.users.CSRFToken(r.Context())}
		for _, v := range items {
			shell.Items = append(shell.Items, components.WorkspaceItem{ID: v.ID, Name: v.Name, Role: v.Role, LogoVersion: v.LogoVersion})
		}
		ctx := scope.With(r.Context(), selection.ID, selection.Role)
		ctx = components.WithWorkspace(ctx, shell)
		r = r.WithContext(ctx)
		if !m.guard(w, r, user, selection) {
			return
		}
		m.users.RememberWorkspace(r.Context(), selection.ID)
		next.ServeHTTP(w, r)
	})
}
func (m *Middleware) selectWorkspace(r *http.Request, user int64, items []Workspace) (Workspace, error) {
	id := m.users.SelectedWorkspaceID(r.Context())
	if id == 0 {
		var err error
		id, err = m.service.repo.Preference(r.Context(), user)
		if err != nil {
			return Workspace{}, err
		}
	}
	if repo := repositoryPathID(r); repo > 0 {
		v, err := m.service.Authorize(r.Context(), user, repo, "read", false)
		if err != nil {
			return Workspace{}, err
		}
		id = v.WorkspaceID.Int64
	}
	if strings.HasPrefix(r.URL.Path, "/workspaces/") {
		part := strings.Split(strings.TrimPrefix(r.URL.Path, "/workspaces/"), "/")[0]
		if n, e := publicid.Value(r, "workspace", part, 0); e == nil && n > 0 {
			id = n
		}
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	if id > 0 && (repositoryPathID(r) > 0 || strings.HasPrefix(r.URL.Path, "/workspaces/")) {
		return Workspace{}, ErrUnavailable
	}
	if len(items) > 0 {
		return items[0], nil
	}
	return Workspace{}, nil
}
func repositoryPathID(r *http.Request) int64 {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/repositories/") {
		return 0
	}
	part := strings.Split(strings.TrimPrefix(path, "/repositories/"), "/")[0]
	id, err := publicid.Value(r, "repository", part, 0)
	if err != nil {
		return 0
	}
	return id
}
func needsWorkspace(path string) bool {
	return path == "/dashboard" || strings.HasPrefix(path, "/repositories") || path == "/builds" || path == "/previews" || path == "/installations/start"
}
func (m *Middleware) guard(w http.ResponseWriter, r *http.Request, user int64, selected Workspace) bool {
	if selected.Role == "reviewer" && (needsWorkspace(r.URL.Path) || r.URL.Path == "/starter-kits" || r.URL.Path == "/developer-tools") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return false
		}
		if r.URL.Path != "/previews" {
			publicid.Redirect(w, r, "/previews", http.StatusSeeOther)
			return false
		}
	}
	if r.URL.Path == "/installations/start" && !selected.Manage() {
		http.NotFound(w, r)
		return false
	}
	if (r.URL.Path == "/developer-tools" || strings.HasPrefix(r.URL.Path, "/queue/")) && !selected.Manage() {
		http.NotFound(w, r)
		return false
	}
	repo := repositoryPathID(r)
	if repo == 0 {
		return true
	}
	ability, source := repositoryAbility(r)
	if _, err := m.service.Authorize(r.Context(), user, repo, ability, source); err != nil {
		m.fail(w, r, err)
		return false
	}
	return true
}
func (m *Middleware) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrUnavailable) {
		http.NotFound(w, r)
		return
	}
	m.logger.WarnContext(r.Context(), "workspace authorization unavailable")
	http.Error(w, "Workspace or GitHub access could not be verified. Please try again.", http.StatusServiceUnavailable)
}

func repositoryAbility(r *http.Request) (string, bool) {
	path := r.URL.Path
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
	settings := strings.Contains(path, "/secrets") || strings.HasSuffix(path, "/policy") || strings.HasSuffix(path, "/preview-access") || strings.Contains(path, "/outbound")
	if settings {
		source := mutation
		if strings.HasSuffix(path, "/delete") || strings.HasSuffix(path, "/remove") || strings.HasSuffix(path, "/secrets/build") && r.PostFormValue("build") == "false" || strings.HasSuffix(path, "/preview-access") && r.PostFormValue("private") == "false" || strings.HasSuffix(path, "/policy") && r.PostFormValue("mode") == "manual" {
			source = false
		}
		return "manage", source
	}
	if mutation || strings.Contains(path, "/logs") || strings.Contains(path, "/output") {
		return "operate", true
	}
	return "read", false
}
