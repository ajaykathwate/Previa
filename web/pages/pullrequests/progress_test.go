package pullrequests

import (
	"slices"
	"testing"
)

func TestDiagnoseNamesTheCauseAndWhatToDo(t *testing.T) {
	for _, c := range []struct {
		code, line, title, stage string
		retry                    bool
	}{
		{"source_failed", "GitHub denied access to the repository archive; check repository access and App permissions.", "GitHub didn't let Previa download your code", "code", false},
		{"source_failed", "Add .previewrc.yaml to the repository root at the requested commit.", "This commit has no .previewrc.yaml", "code", false},
		{"source_failed", "Installation authentication is unavailable; check the GitHub App configuration.", "Previa couldn't sign in to GitHub as your app", "code", false},
		{"source_failed", "GitHub rate limit reached; retry after it resets.", "GitHub asked Previa to slow down", "code", true},
		{"source_failed", "The archive download ended early.", "Previa couldn't reach GitHub", "code", true},
		{"source_failed", `The Dockerfile "web/Dockerfile" named in .previewrc.yaml does not exist at the requested commit.`, "Previa couldn't find a usable Dockerfile", "code", false},
		{"source_failed", "Configuration must not exceed 64 KiB.", "Your .previewrc.yaml has a problem", "code", false},
		{"source_failed", "The archive does not contain the requested commit.", "This commit isn't available to Previa", "code", false},
		{"source_failed", "GitHub returned an archive location that is not on an allowed host.", "GitHub's download couldn't be used", "code", true},
		{"source_failed", "The archive contains a symlink that points outside the source tree.", "Your repository has files Previa can't safely unpack", "code", false},
		{"source_failed", "The worker could not create a source workspace.", "The build server couldn't prepare a workspace", "code", true},
		{"build_failed", "ERROR: failed to solve: process did not complete successfully", "Your Dockerfile build failed", "image", false},
		{"unhealthy", "", "Your app didn't become ready", "start", false},
		{"capacity", "", "The server had no room for another preview", "start", true},
		{"route_not_converged", "", "The preview couldn't be published", "publish", true},
		{"a_future_category", "", "The build failed", "image", false},
	} {
		var output []string
		switch {
		case c.code == "source_failed":
			output = []string{"Preparing commit 1234567.", sourceFailedPrefix + c.line}
		case c.line != "":
			output = []string{"#7 RUN npm ci", c.line}
		}
		d := Diagnose(c.code, output)
		if d.Title != c.title || d.Stage != c.stage || d.Retry != c.retry || d.Code != c.code {
			t.Errorf("%s %q: got %q at %q (retry %v)", c.code, c.line, d.Title, d.Stage, d.Retry)
		}
		if d.Summary == "" || len(d.Steps) == 0 {
			t.Errorf("%s %q: every diagnosis says what happened and what to do", c.code, c.line)
		}
		if d.Reason != c.line {
			t.Errorf("%s: reason %q, want the recorded line %q", c.code, d.Reason, c.line)
		}
	}
}

func TestDetectStageReadsTheRuntimeProgressLines(t *testing.T) {
	system := func(text string) LogEntry { return LogEntry{System: true, Text: text} }
	output := func(text string) LogEntry { return LogEntry{Text: text} }
	for _, c := range []struct {
		lines []LogEntry
		want  string
	}{
		{nil, ""},
		{[]LogEntry{system("Preparing commit 1234567.")}, "code"},
		{[]LogEntry{system(sourceFailedPrefix + "GitHub denied access.")}, "code"},
		{[]LogEntry{system("Preparing commit 1234567."), output("#1 [internal] load build definition")}, "image"},
		{[]LogEntry{output("#9 exporting layers")}, "image"}, // the code step scrolled out of view
		{[]LogEntry{output("#9 done"), system("Pulling the postgres:17 image.")}, "start"},
		{[]LogEntry{system("Starting the process web."), output("listening on :3000")}, "start"},
		{[]LogEntry{system("Running the setup command."), system("The preview is healthy. It is not routed yet.")}, "publish"},
		{[]LogEntry{system("The preview is healthy. It is not routed yet."), system("Preparing commit 1234567.")}, "publish"}, // never moves back
	} {
		if got := DetectStage(c.lines); got != c.want {
			t.Errorf("%v: stage %q, want %q", c.lines, got, c.want)
		}
	}
}

func TestProgressPlacesTheBuildRequest(t *testing.T) {
	states := func(p Progress) (out []string) {
		for _, s := range p.Stages {
			out = append(out, s.State)
		}
		return out
	}
	for _, c := range []struct {
		name   string
		pr     PR
		want   []string
		active bool
	}{
		{"queued", PR{Build: "queued"}, []string{"current", "pending", "pending", "pending", "pending", "pending"}, false},
		{"running", PR{Build: "running", StartedAt: "2026-10-10T10:00:00Z"}, []string{"done", "working", "working", "working", "pending", "pending"}, true},
		{"publishing", PR{Build: "running", Environment: "publishing"}, []string{"done", "done", "done", "done", "current", "pending"}, true},
		{"failed to start", PR{Build: "failed", Failure: "The app did not become ready.", FailureCode: "unhealthy"}, []string{"done", "done", "done", "failed", "pending", "pending"}, false},
		{"failed to download", PR{Build: "failed", Failure: "The commit could not be prepared.", FailureCode: "source_failed"}, []string{"done", "failed", "pending", "pending", "pending", "pending"}, false},
	} {
		p, ok := c.pr.Progress()
		if !ok || p.Active != c.active || len(p.Stages) != len(StageKeys) {
			t.Fatalf("%s: tracked %v, active %v, %d stages", c.name, ok, p.Active, len(p.Stages))
		}
		if got := states(p); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		if c.active && p.Since != c.pr.StartedAt {
			t.Errorf("%s: clock starts at %q", c.name, p.Since)
		}
	}
	for _, pr := range []PR{{Build: "Not requested"}, {Build: "succeeded", Environment: "running"}, {Build: "failed"}} {
		if _, ok := pr.Progress(); ok {
			t.Errorf("%+v: no tracker when nothing is in flight or explained", pr)
		}
	}
}

func TestWorkerFailuresDoNotInventAnImageFailureOrKeepSpinning(t *testing.T) {
	for _, pr := range []PR{{Build: "failed", FailureCode: "worker_failed", Failure: "Worker failed"}, {Build: "running", ExecutionRetrying: true}, {Build: "running", BuildWorkerMissing: true}, {Build: "succeeded", Environment: "publishing", PublicationRetrying: true}} {
		progress, tracked := pr.Progress()
		if tracked || progress.Active {
			t.Fatal("interrupted execution still shows active stage progress")
		}
	}
	diagnosis := Diagnose("worker_failed", nil)
	if diagnosis.Title != "The build worker could not finish" || diagnosis.Stage != "" {
		t.Fatal("worker finalization failure was blamed on repository code")
	}
}
