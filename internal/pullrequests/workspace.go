package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
)

// ValidateSelection protects old query-based bookmarks before redirecting them.
func (h *Handler) ValidateSelection(ctx context.Context, userID, repoID int64, params url.Values) error {
	_, err := h.page(ctx, userID, repoID, params)
	return err
}

func (h *Handler) page(ctx context.Context, userID, repoID int64, params url.Values) (pages.Page, error) {
	if err := publicid.Selection(ctx, repoID, params); err != nil {
		return pages.Page{}, installations.ErrWorkspaceInput
	}
	f, number, err := workspaceQuery(params)
	if err != nil {
		return pages.Page{}, err
	}
	data, err := h.repo.Page(ctx, userID, repoID, f)
	if err != nil {
		return pages.Page{}, panelError(err)
	}
	page := pages.Page{RepoID: repoID, Name: data.Repo.FullName, Mode: data.Repo.BuildMode, PrivatePreviews: data.Repo.PrivatePreviews,
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
		page.PrivatePreviewsReady = caps.PrivatePreviews
	}
	page.PrivatePreviewUnavailable = params.Get("preview_access") == "unavailable"
	if page.Syncing() && data.Sync.RequestedAt.Valid {
		page.SyncStale = syncStale(data.Sync, time.Now())
		page.SyncStaleAt = data.Sync.RequestedAt.Time.Add(SyncStaleAfter).UTC().Format(time.RFC3339)
	}
	for _, row := range data.Rows {
		page.Rows = append(page.Rows, h.listPR(row, page.WorkerMissing))
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

// workspaceQuery reads the PR list's filter and the selected PR's number
// from a workspace URL; anything malformed is ErrWorkspaceInput.
func workspaceQuery(params url.Values) (f Filter, number int64, err error) {
	f = Filter{Page: 1, State: params.Get("pr_state"), Search: strings.TrimSpace(params.Get("pr_q"))}
	if f.State == "" {
		f.State = environments.StateOpen
	}
	if value := params.Get("pr_page"); value != "" {
		if f.Page, err = strconv.ParseInt(value, 10, 32); err != nil || f.Page < 1 {
			return Filter{}, 0, installations.ErrWorkspaceInput
		}
	}
	switch f.State {
	case environments.StateOpen, environments.StateClosed, environments.StateMerged, "all":
	default:
		return Filter{}, 0, installations.ErrWorkspaceInput
	}
	if len([]rune(f.Search)) > 100 {
		return Filter{}, 0, installations.ErrWorkspaceInput
	}
	if value := params.Get("pr"); value != "" {
		if number, err = strconv.ParseInt(value, 10, 32); err != nil || number < 1 {
			return Filter{}, 0, installations.ErrWorkspaceInput
		}
	}
	return f, number, nil
}

// listPR maps a PR to its row in the list.
func (h *Handler) listPR(row dbgen.ListPRsRow, noWorker bool) pages.PR {
	pr := pages.PR{Number: row.Number, Title: row.Title, Author: row.Author,
		Branch: row.HeadBranch, SHA: row.HeadSha, State: DisplayStatus(row.State, row.BuildStatus), Build: row.BuildState,
		Fork: row.IsFork, NeedsRefresh: row.NeedsRefresh, RequestCurrent: row.RequestCurrent,
		DeployedSHA: row.DeployedSha, Environment: row.EnvironmentStatus, NoWorker: noWorker,
		URL: row.HtmlUrl, UpdatedAt: timestamp(row.SourceUpdatedAt)}
	if url := h.previewURL(row.Hostname); url != "" && previewDeployed(row.DeployedSha, row.EnvironmentStatus) {
		pr.PreviewURL, pr.OlderPreview = url, row.DeployedSha != row.HeadSha
	}
	if pr.State == "checking" && row.MergeabilityUnresolvedAt.Valid {
		pr.State, pr.MergeUnknown = "unknown", true
	}
	return pr
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
	if location, ok := returnLocation(r, repoID); ok {
		separator := "?"
		if strings.Contains(location, "?") {
			separator = "&"
		}
		switch {
		case r.FormValue("return") == "review" && r.PostForm.Get("reset") == "true":
			location += separator + "saved=reset"
		case strings.HasSuffix(r.URL.Path, "/build"):
			location += separator + "saved=build"
		case strings.HasSuffix(r.URL.Path, "/refresh"):
			location += separator + "saved=sync"
		}
		publicid.Redirect(w, r, location, http.StatusSeeOther)
		return
	}
	location := repositorypages.WorkspaceURL(repoID, params)
	if r.Header.Get("HX-Request") != "true" {
		publicid.Redirect(w, r, location, http.StatusSeeOther)
		return
	}
	page, err := h.page(r.Context(), userID, repoID, params)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := publicid.SetHeader(w, r, "HX-Push-Url", location); err != nil {
		http.Error(w, "Unable to prepare the page address.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Add("Vary", "HX-Request, HX-Target")
	publicid.Handler(r, pages.Panel(page, h.users.CSRFToken(r.Context())), page).ServeHTTP(w, r)
}

// setupLocation is a repository's Setup tab, on the pull request the form
// came from (a fixed path; only its pr selector comes from the form).
func setupLocation(repoID int64, params url.Values) string {
	location := "/repositories/" + strconv.FormatInt(repoID, 10) + "/setup"
	if pr := params.Get("pr"); pr != "" {
		location += "?pr=" + url.QueryEscape(pr)
	}
	return location
}

// returnLocation is where an action started on another page goes back to:
// the Setup tab (return=setup) or the pull request's review page
// (return=review, Reset test data). workspace_query names the pull request.
func returnLocation(r *http.Request, repoID int64) (string, bool) {
	params, err := url.ParseQuery(r.FormValue("workspace_query"))
	if err != nil {
		params = url.Values{}
	}
	switch r.FormValue("return") {
	case "setup":
		return setupLocation(repoID, params), true
	case "review":
		if pr := params.Get("pr"); pr != "" {
			return "/repositories/" + strconv.FormatInt(repoID, 10) + "/pull-requests/" + url.PathEscape(pr) + "/review", true
		}
	}
	return "", false
}

// refusedFromSetup sends a refused request from the Setup tab or the review
// page back there with a fixed notice code, instead of a plain-text error
// page.
func refusedFromSetup(w http.ResponseWriter, r *http.Request, repoID int64, code string) bool {
	location, ok := returnLocation(r, repoID)
	if !ok {
		return false
	}
	separator := "?"
	if strings.Contains(location, "?") {
		separator = "&"
	}
	publicid.Redirect(w, r, location+separator+"notice="+code, http.StatusSeeOther)
	return true
}
