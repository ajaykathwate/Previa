package builds

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/web/components"
	"github.com/ajaykathwatee/previa/web/components/logviewer"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// LogLine is a presentation event shared with the PR log panel.
type LogLine = logviewer.Line

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

	Source, Query string
	ProgressLines []LogLine
	Earlier       bool
	Lines         []LogLine
	Empty         string // shown when Lines is empty
	Before        int64  // first shown sequence; earlier lines exist when > 0
	After         int64  // sequence after the last shown line
	More          bool   // later lines exist beyond this page
	Tail          bool   // the newest lines are shown; live updates continue from here

	// Live updates resume from Epoch/After. Empty EventsURL disables them.
	EventsURL string
	Epoch     int64
	// HasOutput: the attempt has stored lines, so they can be downloaded.
	HasOutput bool
	// CanDelete: the owner may delete the attempt's logs (it is not writing).
	CanDelete bool
}

// DeleteURL deletes the attempt's logs (POST, with Epoch).
func (p LogPage) DeleteURL() string { return pages.LogsURL(p.RepoID, p.Number) + "/delete" }

// DownloadURL is the whole current output as a text file.
func (p LogPage) DownloadURL() string {
	return logviewer.FilterURL(pages.LogsURL(p.RepoID, p.Number)+"/download", p.Source, p.Query)
}

func (p LogPage) PRURL() string   { return pages.PRURL(p.RepoID, p.Number) }
func (p LogPage) RepoURL() string { return pages.RepoURL(p.RepoID) }
func (p LogPage) LatestURL() string {
	return logviewer.FilterURL(pages.LogsURL(p.RepoID, p.Number), p.Source, p.Query)
}

func (p LogPage) EarlierURL() string {
	return pages.LogsURL(p.RepoID, p.Number) + "?" + p.position("before", p.Before)
}

func (p LogPage) LaterURL() string {
	return pages.LogsURL(p.RepoID, p.Number) + "?" + p.position("after", p.After)
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
	lines := p.Lines
	if p.ProgressLines != nil {
		lines = p.ProgressLines
	}
	entries := make([]pages.LogEntry, len(lines))
	for i, l := range lines {
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

func (p LogPage) position(key string, value int64) string {
	v := url.Values{key: {strconv.FormatInt(value, 10)}}
	if p.Source != "" && p.Source != "all" {
		v.Set("source", p.Source)
	}
	if p.Query != "" {
		v.Set("q", p.Query)
	}
	return v.Encode()
}
func (p LogPage) Viewer() logviewer.Panel {
	v := logviewer.Panel{Lines: p.Lines, Empty: p.Empty, Source: p.Source, Query: p.Query,
		BaseURL: pages.LogsURL(p.RepoID, p.Number), EventsURL: p.EventsURL, Cursor: p.Cursor(),
		LatestURL: p.LatestURL(), DownloadURL: p.DownloadURL(), HasOutput: p.HasOutput, CanDelete: p.CanDelete}
	if p.Earlier {
		v.EarlierURL = p.EarlierURL()
	}
	if p.More {
		v.LaterURL = p.LaterURL()
	}
	return v
}

func (p LogPage) PublicReferences() []components.ResourceRef {
	return components.PRRefs(p.RepoID, p.Number)
}
