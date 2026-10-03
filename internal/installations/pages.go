package installations

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
)

func (h *Handler) repositories(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "HX-Request, HX-Target")
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
	userID, _, _ := h.users.UserIDs(r.Context())
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
		http.Redirect(w, r, repositorypages.PageURL(result.Page, filter.Search, filter.Access), http.StatusSeeOther)
		return
	}
	page := repositorypages.Page{Total: result.Total, Number: result.Page, Pages: result.Pages, Search: filter.Search, Access: filter.Access, Installations: summary.Installations, Enabled: h.slug != ""}
	if result.Total > 0 {
		page.Start = (result.Page-1)*RepositoryPageSize + 1
		page.End = page.Start + int64(len(result.Rows)) - 1
	}
	for _, row := range result.Rows {
		page.Rows = append(page.Rows, repositorypages.Row{FullName: row.FullName, Account: row.Account, InstallationID: row.InstallationID, Status: row.Status, Accessible: row.Accessible})
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "repository-results" {
		templ.Handler(repositorypages.Results(page)).ServeHTTP(w, r)
		return
	}
	page.Connected = h.sessions.PopString(r.Context(), "installation_notice") == "linked"
	username, _ := h.users.Username(r.Context())
	templ.Handler(repositorypages.Index(username, h.users.CSRFToken(r.Context()), page)).ServeHTTP(w, r)
}
