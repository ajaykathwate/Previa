package buildlogs

import (
	"fmt"
	"time"

	buildpages "github.com/ajaykathwatee/previa/web/pages/builds"
	pages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

// status is what the page and the live stream say about the current attempt.
type status struct {
	Label   string                 `json:"label"`
	Tone    string                 `json:"tone"`
	Summary string                 `json:"summary"`
	Notices []buildpages.LogNotice `json:"notices"`
	SHA     string                 `json:"sha"`
	Empty   string                 `json:"empty"`
	Epoch   int64                  `json:"epoch"`
}

// describe states only what is recorded: the current intent and the stored
// stream. A stream from an earlier attempt is labelled as such, never shown as
// the current build's output. retention is how long finished logs are kept.
func describe(t Target, s Stream, hasStream bool, retention time.Duration) status {
	st := status{Label: "No build requested", Tone: pages.Tone(""), Summary: "No build has been requested for this pull request."}
	if hasStream {
		st.SHA, st.Epoch = s.HeadSHA, s.Epoch
	}
	current := hasStream && t.Intent != nil && s.Generation == t.Intent.Generation && s.Revision == t.Intent.Revision
	if i := t.Intent; i != nil {
		st.Label, st.Tone = pages.Label(i.State), pages.Tone(i.State)
		if summary := intentSummary(i, current, s.Finished); summary != "" {
			st.Summary = summary
		}
		if !current && hasStream {
			st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-info", Text: "These logs are from an earlier attempt (commit " + pages.Short(s.HeadSHA) + "). The current request has not produced output yet."})
		}
	}
	if !t.Available {
		st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-warn", Text: "GitHub access to this repository was removed. Stored logs stay readable until they expire."})
	}
	st.Empty = emptyText(s, hasStream)
	switch {
	case hasStream && !s.DeletedAt.IsZero():
		st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-neutral", Text: "You deleted this attempt's logs on " + s.DeletedAt.UTC().Format("2 Jan 2006, 15:04 UTC") + "."})
	case hasStream && !s.PrunedAt.IsZero():
		st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-neutral", Text: fmt.Sprintf("Logs are kept for %d days after a build finishes; this attempt's logs were deleted.", int(retention/(24*time.Hour)))})
	}
	if hasStream && s.Truncated {
		st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-warn", Text: fmt.Sprintf("Logs truncated: the size or line limit was reached, and %d later lines were not stored.", s.OmittedLines)})
	}
	if hasStream && s.LostLines > 0 {
		st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-warn", Text: fmt.Sprintf("%d lines could not be stored because log storage was unavailable.", s.LostLines)})
	}
	if st.Notices == nil {
		st.Notices = []buildpages.LogNotice{}
	}
	return st
}

// intentSummary says what the current request is doing; "" for a state it
// does not know. current is whether the stored stream is the request's own.
func intentSummary(i *Intent, current, finished bool) string {
	switch i.State {
	case "pending":
		if i.AvailableAt.IsZero() {
			return "The automatic build is waiting for five quiet minutes after the latest push."
		}
		return "The automatic build is waiting for five quiet minutes; it becomes eligible after " + i.AvailableAt.UTC().Format("2 Jan 2006, 15:04 UTC") + "."
	case "queued":
		return "The build is queued and has not started yet."
	case "running":
		switch {
		case current && !finished:
			return "The build is running. New output appears here as it arrives."
		case current:
			return "Build output is complete; the preview is being published."
		default:
			return "The build is starting."
		}
	case "succeeded":
		return "The build succeeded."
	case "failed":
		return "The build failed. The output below shows where it stopped."
	case "cancelled":
		return "The build was cancelled."
	case "superseded":
		return "A newer commit or request replaced this build."
	default:
		return ""
	}
}

// emptyText is what the log shows instead of lines, if it has none to show.
func emptyText(s Stream, hasStream bool) string {
	switch {
	case !hasStream:
		return "No build output is stored for this pull request."
	case !s.DeletedAt.IsZero():
		return "Logs deleted."
	case !s.PrunedAt.IsZero():
		return "These logs expired."
	case s.Lines == 0 && !s.Finished:
		return "Waiting for output…"
	case s.Lines == 0:
		return "This attempt produced no output."
	default:
		return ""
	}
}

// deletable reports whether the owner may delete the stored logs: they
// exist, and the attempt finished or can no longer write (it is not the
// PR's running request). Store.Delete checks the same under a lock.
func deletable(t Target, s Stream, hasStream bool) bool {
	if !hasStream || !s.PrunedAt.IsZero() || s.Lines == 0 {
		return false
	}
	if s.Finished {
		return true
	}
	i := t.Intent
	running := i != nil && (i.State == "pending" || i.State == "queued" || i.State == "running")
	return !running || i.Generation != s.Generation || i.Revision != s.Revision
}
