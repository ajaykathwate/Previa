package repositories

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func TestRepositoryPageEscapesDataAndPreservesFilters(t *testing.T) {
	var output strings.Builder
	data := Page{Rows: []Row{{FullName: "owner<script>/<script>", Account: "owner<script>", Status: "suspended", InstallationID: 99}}, Number: 2, Pages: 6, Total: 150, Start: 26, End: 50, Installations: 1, Enabled: true, Search: "owner & code", Access: "unavailable"}
	if err := Index("owner", "csrf", data).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Choose repositories on GitHub", "owner&lt;script&gt;", "Installation suspended", "rel=\"next\"", "q=owner+%26+code", "access=unavailable", "aria-current=\"page\"", "csrf_token"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	if strings.Contains(output.String(), "<script>") {
		t.Fatal("unescaped repository name")
	}
	output.Reset()
	data.Enabled = false
	if err := Index("owner", "csrf", data).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	// Not configured: no connection form and no dead button, only the reason.
	if strings.Contains(output.String(), "/installations/start") || !strings.Contains(output.String(), "not configured on this server") ||
		regexp.MustCompile(`<button[^>]*\sdisabled(\s|=|>)`).MatchString(output.String()) {
		t.Fatal("unconfigured installation action offered")
	}
	output.Reset()
	data.Enabled, data.AwaitingApproval = true, true
	if err := Index("owner", "csrf", data).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "/installations/start") || !strings.Contains(output.String(), "early access is approved") ||
		!strings.Contains(output.String(), `href="/early-access"`) {
		t.Fatal("a pending workspace must explain why it cannot connect yet, not offer the button")
	}
}
