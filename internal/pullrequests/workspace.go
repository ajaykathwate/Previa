package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/installations"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
)

// RepositoryPanel is the small presentation boundary consumed by installations.
// Full workspace, panel selection, polling and PR filters share ownership checks.
func (h *Handler) RepositoryPanel(ctx context.Context, userID, repoID int64, params url.Values, csrf, target string) (templ.Component, error) {
	f := Filter{Page: 1, State: params.Get("pr_state"), Search: strings.TrimSpace(params.Get("pr_q"))}
	if f.State == "" {
		f.State = "open"
	}
	if value := params.Get("pr_page"); value != "" {
		var err error
		f.Page, err = strconv.ParseInt(value, 10, 32)
		if err != nil || f.Page < 1 {
			return nil, installations.ErrWorkspaceInput
		}
	}
	if len([]rune(f.Search)) > 100 || f.State != "open" && f.State != "closed" && f.State != "merged" && f.State != "all" {
		return nil, installations.ErrWorkspaceInput
	}
	var number int64
	if value := params.Get("pr"); value != "" {
		var err error
		number, err = strconv.ParseInt(value, 10, 32)
		if err != nil || number < 1 {
			return nil, installations.ErrWorkspaceInput
		}
	}
	data, err := h.repo.Page(ctx, userID, repoID, f)
	if err != nil {
		return nil, panelError(err)
	}
	page := pages.Page{RepoID: repoID, Name: data.Repo.FullName, Mode: data.Repo.BuildMode,
		Available: data.Repo.Accessible && data.Repo.Status == "active", DiscoveryEnabled: h.enabled,
		SyncState: data.Sync.State, SyncMessage: data.Sync.Message, LastRefresh: timestamp(data.Sync.LastSuccessAt),
		Fetched: data.Sync.Fetched, Number: data.Page, Pages: data.Pages, Total: data.Total,
		Search: f.Search, State: f.State, WorkspaceQuery: params.Encode()}
	for _, row := range data.Rows {
		page.Rows = append(page.Rows, pages.PR{Number: row.Number, Title: row.Title, Author: row.Author,
			Branch: row.HeadBranch, SHA: row.HeadSha, State: row.State, Build: row.BuildState,
			Fork: row.IsFork, NeedsRefresh: row.NeedsRefresh})
	}
	if target == "pull-request-results" {
		return pages.Results(page), nil
	}
	if number > 0 {
		row, err := h.repo.Detail(ctx, userID, repoID, int32(number))
		if err != nil {
			return nil, panelError(err)
		}
		page.SelectedPR = &pages.PR{Number: row.Number, Title: row.Title, Author: row.Author,
			Branch: row.HeadBranch, SHA: row.HeadSha, State: row.State, Build: row.BuildState,
			URL: row.HtmlUrl, RequestedSHA: row.RequestedSha, DeployedSHA: row.DeployedSha,
			Environment: row.EnvironmentStatus, Available: row.Available.Bool,
			Deadline: timestamp(row.AvailableAt), Fork: row.IsFork, NeedsRefresh: row.NeedsRefresh}
	}
	return pages.Panel(page, csrf), nil
}

func panelError(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return installations.ErrPolicyUnavailable
	}
	return fmt.Errorf("load selected repository: %w", err)
}

func (h *Handler) finishMutation(w http.ResponseWriter, r *http.Request, userID, repoID int64) {
	params, err := url.ParseQuery(r.FormValue("workspace_query"))
	if err != nil {
		http.Error(w, "Invalid workspace filters.", http.StatusBadRequest)
		return
	}
	location := repositorypages.WorkspaceURL(repoID, params)
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, location, http.StatusSeeOther)
		return
	}
	panel, err := h.RepositoryPanel(r.Context(), userID, repoID, params, h.users.CSRFToken(r.Context()), "repository-detail")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Push-Url", location)
	w.Header().Add("Vary", "HX-Request, HX-Target")
	templ.Handler(panel).ServeHTTP(w, r)
}
