package repositories

import (
	"net/url"
	"strconv"
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
	SelectedRepoID                                  int64
	Query                                           string
}

// Only workspace query keys survive navigation; actions never accept redirect URLs.
func WorkspaceURL(repoID int64, query url.Values) string {
	values := url.Values{}
	for _, key := range []string{"q", "access", "page", "pr_q", "pr_state", "pr_page", "pr"} {
		if value, exists := query[key]; exists && len(value) > 0 {
			values.Set(key, value[0])
		}
	}
	if repoID > 0 {
		values.Set("repo_id", strconv.FormatInt(repoID, 10))
	}
	if len(values) == 0 {
		return "/repositories"
	}
	return "/repositories?" + values.Encode()
}

func (p Page) URL(number int64) string {
	v, _ := url.ParseQuery(p.Query)
	v.Set("page", strconv.FormatInt(number, 10))
	v.Set("q", p.Search)
	v.Set("access", p.Access)
	return WorkspaceURL(p.SelectedRepoID, v)
}

func (p Page) SelectURL(repoID int64) string {
	v, _ := url.ParseQuery(p.Query)
	for _, key := range []string{"pr", "pr_q", "pr_state", "pr_page"} {
		v.Del(key)
	}
	v.Set("page", strconv.FormatInt(p.Number, 10))
	v.Set("q", p.Search)
	v.Set("access", p.Access)
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

func (p Page) PRFilters() map[string]string {
	v, _ := url.ParseQuery(p.Query)
	values := make(map[string]string)
	for _, key := range []string{"pr", "pr_q", "pr_state", "pr_page"} {
		if v.Has(key) {
			values[key] = v.Get(key)
		}
	}
	return values
}
