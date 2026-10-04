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
// the current build's output.
func describe(t Target, s Stream, hasStream bool) status {
	st := status{Label: "No build requested", Tone: pages.Tone(""), Summary: "No build has been requested for this pull request."}
	if hasStream {
		st.SHA, st.Epoch = s.HeadSHA, s.Epoch
	}
	current := hasStream && t.Intent != nil && s.Generation == t.Intent.Generation && s.Revision == t.Intent.Revision
	if i := t.Intent; i != nil {
		st.Label, st.Tone = pages.Label(i.State), pages.Tone(i.State)
		switch i.State {
		case "pending":
			st.Summary = "The automatic build is waiting for five quiet minutes after the latest push."
			if !i.AvailableAt.IsZero() {
				st.Summary = "The automatic build is waiting for five quiet minutes; it becomes eligible after " + i.AvailableAt.UTC().Format("2 Jan 2006, 15:04 UTC") + "."
			}
		case "queued":
			st.Summary = "The build is queued and has not started yet."
		case "running":
			switch {
			case current && !s.Finished:
				st.Summary = "The build is running. New output appears here as it arrives."
			case current:
				st.Summary = "Build output is complete; the preview is being published."
			default:
				st.Summary = "The build is starting."
			}
		case "succeeded":
			st.Summary = "The build succeeded."
		case "failed":
			st.Summary = "The build failed. The output below shows where it stopped."
		case "cancelled":
			st.Summary = "The build was cancelled."
		case "superseded":
			st.Summary = "A newer commit or request replaced this build."
		}
		if !current && hasStream {
			st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-info", Text: "These logs are from an earlier attempt (commit " + pages.Short(s.HeadSHA) + "). The current request has not produced output yet."})
		}
	}
	if !t.Available {
		st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-warn", Text: "GitHub access to this repository was removed. Stored logs stay readable until they expire."})
	}
	switch {
	case !hasStream:
		st.Empty = "No build output is stored for this pull request."
	case !s.PrunedAt.IsZero():
		st.Empty = "These logs expired."
		st.Notices = append(st.Notices, buildpages.LogNotice{Tone: "tone-neutral", Text: fmt.Sprintf("Logs are kept for %d days after a build finishes; this attempt's logs were deleted.", int(Retention/(24*time.Hour)))})
	case s.Lines == 0 && !s.Finished:
		st.Empty = "Waiting for output…"
	case s.Lines == 0:
		st.Empty = "This attempt produced no output."
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
