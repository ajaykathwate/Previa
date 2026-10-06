package pullrequests

import "testing"

func TestCommitURLComesFromThePullRequestAddress(t *testing.T) {
	pr := PR{URL: "https://github.com/owner/app/pull/42"}
	if got := pr.CommitURL("abc123"); got != "https://github.com/owner/app/commit/abc123" {
		t.Fatalf("commit URL %q", got)
	}
	for _, other := range []PR{{URL: ""}, {URL: "javascript:alert(1)/pull/1"}, {URL: "https://github.com/owner/app"}} {
		if got := other.CommitURL("abc123"); got != "" {
			t.Fatalf("%q gave %q", other.URL, got)
		}
	}
	if got := pr.CommitURL(""); got != "" {
		t.Fatalf("empty commit gave %q", got)
	}
}

func TestTookReadsLikeADuration(t *testing.T) {
	for start, want := range map[string]string{
		"2026-10-05T10:00:00Z": "3 min 12 s",
		"2026-10-05T10:03:00Z": "12 s",
		"2026-10-05T10:02:12Z": "1 min",
		"2026-10-05T08:00:00Z": "2 h 3 min",
		"2026-10-05T09:03:12Z": "1 h",
		"not a time":           "",
		"2026-10-05T11:00:00Z": "", // ends before it starts
	} {
		if got := Took(start, "2026-10-05T10:03:12Z"); got != want {
			t.Errorf("Took(%s) = %q, want %q", start, got, want)
		}
	}
}

func TestLabelsAndTeardownWording(t *testing.T) {
	if Label("building") != "Building" || Tone("building") != "tone-info" {
		t.Fatal("an environment that is building has a proper label and tone")
	}
	if (Actions{CancelOnly: true}).TeardownLabel() != "Cancel build" || (Actions{}).TeardownLabel() != "Tear down preview" {
		t.Fatal("teardown wording follows what it would do")
	}
	if !LooksLikeError("#6 ERROR: process did not complete") || !LooksLikeError("npm error code 2") || LooksLikeError("0 errors found") {
		t.Fatal("error lines are recognised by whole words")
	}
	if Gate("conflicts") != "Has merge conflicts" || Gate("open") != "Ready to build" {
		t.Fatal("mergeability wording")
	}
}
