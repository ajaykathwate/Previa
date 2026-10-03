package installations

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
)

func (h *Handler) repositories(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "HX-Request, HX-Target")
	var selected int64
	if value := r.URL.Query().Get("repo_id"); value != "" {
		var err error
		selected, err = strconv.ParseInt(value, 10, 64)
		if err != nil || selected <= 0 {
			h.fail(w, r, http.StatusBadRequest, "Choose a valid repository.")
			return
		}
	}
	userID, _, _ := h.users.UserIDs(r.Context())
	target := r.Header.Get("HX-Target")
	partial := r.Header.Get("HX-Request") == "true"
	if selected > 0 {
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
			w.Header().Set("HX-Redirect", location)
			w.WriteHeader(http.StatusOK)
		} else {
			http.Redirect(w, r, location, http.StatusSeeOther)
		}
		return
	}

	filter := RepositoryFilter{Page: 1, Search: strings.TrimSpace(r.URL.Query().Get("q")), Access: r.URL.Query().Get("access")}
	if filter.Access == "" {
		filter.Access = "available"
	}
	if value := r.URL.Query().Get("page"); value != "" {
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil || number < 1 {
			h.fail(w, r, http.StatusBadRequest, "Page must be a positive number.")
			return
		}
		filter.Page = number
	}
	if len([]rune(filter.Search)) > 100 || filter.Access != "all" && filter.Access != "available" && filter.Access != "unavailable" {
		h.fail(w, r, http.StatusBadRequest, "Use a search of up to 100 characters and a valid access filter.")
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
		query := r.URL.Query()
		query.Set("page", strconv.FormatInt(result.Page, 10))
		location := repositorypages.WorkspaceURL(selected, query)
		if partial {
			w.Header().Set("HX-Replace-Url", location)
		} else {
			http.Redirect(w, r, location, http.StatusSeeOther)
			return
		}
	}
	page := repositorypages.Page{Total: result.Total, Number: result.Page, Pages: result.Pages, Search: filter.Search, Access: filter.Access, Installations: summary.Installations, Enabled: h.slug != "", Query: r.URL.RawQuery}
	if result.Total > 0 {
		page.Start = (result.Page-1)*RepositoryPageSize + 1
		page.End = page.Start + int64(len(result.Rows)) - 1
	}
	for _, row := range result.Rows {
		page.Rows = append(page.Rows, repositorypages.Row{ID: row.ID, FullName: row.FullName, Account: row.Account, InstallationID: row.InstallationID, Status: row.Status, Accessible: row.Accessible})
	}
	if partial && target == "repository-results" {
		templ.Handler(repositorypages.Results(page)).ServeHTTP(w, r)
		return
	}

	page.Connected = h.sessions.PopString(r.Context(), "installation_notice") == "linked"
	username, _ := h.users.Username(r.Context())
	templ.Handler(repositorypages.Index(username, h.users.CSRFToken(r.Context()), page)).ServeHTTP(w, r)
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
