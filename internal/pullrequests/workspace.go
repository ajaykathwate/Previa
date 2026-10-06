package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/internal/installations"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
)

// ValidateSelection protects old query-based bookmarks before redirecting them.
func (h *Handler) ValidateSelection(ctx context.Context, userID, repoID int64, params url.Values) error {
	_, err := h.page(ctx, userID, repoID, params)
	return err
}

func (h *Handler) page(ctx context.Context, userID, repoID int64, params url.Values) (pages.Page, error) {
	f := Filter{Page: 1, State: params.Get("pr_state"), Search: strings.TrimSpace(params.Get("pr_q"))}
	if f.State == "" {
		f.State = "open"
	}
	if value := params.Get("pr_page"); value != "" {
		var err error
		f.Page, err = strconv.ParseInt(value, 10, 32)
		if err != nil || f.Page < 1 {
			return pages.Page{}, installations.ErrWorkspaceInput
		}
	}
	if len([]rune(f.Search)) > 100 || f.State != "open" && f.State != "closed" && f.State != "merged" && f.State != "all" {
		return pages.Page{}, installations.ErrWorkspaceInput
	}
	var number int64
	if value := params.Get("pr"); value != "" {
		var err error
		number, err = strconv.ParseInt(value, 10, 32)
		if err != nil || number < 1 {
			return pages.Page{}, installations.ErrWorkspaceInput
		}
	}
	data, err := h.repo.Page(ctx, userID, repoID, f)
	if err != nil {
		return pages.Page{}, panelError(err)
	}
	page := pages.Page{RepoID: repoID, Name: data.Repo.FullName, Mode: data.Repo.BuildMode,
		Available: data.Repo.Accessible && data.Repo.Status == "active", DiscoveryEnabled: h.enabled,
		SyncState: data.Sync.State, SyncMessage: data.Sync.Message, LastRefresh: timestamp(data.Sync.LastSuccessAt),
		Fetched: data.Sync.Fetched, Number: data.Page, Pages: data.Pages, Total: data.Total,
		Search: f.Search, State: f.State, WorkspaceQuery: params.Encode(), CSRF: h.users.CSRFToken(ctx)}
	if h.actions != nil {
		caps, err := h.actions.Capabilities.Current(ctx)
		if err != nil {
			return pages.Page{}, err
		}
		page.WorkerMissing = !caps.Discovery
	}
	if page.Syncing() && data.Sync.RequestedAt.Valid {
		page.SyncStale = syncStale(data.Sync, time.Now())
		page.SyncStaleAt = data.Sync.RequestedAt.Time.Add(SyncStaleAfter).UTC().Format(time.RFC3339)
	}
	for _, row := range data.Rows {
		pr := pages.PR{Number: row.Number, Title: row.Title, Author: row.Author,
			Branch: row.HeadBranch, SHA: row.HeadSha, State: DisplayStatus(row.State, row.BuildStatus), Build: row.BuildState,
			Fork: row.IsFork, NeedsRefresh: row.NeedsRefresh, RequestCurrent: row.RequestCurrent,
			DeployedSHA: row.DeployedSha, Environment: row.EnvironmentStatus}
		live := row.DeployedSha != "" && (row.EnvironmentStatus == "running" || row.EnvironmentStatus == "failed" || row.EnvironmentStatus == "publishing")
		if live && h.actions != nil && h.actions.PreviewBaseDomain != "" && row.Hostname != "" {
			pr.PreviewURL = "https://" + row.Hostname + "." + h.actions.PreviewBaseDomain
			pr.OlderPreview = row.DeployedSha != row.HeadSha
		}
		if pr.State == "checking" && row.MergeabilityUnresolvedAt.Valid {
			pr.State, pr.MergeUnknown = "unknown", true
		}
		pr.NoWorker = page.WorkerMissing
		page.Rows = append(page.Rows, pr)
	}
	if number > 0 {
		row, err := h.repo.Detail(ctx, userID, repoID, int32(number))
		if err != nil {
			return pages.Page{}, panelError(err)
		}
		pr, err := h.detailPR(ctx, row)
		if err != nil {
			return pages.Page{}, err
		}
		page.SelectedPR = &pr
	}
	return page, nil
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
	page, err := h.page(r.Context(), userID, repoID, params)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("HX-Push-Url", location)
	w.Header().Add("Vary", "HX-Request, HX-Target")
	templ.Handler(pages.Panel(page, h.users.CSRFToken(r.Context()))).ServeHTTP(w, r)
}
