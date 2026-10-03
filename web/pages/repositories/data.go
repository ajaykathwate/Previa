package repositories

import (
	"net/url"
	"strconv"
	"strings"
)

type Row struct {
	ID                        int64
	FullName, Account, Status string
	InstallationID            int64
	Accessible                bool
}
type Page struct {
	Rows                                            []Row
	Total, Number, Pages, Start, End, Installations int64
	Search, Access                                  string
	Enabled, Connected                              bool
	Query                                           string
}

// Navigation is local and whitelisted; the PR number selects a dedicated path.
func WorkspaceURL(repoID int64, query url.Values) string {
	values := url.Values{}
	keys := []string{"q", "access", "page"}
	path := "/repositories"
	if repoID > 0 {
		path += "/" + strconv.FormatInt(repoID, 10)
		keys = append(keys, "pr_q", "pr_state", "pr_page")
		if number, err := strconv.ParseInt(query.Get("pr"), 10, 32); err == nil && number > 0 {
			path += "/pull-requests/" + strconv.FormatInt(number, 10)
		}
	}
	for _, key := range keys {
		if value, exists := query[key]; exists && len(value) > 0 {
			values.Set(key, value[0])
		}
	}
	if len(values) > 0 {
		path += "?" + values.Encode()
	}
	return path
}

func (p Page) URL(number int64) string {
	return PageURL(number, p.Search, p.Access)
}
func (p Page) SelectURL(repoID int64) string {
	v := url.Values{"q": {p.Search}, "access": {p.Access}, "page": {strconv.FormatInt(p.Number, 10)}}
	return WorkspaceURL(repoID, v)
}

func PageURL(number int64, search, access string) string {
	values := url.Values{"page": {strconv.FormatInt(number, 10)}}
	if search != "" {
		values.Set("q", search)
	}
	if access != "" {
		values.Set("access", access)
	}
	return "/repositories?" + values.Encode()
}
func PageNumbers(current, pages int64) []int64 {
	start := max(int64(1), current-2)
	end := min(pages, start+4)
	start = max(int64(1), end-4)
	result := make([]int64, 0, 5)
	for number := start; number <= end; number++ {
		result = append(result, number)
	}
	return result
}

func (r Row) Name() string {
	_, name, found := strings.Cut(r.FullName, "/")
	if !found {
		return r.FullName
	}
	return name
}
func (r Row) Owner() string {
	owner, _, found := strings.Cut(r.FullName, "/")
	if !found {
		return r.Account
	}
	return owner
}
