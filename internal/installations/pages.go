package installations

import (
	"errors"
	"github.com/ajaykathwatee/previa/internal/platform/publicid"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
)

func (h *Handler) repositories(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "HX-Request, HX-Target")
	var selected int64
	if value := r.URL.Query().Get("repo_id"); value != "" {
		var err error
		selected, err = publicid.Value(r, "repository", value, 0)
		if err != nil || selected <= 0 {
			h.fail(w, r, http.StatusBadRequest, "Choose a valid repository.")
			return
		}
	}
	userID, _, _ := h.users.UserIDs(r.Context())
	partial := r.Header.Get("HX-Request") == "true"
	if selected > 0 {
		h.openWorkspace(w, r, userID, selected, partial)
		return
	}
	filter, problem := repositoryFilter(r.URL.Query())
	if problem != "" {
		h.fail(w, r, http.StatusBadRequest, problem)
		return
	}
	result, err := h.service.Page(r.Context(), userID, filter)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "load repository page", "error", err)
		h.fail(w, r, http.StatusServiceUnavailable, "Repositories are temporarily unavailable. Please try again.")
		return
	}
	summary, err := h.service.Summary(r.Context(), userID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "load repository summary", "error", err)
		h.fail(w, r, http.StatusServiceUnavailable, "Repository connections are temporarily unavailable. Please try again.")
		return
	}
	if result.Page != filter.Page {
		// The requested page no longer exists: show the last one under its own URL.
		query := r.URL.Query()
		query.Set("page", strconv.FormatInt(result.Page, 10))
		location := repositorypages.WorkspaceURL(selected, query)
		if !partial {
			publicid.Redirect(w, r, location, http.StatusSeeOther)
			return
		}
		if err := publicid.SetHeader(w, r, "HX-Replace-Url", location); err != nil {
			http.Error(w, "Unable to prepare the page address.", http.StatusServiceUnavailable)
			return
		}
	}
	page := repositoryPage(result, filter, summary.Installations, h.slug != "", r.URL.RawQuery)
	if partial && r.Header.Get("HX-Target") == "repository-results" {
		publicid.Handler(r, repositorypages.Results(page), page).ServeHTTP(w, r)
		return
	}
	page.Connected = h.sessions.PopString(r.Context(), "installation_notice") == "linked"
	username, _ := h.users.Username(r.Context())
	publicid.Handler(r, repositorypages.Index(username, h.users.CSRFToken(r.Context()), page), page).ServeHTTP(w, r)
}

// openWorkspace sends an old query-based bookmark of a repository to its
// workspace page, after checking the selection.
func (h *Handler) openWorkspace(w http.ResponseWriter, r *http.Request, userID, selected int64, partial bool) {
	if h.navigator == nil {
		http.NotFound(w, r)
		return
	}
	if err := h.navigator.ValidateSelection(r.Context(), userID, selected, r.URL.Query()); err != nil {
		h.panelFailure(w, r, err)
		return
	}
	location := repositorypages.WorkspaceURL(selected, r.URL.Query())
	if partial {
		if err := publicid.SetHeader(w, r, "HX-Redirect", location); err != nil {
			http.Error(w, "Unable to prepare the page address.", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	publicid.Redirect(w, r, location, http.StatusSeeOther)
}

// repositoryFilter reads the repository list's filter; a malformed one is
// explained in problem.
func repositoryFilter(query url.Values) (filter RepositoryFilter, problem string) {
	filter = RepositoryFilter{Page: 1, Search: strings.TrimSpace(query.Get("q")), Access: query.Get("access")}
	if filter.Access == "" {
		filter.Access = "available"
	}
	if value := query.Get("page"); value != "" {
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil || number < 1 {
			return RepositoryFilter{}, "Page must be a positive number."
		}
		filter.Page = number
	}
	if len([]rune(filter.Search)) > 100 || filter.Access != "all" && filter.Access != "available" && filter.Access != "unavailable" {
		return RepositoryFilter{}, "Use a search of up to 100 characters and a valid access filter."
	}
	return filter, ""
}

// repositoryPage maps one page of repositories to display data.
func repositoryPage(result RepositoryPage, filter RepositoryFilter, installations int64, enabled bool, query string) repositorypages.Page {
	page := repositorypages.Page{Total: result.Total, Number: result.Page, Pages: result.Pages, Search: filter.Search, Access: filter.Access,
		Installations: installations, Enabled: enabled, Query: query}
	if result.Total > 0 {
		page.Start = (result.Page-1)*RepositoryPageSize + 1
		page.End = page.Start + int64(len(result.Rows)) - 1
	}
	for _, row := range result.Rows {
		synced := ""
		if !row.LastSynced.IsZero() {
			synced = row.LastSynced.UTC().Format(time.RFC3339)
		}
		page.Rows = append(page.Rows, repositorypages.Row{ID: row.ID, FullName: row.FullName, Account: row.Account, InstallationID: row.InstallationID, Status: row.Status, Accessible: row.Accessible,
			BuildMode: row.BuildMode, OpenPRs: row.OpenPRs, LivePreviews: row.LivePreviews, LastSynced: synced})
	}
	return page
}

func (h *Handler) panelFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrPolicyUnavailable):
		http.NotFound(w, r)
	case errors.Is(err, ErrWorkspaceInput):
		h.fail(w, r, http.StatusBadRequest, "Use valid pull request filters and a positive page or PR number.")
	default:
		h.logger.ErrorContext(r.Context(), "load repository panel", "error", err)
		h.fail(w, r, http.StatusServiceUnavailable, "Pull requests are temporarily unavailable. Please retry.")
	}
}
