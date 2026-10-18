// Package sampledata renders a repository's Test data tab and its baseline
// refresh logs (docs/32-persistent-test-data.md). Handlers pass display data;
// nothing here reads storage.
package sampledata

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/web/components"
	"github.com/ajaykathwatee/previa/web/components/logviewer"
)

// Page is the Test data tab of one repository.
type Page struct {
	RepoID   int64
	Name     string // owner/name
	CSRF     string
	LiveURL  string
	Toast    string
	Notice   Notice
	Manager  bool // Owner/Admin: settings and Delete baseline
	Operator bool // may refresh the baseline
	// Available: Previa still has GitHub access to the repository.
	Available bool
	// Databases: the workers offer PostgreSQL or MySQL. Baselines: a worker
	// stores baselines.
	Databases, Baselines bool

	Settings  Settings
	Error     Field // a rejected settings value, shown beside its field
	Current   []Baseline
	Moved     string // the branch moved past the baseline, said once
	Refreshes []Refresh
}

// Notice explains something about the whole tab.
type Notice struct{ Tone, Text string }

// Field is a settings value the server rejected.
type Field struct{ Name, Message, Value string }

// Settings are the repository's settings as the forms submit them.
type Settings struct {
	Enabled, KeepData, UseBaseline, AutoRefresh bool
	Branch                                      string // "" follows the default branch
	DefaultBranch                               string // what the default branch was when last looked at
	Revision                                    int64
	UpdatedAt                                   string // RFC 3339; empty before the first save
}

// BranchKnown reports whether BranchLabel names a branch, rather than
// describing the default one.
func (s Settings) BranchKnown() bool { return s.Branch != "" || s.DefaultBranch != "" }

// BranchLabel names the branch baselines come from.
func (s Settings) BranchLabel() string {
	switch {
	case s.Branch != "":
		return s.Branch
	case s.DefaultBranch != "":
		return s.DefaultBranch + " (default branch)"
	}
	return "the default branch"
}

// BranchPlaceholder is the inline branch field's hint.
func (s Settings) BranchPlaceholder() string {
	if s.DefaultBranch != "" {
		return "Default branch (" + s.DefaultBranch + ")"
	}
	return "Default branch"
}

// Baseline is one current baseline.
type Baseline struct {
	Service        string // "PostgreSQL 17"
	Branch, Commit string
	BuiltAt        string // RFC 3339
	Size           string
}

// Refresh is one baseline refresh.
type Refresh struct {
	Number         int64
	Trigger        string
	Label, Tone    string
	Branch, Commit string
	When           string // RFC 3339 of its latest step
	WhenLabel      string // "Finished", "Started", "Requested", "Starts"
	Took           string
	Detail         string // why it failed or waits; safe to show
	HasLog         bool
}

// URL is the Test data tab.
func URL(repoID int64) string { return "/repositories/" + strconv.FormatInt(repoID, 10) + "/test-data" }

// LogURL is a refresh's build log.
func LogURL(repoID, number int64) string {
	return URL(repoID) + "/refreshes/" + strconv.FormatInt(number, 10) + "/logs"
}

// Repository is the repository's name without its owner.
func (p Page) Repository() string {
	if _, name, ok := strings.Cut(p.Name, "/"); ok {
		return name
	}
	return p.Name
}

// Owner is the repository's owner.
func (p Page) Owner() string {
	owner, _, _ := strings.Cut(p.Name, "/")
	return owner
}

// PublicReferences lets public addresses be prepared before rendering.
func (p Page) PublicReferences() []components.ResourceRef {
	return components.ResourceRefs("repository", p.RepoID)
}

// UsesBaseline: Test data with a repository baseline.
func (p Page) UsesBaseline() bool { return p.Settings.Enabled && p.Settings.UseBaseline }

// CanRefresh: Refresh baseline applies now.
func (p Page) CanRefresh() bool { return p.Operator && p.UsesBaseline() && p.Available && p.Databases }

// Busy is the newest refresh while it waits or runs.
func (p Page) Busy() *Refresh {
	if len(p.Refreshes) > 0 && (p.Refreshes[0].Label == LabelWaiting || p.Refreshes[0].Label == LabelBuilding) {
		return &p.Refreshes[0]
	}
	return nil
}

// Status is the baseline card's badge.
func (p Page) Status() (label, tone string) {
	switch {
	case !p.Settings.Enabled:
		return "Off", "tone-neutral"
	case !p.Settings.UseBaseline:
		return "Not used", "tone-neutral"
	case p.Busy() != nil:
		return p.Busy().Label, p.Busy().Tone
	case len(p.Current) > 0:
		return "Ready", "tone-ok"
	case len(p.Refreshes) > 0 && p.Refreshes[0].Label == LabelFailed:
		return "Failed", "tone-danger"
	}
	return "None yet", "tone-neutral"
}

// Refresh labels.
const (
	LabelWaiting   = "Waiting"
	LabelBuilding  = "Building"
	LabelReady     = "Ready"
	LabelFailed    = "Failed"
	LabelCancelled = "Cancelled"
)

// TriggerLabel says what started a refresh.
func TriggerLabel(trigger string) string {
	switch trigger {
	case "enabled":
		return "Test data turned on"
	case "merged":
		return "Pull request merged"
	case "button":
		return "Refresh baseline"
	case "daily":
		return "Daily check"
	case "unusable":
		return "Baseline unusable"
	case "branch":
		return "Branch changed"
	}
	return "Refresh"
}

// FailureTitle names a refresh failure category.
func FailureTitle(code string) string {
	switch code {
	case "branch_not_found":
		return "Branch not found"
	case "config_invalid":
		return "Configuration needs a change"
	case "build_failed":
		return "Image build failed"
	case "service_failed":
		return "A database did not start"
	case "setup_failed":
		return "Setup failed"
	case "seed_failed":
		return "Seed failed"
	case "capture_failed":
		return "Capture failed"
	case "capture_too_large":
		return "Baseline too large"
	case "unavailable":
		return "Could not run"
	}
	return "Failed"
}

// WaitingText explains why a refresh waits.
func WaitingText(reason string, until time.Time, attempts int32, now time.Time) string {
	switch {
	case reason == "workspace_accounting":
		return "Waiting for usage accounting. The previous baseline stays current; this refresh retries automatically."
	case reason == "workspace_builds":
		return "Waiting for a workspace build slot. This refresh retries when another build finishes."
	case reason == "workspace_minutes":
		return "Waiting for workspace build minutes. See Usage for the allowance reset; the previous baseline stays current."
	case reason == "workspace_daily":
		return "The operator's daily safety ceiling is reached. This refresh retries at 00:00 UTC."
	case reason == "workspace_restricted":
		return "Workspace compute is restricted. Ask the workspace owner to contact the operator."
	case reason == "capacity_wait":
		return "Waiting for a free build slot."
	case reason == "quota_blocked":
		return "Waiting: the workspace used today's build minutes. It resumes at 00:00 UTC."
	case attempts > 0 && until.After(now):
		return "Waiting to try again: GitHub or the worker was unavailable."
	case until.After(now.Add(5 * time.Second)):
		return "Scheduled: automatic refreshes of a repository are spaced apart."
	}
	return "Waiting for a worker."
}

// Size formats a stored size.
func Size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// Took formats a duration in whole seconds.
func Took(seconds int32) string {
	if seconds <= 0 {
		return ""
	}
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return strconv.Itoa(int(seconds)) + "s"
	}
	return strconv.Itoa(int(d/time.Minute)) + "m " + strconv.Itoa(int(d%time.Minute/time.Second)) + "s"
}

// Short is a commit's short form.
func Short(sha string) string { return sha[:min(7, len(sha))] }

// ServiceTitle names a managed SQL service.
func ServiceTitle(kind, version string) string {
	switch kind {
	case "postgres":
		return "PostgreSQL " + version
	case "mysql":
		return "MySQL " + version
	}
	return kind + " " + version
}

// LogPage is one refresh's build log.
type LogPage struct {
	RepoID         int64
	Name           string
	Number         int64
	Label, Tone    string
	Summary        string
	Branch, Commit string
	Notices        []string
	Panel          logviewer.Panel
}

// PublicReferences lets public addresses be prepared before rendering.
func (p LogPage) PublicReferences() []components.ResourceRef {
	return components.ResourceRefs("repository", p.RepoID)
}

// Repository is the repository's name without its owner.
func (p LogPage) Repository() string {
	if _, name, ok := strings.Cut(p.Name, "/"); ok {
		return name
	}
	return p.Name
}
