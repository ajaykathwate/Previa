package pullrequests

import "strings"

// Stage is one step of a build request on its way to a live preview.
// State is "done", "current", "working" (one of several steps is running
// and only the live log says which), "failed" or "pending".
type Stage struct {
	Key, Label, Hint, State string
}

// Progress is the build tracker shown while a request is in flight or after
// it failed. Stages always run in StageKeys order.
type Progress struct {
	Stages []Stage
	// Active: work is in flight; the page follows the live log to name the
	// exact step, and Since starts the elapsed-time clock (RFC 3339).
	Active bool
	Since  string
	Failed bool
}

// StageKeys are the tracker's steps, in order.
var StageKeys = []string{"queue", "code", "image", "start", "publish", "live"}

var stageText = map[string][2]string{
	"queue":   {"Queued", "Waiting for a build slot"},
	"code":    {"Get code", "Download the exact commit"},
	"image":   {"Build image", "Run your Dockerfile"},
	"start":   {"Start preview", "Services, setup, readiness"},
	"publish": {"Publish", "Give it its HTTPS address"},
	"live":    {"Live", "Serving reviewers"},
}

// Progress places the current build request along the stages; ok is false
// when no tracker applies (nothing requested, or the preview is simply live).
func (pr PR) Progress() (Progress, bool) {
	if pr.Build == "failed" && pr.FailureCode == "worker_failed" {
		return Progress{Failed: true}, false
	}
	if (pr.Build == "running" || pr.Build == "queued") && (pr.ExecutionRetrying || pr.BuildWorkerMissing) {
		return Progress{}, false
	}
	if pr.PublicationRetrying {
		return Progress{}, false
	}
	switch {
	case pr.Build == "pending" || pr.Build == "queued":
		return track("queue", "current", false), true
	case pr.Build == "running" && pr.Environment == "publishing":
		p := track("publish", "current", false)
		p.Active, p.Since = true, pr.StartedAt
		return p, true
	case pr.Build == "running":
		// Downloading, building and starting all happen inside "running";
		// the live log names the exact one.
		p := track("code", "working", false)
		for i := range p.Stages {
			if p.Stages[i].Key == "image" || p.Stages[i].Key == "start" {
				p.Stages[i].State = "working"
			}
		}
		p.Active, p.Since = true, pr.StartedAt
		return p, true
	case pr.Build == "succeeded" && pr.Environment == "publishing":
		p := track("publish", "current", false)
		p.Active, p.Since = true, pr.StartedAt
		return p, true
	case pr.Build == "failed" && pr.Failure != "":
		stage := pr.Diagnosis().Stage
		if stage == "" {
			stage = "image"
		}
		return track(stage, "failed", true), true
	}
	return Progress{}, false
}

// track marks every stage before at as done and at itself with state.
func track(at, state string, failed bool) Progress {
	p := Progress{Failed: failed}
	reached := false
	for _, key := range StageKeys {
		s := Stage{Key: key, Label: stageText[key][0], Hint: stageText[key][1], State: "pending"}
		switch {
		case key == at:
			s.State, reached = state, true
		case !reached:
			s.State = "done"
		}
		p.Stages = append(p.Stages, s)
	}
	return p
}

// Diagnosis explains this PR's failed build; the zero value when it has not
// failed.
func (pr PR) Diagnosis() Diagnosis {
	if pr.Failure == "" {
		return Diagnosis{}
	}
	return Diagnose(pr.FailureCode, pr.FailureLog)
}

// Diagnosis explains a Builds or Previews row's failure, from its
// category alone (rows carry no output).
func (r ActivityRow) Diagnosis() Diagnosis {
	if r.Failure == "" {
		return Diagnosis{}
	}
	return Diagnose(r.FailureCode, nil)
}

// TrackAt is a tracker stopped at one stage: earlier stages done, this one in
// state ("current", "working" or "failed"), later ones pending.
func TrackAt(stage, state string) Progress {
	return track(stage, state, state == "failed")
}

// LogEntry is one line of build output for stage detection.
type LogEntry struct {
	System bool
	Text   string
}

// DetectStage reads which stage a build has reached from its output: the
// runtime's own progress lines (system lines) mark getting the code,
// starting services, setup and the app, and publishing; any line from the
// image build itself means the image is building. "" when nothing shows yet.
// ui.js applies the same rules to lines that arrive live.
func DetectStage(lines []LogEntry) string {
	reached, output := -1, false
	for _, line := range lines {
		output = output || !line.System
		if s := lineStage(line, reached); s > reached {
			reached = s
		}
	}
	if output && reached < 2 {
		reached = 2 // image output, even when the code step scrolled out of view
	}
	if reached < 0 {
		return ""
	}
	return StageKeys[reached]
}

// lineStage is the stage index one line shows, or -1.
func lineStage(line LogEntry, reached int) int {
	text := strings.TrimSpace(line.Text)
	if !line.System {
		if reached >= 1 && reached < 2 {
			return 2 // build output after the code was prepared
		}
		return -1
	}
	switch {
	case strings.HasPrefix(text, "Preparing commit "), strings.HasPrefix(text, sourceFailedPrefix):
		return 1
	case strings.HasPrefix(text, "The preview is healthy"):
		return 4
	case strings.HasPrefix(text, "Pulling the "), strings.HasPrefix(text, "Waiting up to "),
		strings.HasPrefix(text, "Running the setup command"), strings.HasPrefix(text, "The setup command finished"),
		strings.HasPrefix(text, "Starting the process "), strings.HasPrefix(text, "The image runs as root"),
		strings.Contains(text, "approved outbound connection"), strings.Contains(text, " is ready at "):
		return 3
	}
	return -1
}
