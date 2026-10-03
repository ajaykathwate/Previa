package dashboard

import (
	"context"
	"strings"
	"testing"
)

func TestRepositoryConnectionsShowsRealDataAndConfigurationState(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var output strings.Builder
		linked := []Installation{{ID: 99, Account: "owner<script>", Status: "active", Repositories: []Repository{{FullName: "owner/repo", Accessible: true}, {FullName: "owner/removed"}}}}
		if err := RepositoryConnections("csrf", enabled, linked).Render(context.Background(), &output); err != nil {
			t.Fatal(err)
		}
		html := output.String()
		if enabled {
			for _, expected := range []string{"/installations/start", "owner/repo", "owner/removed", "Access unavailable", "csrf_token", "owner&lt;script&gt;"} {
				if !strings.Contains(html, expected) {
					t.Fatalf("missing %s", expected)
				}
			}
		} else if strings.Contains(html, "/installations/start") || !strings.Contains(html, "Not configured") {
			t.Fatal("unconfigured installation button was enabled")
		}
	}
}
