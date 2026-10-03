package pullrequests

import (
	"fmt"
	"net/url"
	"strconv"
)

type PR struct {
	Number                                                                                          int32
	Title, Author, Branch, SHA, State, Build, URL, RequestedSHA, DeployedSHA, Environment, Deadline string
	Available, Fork, NeedsRefresh                                                                   bool
}
type Page struct {
	RepoID                                          int64
	Name, Mode, SyncState, SyncMessage, LastRefresh string
	Fetched                                         int32
	Available, DiscoveryEnabled                     bool
	Rows                                            []PR
	Number, Pages, Total                            int64
	Search, State                                   string
}

func RepoURL(id int64) string { return fmt.Sprintf("/repositories/%d", id) }
func PRURL(id int64, number int32) string {
	return fmt.Sprintf("%s/pull-requests/%d", RepoURL(id), number)
}
func (p Page) PageURL(n int64) string {
	v := url.Values{"page": {strconv.FormatInt(n, 10)}, "state": {p.State}, "q": {p.Search}}
	return RepoURL(p.RepoID) + "?" + v.Encode()
}
func Short(s string) string {
	if s == "" {
		return "—"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

type ActivityRow struct {
	RepoID                                             int64
	Number                                             int32
	Name, Latest, Requested, Deployed, State, Deadline string
	Available                                          bool
}
type Activity struct {
	Rows                         []ActivityRow
	Total, Number, Pages, RepoID int64
	Environments                 bool
}

func (a Activity) PageURL(n int64) string {
	path := "/builds"
	if a.Environments {
		path = "/environments"
	}
	v := url.Values{"page": {strconv.FormatInt(n, 10)}}
	if a.RepoID > 0 {
		v.Set("repo_id", strconv.FormatInt(a.RepoID, 10))
	}
	return path + "?" + v.Encode()
}
