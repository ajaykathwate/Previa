// Package appoutput renders a running preview's live app output.
package appoutput

import (
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/web/components"
	buildpages "github.com/ajaykathwatee/previa/web/pages/builds"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// OutputPage is a running preview's app output. Handlers fill it with
// display-ready values; ui.js streams later lines into it.
type OutputPage struct {
	RepoID     int64
	Repository string
	Number     int32
	Title      string

	StatusLabel, StatusTone string
	Summary                 string
	DeployedSHA             string
	Notices                 []buildpages.LogNotice

	Lines []buildpages.LogLine
	Empty string // shown when Lines is empty
	// Live updates resume from Epoch/After. Empty EventsURL disables them.
	EventsURL string
	Epoch     int64
	After     int64
}

func (p OutputPage) PRURL() string     { return pages.PRURL(p.RepoID, p.Number) }
func (p OutputPage) LogsURL() string   { return pages.LogsURL(p.RepoID, p.Number) }
func (p OutputPage) OutputURL() string { return pages.OutputURL(p.RepoID, p.Number) }

// Cursor is where live updates resume: the follower's epoch and next line.
func (p OutputPage) Cursor() string {
	return strconv.FormatInt(p.Epoch, 10) + "." + strconv.FormatInt(p.After, 10)
}

func (p OutputPage) Name() string {
	_, name, found := strings.Cut(p.Repository, "/")
	if !found {
		return p.Repository
	}
	return name
}

func (p OutputPage) TrailParent() components.Crumb {
	return components.Crumb{Label: "#" + strconv.Itoa(int(p.Number)) + " " + p.Title, Href: p.PRURL()}
}

func (p OutputPage) PublicReferences() []components.ResourceRef {
	return components.PRRefs(p.RepoID, p.Number)
}
