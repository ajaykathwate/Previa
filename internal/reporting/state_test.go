package reporting

import (
	"strings"
	"testing"
	"time"
)

var (
	now      = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	settings = Settings{AppBaseURL: "https://previa.example.com", PreviewBaseDomain: "preview.example.com", CheckName: "Previa preview"}
	headA    = strings.Repeat("a", 40)
	headB    = strings.Repeat("b", 40)
)

func open(intent *Intent, env *Environment) Snapshot {
	return Snapshot{PullRequestID: 9, RepoID: 3, Number: 42, HeadSHA: headB, State: "open", Generation: 5, BuildStatus: "open", Intent: intent, Environment: env}
}

func intent(state string, revision int64) *Intent {
	return &Intent{Generation: 5, Revision: revision, HeadSHA: headB, State: state, AvailableAt: now.Add(-time.Minute)}
}

func TestDerivePhasesAndChecks(t *testing.T) {
	running := &Environment{Status: "running", Hostname: "pr-42-x-abc", DeployedSHA: headB, AppliedGeneration: 5, AppliedRevision: 2}
	oldPreview := &Environment{Status: "failed", Hostname: "pr-42-x-abc", DeployedSHA: headA, AppliedGeneration: 5, AppliedRevision: 2, LastErrorCode: "unhealthy"}
	waiting := intent("pending", 2)
	waiting.AvailableAt = now.Add(3 * time.Minute)
	cases := []struct {
		name               string
		snapshot           Snapshot
		phase              Phase
		status, conclusion string // "" status: no check
		preview            bool
	}{
		{"waiting", open(waiting, nil), PhaseWaiting, "queued", "", false},
		{"queued", open(intent("queued", 2), nil), PhaseQueued, "queued", "", false},
		{"building", open(intent("running", 2), nil), PhaseBuilding, "in_progress", "", false},
		{"retrying after a failed attempt", open(intent("running", 2), &Environment{Status: "failed", Hostname: "h", AppliedGeneration: 5, AppliedRevision: 2, LastErrorCode: "build_failed"}), PhaseBuilding, "in_progress", "", false},
		{"built, not yet published", open(intent("succeeded", 2), &Environment{Status: "publishing", Hostname: "h"}), PhaseBuilding, "in_progress", "", false},
		{"ready", open(intent("succeeded", 2), running), PhaseReady, "completed", "success", true},
		{"activation failed", open(intent("succeeded", 2), &Environment{Status: "failed", Hostname: "h", AppliedGeneration: 5, AppliedRevision: 2, LastErrorCode: "crashed"}), PhaseFailed, "completed", "failure", false},
		{"failed, previous preview serving", open(intent("failed", 2), oldPreview), PhaseFailed, "completed", "failure", true},
		{"nothing requested for the latest commit, older preview live", open(&Intent{Generation: 4, Revision: 1, HeadSHA: headA, State: "succeeded"}, &Environment{Status: "running", Hostname: "h", DeployedSHA: headA}), PhaseReady, "", "", true},
		{"nothing at all", open(intent("superseded", 2), nil), PhaseNone, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Derive(c.snapshot, settings, false, now)
			if v.Phase != c.phase {
				t.Fatalf("phase %s, want %s", v.Phase, c.phase)
			}
			if c.status == "" {
				if v.Check != nil {
					t.Fatalf("unexpected check %+v", v.Check)
				}
			} else if v.Check == nil || v.Check.Status != c.status || v.Check.Conclusion != c.conclusion || v.Check.HeadSHA != headB || v.Check.Revision != 2 {
				t.Fatalf("check %+v", v.Check)
			}
			if (v.PreviewURL != "") != c.preview {
				t.Fatalf("preview URL %q", v.PreviewURL)
			}
		})
	}
}

func TestDeriveFailureAndNotices(t *testing.T) {
	old := &Environment{Status: "failed", Hostname: "pr-42-x-abc", DeployedSHA: headA, AppliedGeneration: 5, AppliedRevision: 2, LastErrorCode: "unhealthy"}
	s := open(intent("failed", 2), old)
	s.Fork, s.SecretCount = true, 2
	v := Derive(s, settings, true, now)
	if v.Failure != "The app did not become ready." || v.Deployed != headA || v.PreviewURL != "https://pr-42-x-abc.preview.example.com" {
		t.Fatalf("view %+v", v)
	}
	if len(v.Notices) != 2 || !strings.Contains(v.Notices[0], "forks") || !strings.Contains(v.Notices[1], "keeps serving") {
		t.Fatalf("notices %v", v.Notices)
	}
	if v.LogsURL != "https://previa.example.com/repositories/3/pull-requests/42/logs" || v.Check.DetailsURL != v.LogsURL {
		t.Fatalf("logs %q %q", v.LogsURL, v.Check.DetailsURL)
	}
	// A failed rebuild of the same commit leaves the earlier preview serving too.
	same := open(intent("failed", 3), &Environment{Status: "failed", Hostname: "pr-42-x-abc", DeployedSHA: headB, AppliedGeneration: 5, AppliedRevision: 2})
	if v := Derive(same, settings, true, now); v.Phase != PhaseFailed || v.PreviewURL == "" || len(v.Notices) != 1 || !strings.Contains(v.Notices[0], "keeps serving") {
		t.Fatalf("same-commit rebuild %+v", v)
	}
	// A failure category never leaks raw text, and unknown codes are generic.
	if FailureMessage("panic: secret value") != "The preview could not be built." {
		t.Fatal("unknown failure category passed through")
	}
}

func TestDeriveBlockedAndClosed(t *testing.T) {
	s := open(intent("queued", 2), nil)
	s.BuildStatus = "conflicts"
	if v := Derive(s, settings, false, now); v.Phase != PhaseBlocked || v.Check.Status != "completed" || v.Check.Conclusion != "neutral" {
		t.Fatalf("conflicts %+v %+v", v, v.Check)
	}
	s.BuildStatus = "checking"
	if v := Derive(s, settings, false, now); v.Phase != PhaseBlocked || v.Check.Status != "queued" {
		t.Fatalf("checking %+v", v)
	}
	live := &Environment{Status: "tearing_down", Hostname: "h", DeployedSHA: headB}
	closed := open(intent("superseded", 2), live)
	closed.State = "closed"
	if v := Derive(closed, settings, true, now); v.Phase != PhaseRemoving || v.Check != nil || v.PreviewURL != "" {
		t.Fatalf("closing %+v", v)
	}
	closed.Environment = &Environment{Status: "torn_down", Hostname: "h"}
	if v := Derive(closed, settings, true, now); v.Phase != PhaseRemoved || !v.Comment || v.LogsURL == "" {
		t.Fatalf("closed %+v", v)
	}
	// A closed PR that never had preview work gets no comment.
	untouched := Snapshot{HeadSHA: headB, State: "closed", Generation: 2}
	if v := Derive(untouched, settings, false, now); v.Comment {
		t.Fatal("comment for a PR without preview work")
	}
	expired := open(intent("superseded", 2), &Environment{Status: "torn_down", Hostname: "h"})
	expired.TeardownRequested, expired.TeardownReason = true, "expired"
	if v := Derive(expired, settings, true, now); v.Phase != PhaseExpired {
		t.Fatalf("expired %+v", v)
	}
}

func TestDeriveWithoutLinksConfigured(t *testing.T) {
	v := Derive(open(intent("succeeded", 2), &Environment{Status: "running", Hostname: "h", DeployedSHA: headB, AppliedGeneration: 5, AppliedRevision: 2}), Settings{CheckName: "x"}, false, now)
	if v.PreviewURL != "" || v.LogsURL != "" || v.Phase != PhaseReady {
		t.Fatalf("view %+v", v)
	}
}

func TestCommentRendering(t *testing.T) {
	v := Derive(open(intent("succeeded", 2), &Environment{Status: "running", Hostname: "pr-42-x-abc", DeployedSHA: headB, AppliedGeneration: 5, AppliedRevision: 2}), settings, false, now)
	body := CommentBody(v, 7)
	if !strings.HasPrefix(body, Marker+"\n") || CommentVersion(body) != 7 {
		t.Fatalf("marker or version missing: %q", body)
	}
	for _, want := range []string{"Previa preview: Ready", "https://pr-42-x-abc.preview.example.com", "`bbbbbbb`", "Build log"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	// The version is not part of the content, so an unchanged state costs no write.
	if Digest(CommentContent(v)) != Digest(CommentContent(Derive(open(intent("succeeded", 2), &Environment{Status: "running", Hostname: "pr-42-x-abc", DeployedSHA: headB, AppliedGeneration: 5, AppliedRevision: 2}), settings, false, now))) {
		t.Fatal("identical state rendered differently")
	}
	if CommentVersion("no marker") != 0 {
		t.Fatal("version invented")
	}
	if got := ExternalID(9, &Check{HeadSHA: headB, Revision: 3}); got != "previa:pr:9:"+headB+":r3" {
		t.Fatalf("external id %q", got)
	}
	removed := CommentContent(View{Phase: PhaseRemoved, HeadSHA: headB, LogsURL: "https://x/logs"})
	if strings.Contains(removed, "Build log") || strings.Contains(removed, "https://") {
		t.Fatalf("removed preview links somewhere: %q", removed)
	}
}

func TestDeriveWaitingForCapacityAndDisk(t *testing.T) {
	queued := intent("queued", 2)
	queued.Waiting = "capacity"
	v := Derive(open(queued, nil), settings, false, now)
	if v.Phase != PhaseCapacity || v.Check.Status != "queued" || v.Check.Title != "Waiting for capacity" {
		t.Fatalf("waiting for capacity %+v %+v", v, v.Check)
	}
	if body := CommentContent(v); !strings.Contains(body, "Waiting for capacity") || strings.Contains(body, "position") {
		t.Fatalf("comment %q", body)
	}
	// An admitted request waiting for disk keeps the older preview serving.
	running := intent("running", 3)
	running.Waiting = "disk"
	live := &Environment{Status: "running", Hostname: "pr-42-x-abc", DeployedSHA: headB, AppliedGeneration: 5, AppliedRevision: 2}
	v = Derive(open(running, live), settings, true, now)
	if v.Phase != PhaseCapacity || v.Check.Title != "Waiting for disk space" || len(v.Notices) != 1 || !strings.Contains(v.Notices[0], "keeps serving") {
		t.Fatalf("waiting for disk %+v", v)
	}
	// Merge conflicts take precedence over a wait: nothing would start anyway.
	conflicted := open(queued, nil)
	conflicted.BuildStatus = "conflicts"
	if v := Derive(conflicted, settings, false, now); v.Phase != PhaseBlocked {
		t.Fatalf("conflicts while waiting %+v", v)
	}
}

func TestCommentShowsExpiryOfALivePreview(t *testing.T) {
	expires := now.Add(48 * time.Hour)
	live := &Environment{Status: "running", Hostname: "pr-42-x-abc", DeployedSHA: headB, AppliedGeneration: 5, AppliedRevision: 2, ExpiresAt: expires}
	body := CommentContent(Derive(open(intent("succeeded", 2), live), settings, false, now))
	if !strings.Contains(body, "expires at 12:00 UTC on 7 Oct 2026") {
		t.Fatalf("expiry missing: %q", body)
	}
	closed := open(intent("superseded", 2), &Environment{Status: "torn_down", Hostname: "h", ExpiresAt: expires})
	closed.State = "closed"
	if strings.Contains(CommentContent(Derive(closed, settings, true, now)), "expires") {
		t.Fatal("a removed preview still announces its expiry")
	}
}
