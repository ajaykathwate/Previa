package builds

import (
	"net/url"
	"strconv"
	"strings"

	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// LogLine is one stored line of the current build attempt.
type LogLine struct {
	Sequence int64
	System   bool
	Text     string
}

// Number is the 1-based line number shown beside the text.
func (l LogLine) Number() string { return strconv.FormatInt(l.Sequence+1, 10) }

// Anchor is the fragment that links to this line.
func (l LogLine) Anchor() string { return "L" + l.Number() }

// LogNotice is a status message; Tone is one of the shared tone classes.
type LogNotice struct {
	Tone string `json:"tone"`
	Text string `json:"text"`
}

// LogPage is the current-build log page of one pull request. Handlers fill it
// with display-ready values; the template adds no state of its own.
type LogPage struct {
	RepoID     int64
	Repository string
	Number     int32
	Title      string

	StatusLabel, StatusTone string
	Summary                 string
	StreamSHA               string
	Notices                 []LogNotice

	Lines  []LogLine
	Empty  string // shown when Lines is empty
	Before int64  // first shown sequence; earlier lines exist when > 0
	After  int64  // sequence after the last shown line
	More   bool   // later lines exist beyond this page
	Tail   bool   // the newest lines are shown; live updates continue from here

	// Live updates resume from Epoch/After. Empty EventsURL disables them.
	EventsURL string
	Epoch     int64
}

func (p LogPage) PRURL() string   { return pages.PRURL(p.RepoID, p.Number) }
func (p LogPage) RepoURL() string { return pages.RepoURL(p.RepoID) }
func (p LogPage) LatestURL() string {
	return pages.LogsURL(p.RepoID, p.Number)
}

func (p LogPage) EarlierURL() string {
	return pages.LogsURL(p.RepoID, p.Number) + "?" + url.Values{"before": {strconv.FormatInt(p.Before, 10)}}.Encode()
}

func (p LogPage) LaterURL() string {
	return pages.LogsURL(p.RepoID, p.Number) + "?" + url.Values{"after": {strconv.FormatInt(p.After, 10)}}.Encode()
}

// Cursor is where live updates resume: the attempt epoch and next sequence.
func (p LogPage) Cursor() string {
	return strconv.FormatInt(p.Epoch, 10) + "." + strconv.FormatInt(p.After, 10)
}

// Owner and Name split "owner/repository" for the breadcrumb.
func (p LogPage) Owner() string {
	owner, _, _ := strings.Cut(p.Repository, "/")
	return owner
}

func (p LogPage) Name() string {
	_, name, found := strings.Cut(p.Repository, "/")
	if !found {
		return p.Repository
	}
	return name
}
