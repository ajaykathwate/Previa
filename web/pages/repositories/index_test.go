package repositories

import (
	"context"
	"strings"
	"testing"
)

func TestRepositoryPageEscapesDataAndPreservesFilters(t *testing.T) {
	var output strings.Builder
	data := Page{Rows: []Row{{FullName: "owner/<script>", Account: "owner<script>", Status: "suspended", InstallationID: 99}}, Number: 2, Pages: 6, Total: 150, Start: 26, End: 50, Installations: 1, Enabled: true, Search: "owner & code", Access: "unavailable"}
	if err := Index("owner", "csrf", data).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Manage connections", "owner&lt;script&gt;", "Installation suspended", "rel=\"next\"", "q=owner+%26+code", "access=unavailable", "aria-current=\"page\"", "csrf_token"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	if strings.Contains(output.String(), "<script>owner") {
		t.Fatal("unescaped repository name")
	}
	output.Reset()
	data.Enabled = false
	if err := Index("owner", "csrf", data).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "/installations/start") || !strings.Contains(output.String(), "Not configured") {
		t.Fatal("unconfigured installation action enabled")
	}
}
