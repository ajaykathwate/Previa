package pullrequests

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	repositorypages "github.com/ajaykathwatee/previa/web/pages/repositories"
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
	WorkspaceQuery                                  string
	SelectedPR                                      *PR
}

func RepoURL(id int64) string { return fmt.Sprintf("/repositories/%d", id) }
func PRURL(id int64, number int32) string {
	return repositorypages.WorkspaceURL(id, url.Values{"pr": {strconv.Itoa(int(number))}})
}
func (p Page) PageURL(n int64) string {
	v, _ := url.ParseQuery(p.WorkspaceQuery)
	v.Set("pr_page", strconv.FormatInt(n, 10))
	v.Set("pr_state", p.State)
	v.Set("pr_q", p.Search)
	v.Del("pr")
	return repositorypages.WorkspaceURL(p.RepoID, v)
}

func (p Page) DetailURL(n int32) string {
	location, _ := url.Parse(p.PageURL(p.Number))
	v := location.Query()
	v.Set("pr", strconv.Itoa(int(n)))
	return repositorypages.WorkspaceURL(p.RepoID, v)
}

func (p Page) CurrentURL() string {
	if p.SelectedPR != nil {
		return p.DetailURL(p.SelectedPR.Number)
	}
	return p.PageURL(p.Number)
}

func (p Page) Query() string {
	_, query, _ := strings.Cut(p.CurrentURL(), "?")
	v, _ := url.ParseQuery(query)
	if p.SelectedPR != nil {
		v.Set("pr", strconv.Itoa(int(p.SelectedPR.Number)))
	}
	return v.Encode()
}

func Label(value string) string {
	switch value {
	case "open":
		return "Open"
	case "closed":
		return "Closed"
	case "merged":
		return "Merged"
	case "all":
		return "All"
	case "manual":
		return "Manual"
	case "automatic":
		return "Automatic"
	case "pending":
		return "Waiting"
	case "queued":
		return "Queued"
	case "running":
		return "Running"
	case "succeeded":
		return "Succeeded"
	case "failed":
		return "Failed"
	case "cancelled":
		return "Cancelled"
	case "superseded":
		return "Superseded"
	case "torn_down":
		return "Removed"
	case "tearing_down":
		return "Removing"
	case "publishing":
		return "Publishing"
	default:
		return value
	}
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

func (p Page) DirectoryFilters() map[string]string {
	v, _ := url.ParseQuery(p.WorkspaceQuery)
	values := make(map[string]string)
	for _, key := range []string{"q", "access", "page"} {
		if v.Has(key) {
			values[key] = v.Get(key)
		}
	}
	return values
}
func StateClass(state string) string {
	if state == "open" {
		return "available"
	}
	return "unavailable"
}
func TimeLabel(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format("2 Jan 2006, 15:04 UTC")
}

func (p Page) Selected() PR { return *p.SelectedPR }

func (p Page) Owner() string { owner, _, _ := strings.Cut(p.Name, "/"); return owner }
func (p Page) RepositoryName() string {
	_, name, found := strings.Cut(p.Name, "/")
	if !found {
		return p.Name
	}
	return name
}
func (p Page) CloseURL() string {
	v := url.Values{}
	for key, value := range p.DirectoryFilters() {
		v.Set(key, value)
	}
	return repositorypages.WorkspaceURL(0, v)
}
func (p Page) PolicyMode(modal bool) string {
	if !modal && p.Mode == "automatic" {
		return "manual"
	}
	return "automatic"
}
func (p Page) PolicyAction(modal bool) string {
	if p.PolicyMode(modal) == "manual" {
		return "Turn off automatic"
	}
	return "Enable automatic"
}
