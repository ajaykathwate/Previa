package builds

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/web/components"
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
	// HasOutput: the attempt has a stored stream, so it can be downloaded.
	HasOutput bool
}

// DownloadURL is the whole current output as a text file.
func (p LogPage) DownloadURL() string { return pages.LogsURL(p.RepoID, p.Number) + "/download" }

// IsError marks a line worth the reader's eye; only its colour changes.
func (l LogLine) IsError() bool { return !l.System && pages.LooksLikeError(l.Text) }

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

// Progress places the attempt on the build tracker from its status and the
// output shown: the step the output has reached, failed or in progress.
// ok is false when no attempt applies (none requested, cancelled, replaced).
func (p LogPage) Progress() (pages.Progress, bool) {
	entries := make([]pages.LogEntry, len(p.Lines))
	for i, l := range p.Lines {
		entries[i] = pages.LogEntry{System: l.System, Text: l.Text}
	}
	reached := pages.DetectStage(entries)
	switch p.StatusLabel {
	case pages.Label("pending"), pages.Label("queued"):
		return pages.TrackAt("queue", "current"), true
	case pages.Label("running"):
		if reached == "" {
			reached = "code"
		}
		progress := pages.TrackAt(reached, "current")
		progress.Active = true
		return progress, true
	case pages.Label("failed"):
		if reached == "" {
			reached = "code"
		}
		return pages.TrackAt(reached, "failed"), true
	case pages.Label("succeeded"):
		return pages.TrackAt("live", "done"), true
	}
	return pages.Progress{}, false
}

// TrailParent is the pull request this log belongs to, one level up.
func (p LogPage) TrailParent() components.Crumb {
	return components.Crumb{Label: "#" + strconv.Itoa(int(p.Number)) + " " + p.Title, Href: p.PRURL()}
}
