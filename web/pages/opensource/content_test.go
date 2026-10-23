package opensource

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	anchor   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func TestEveryAppHasACompleteTest(t *testing.T) {
	caps := map[string]bool{}
	for _, c := range Capabilities() {
		if !anchor.MatchString(c.ID) || c.Name == "" || caps[c.ID] || !slices.Contains(CapabilityGroups(), c.Group) {
			t.Fatalf("invalid or duplicate capability %+v", c)
		}
		caps[c.ID] = true
	}
	seen := map[string]bool{}
	for _, r := range Repos() {
		switch {
		case !anchor.MatchString(r.ID) || seen[r.ID]:
			t.Errorf("%q: needs a unique anchor", r.ID)
		case !repoName.MatchString(r.Name) || r.URL != "https://github.com/"+r.Name:
			t.Errorf("%s: the link must be the repository on GitHub", r.ID)
		case !slices.Contains([]string{"Frontend", "Backend API", "Full-stack"}, r.Kind),
			!slices.Contains([]string{"Small", "Medium", "Large"}, r.Size),
			!slices.Contains([]string{"Basic", "Intermediate", "Advanced"}, r.Level):
			t.Errorf("%s: unknown kind, size or level", r.ID)
		case len(r.Stack) == 0 || r.Architecture == "" || r.Why == "" || r.Config == "" || len(r.Steps) == 0 || len(r.Expect) == 0 || len(r.Covers) == 0:
			t.Errorf("%s: every repository needs a stack, its architecture, a reason, a config, steps and expected results", r.ID)
		}
		seen[r.ID] = true
		for _, c := range r.Covers {
			if !caps[c] {
				t.Errorf("%s covers unknown capability %q", r.ID, c)
			}
		}
		for _, s := range r.Secrets {
			if !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(s.Name) || s.Hint == "" {
				t.Errorf("%s: secret %q needs an environment-style name and a hint", r.ID, s.Name)
			}
		}
		for _, text := range slices.Concat(r.Changes, r.Steps, r.Expect, r.Watch) {
			if strings.Count(text, "`")%2 != 0 {
				t.Errorf("%s: unbalanced code marks in %q", r.ID, text)
			}
		}
	}
	for id := range caps {
		if len(Covering(id)) == 0 {
			t.Errorf("no repository exercises %q", id)
		}
	}
	if len(Repos()) != 10 {
		t.Errorf("%d projects, want exactly ten", len(Repos()))
	}
}

// The page lists each project once and never shows a test status.
func TestPageRendersEveryProjectOnceWithoutStatuses(t *testing.T) {
	var page strings.Builder
	if err := Page(false).Render(context.Background(), &page); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	for _, r := range Repos() {
		if n := strings.Count(html, `id="repo-`+r.ID+`"`); n != 1 {
			t.Errorf("%s rendered %d times", r.ID, n)
		}
		if !strings.Contains(html, `href="#repo-`+r.ID+`"`) {
			t.Errorf("%s is missing from the index", r.ID)
		}
	}
	if n := strings.Count(html, "<span>Test it yourself</span>"); n != len(Repos()) {
		t.Errorf("Test it yourself appears %d times, want one per project (%d)", n, len(Repos()))
	}
	for _, status := range []string{"In testing", "Verified", "Not yet", "Core", ">Status<"} {
		if strings.Contains(html, status) {
			t.Errorf("page shows a test status: %q", status)
		}
	}
	for _, unsafe := range []string{"<script>alert", "&lt;code&gt;"} {
		if strings.Contains(html, unsafe) {
			t.Errorf("page contains %q", unsafe)
		}
	}
}

func TestInlineEscapesTextAndMarksCode(t *testing.T) {
	var out strings.Builder
	if err := inline("Run `a<b>` & <i>go</i>").Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if want := "Run <code>a&lt;b&gt;</code> &amp; &lt;i&gt;go&lt;/i&gt;"; out.String() != want {
		t.Fatalf("inline = %q, want %q", out.String(), want)
	}
}
