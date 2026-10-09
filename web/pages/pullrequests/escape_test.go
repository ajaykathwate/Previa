package pullrequests

import (
	"bytes"
	"strings"
	"testing"
)

// TestGitHubDataRendersAsText puts attacker-controlled GitHub data (a PR's
// title, author, branch and failure output) and a hostile search through the
// real list and detail templates: all of it must come out as text.
func TestGitHubDataRendersAsText(t *testing.T) {
	const (
		script = `<script>alert(document.cookie)</script>`
		image  = `"><img src=x onerror=alert(1)>`
	)
	hostile := PR{Number: 7, Title: script, Author: image, Branch: "feature/" + image, SHA: "1234567890abcdef", State: "open",
		Build: "failed", Available: true, URL: "javascript:alert(1)", Failure: script, FailureLog: []string{script, image},
		Headline: Headline{Tone: "tone-danger", Title: script, Detail: image}}
	page := Page{RepoID: 1, Name: "owner/" + image, Mode: "manual", Available: true, DiscoveryEnabled: true, SyncState: "complete",
		Rows: []PR{hostile}, Number: 1, Pages: 1, Total: 1, Search: `"><script>alert(2)</script>`, State: "open", SelectedPR: &hostile}
	var out bytes.Buffer
	if err := Panel(page, "csrf-token").Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, raw := range []string{"<script>alert", "<img src=x", `href="javascript:`, "onerror=alert(1)>"} {
		if strings.Contains(html, raw) {
			t.Errorf("rendered markup contains %q", raw)
		}
	}
	if !strings.Contains(html, "&lt;script&gt;alert(document.cookie)&lt;/script&gt;") {
		t.Error("the hostile title is not shown, escaped, as text")
	}
	// A genuine GitHub address still links to the pull request.
	hostile.URL = "https://github.com/owner/repo/pull/7"
	page.SelectedPR = &hostile
	out.Reset()
	if err := Panel(page, "csrf-token").Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `href="https://github.com/owner/repo/pull/7"`) {
		t.Error("an HTTPS GitHub address is not linked")
	}
}
