package app

import (
	"os"
	"path"
	"regexp"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

// Previa previews itself: its own .previewrc.yaml must pass the rules every
// repository's does, and the Dockerfile stage a preview builds (the last one,
// since previews name no target) must be the dashboard holding the setup
// command.
func TestPreviasOwnPreviewConfigBuildsTheDashboard(t *testing.T) {
	data, err := os.ReadFile("../../.previewrc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := config.ParseServiceCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	result, err := repoconfig.ParseFor(data, repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: catalog})
	if err != nil {
		t.Fatalf("Previa rejects its own .previewrc.yaml: %v", err)
	}
	dockerfile, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	stages := regexp.MustCompile(`(?m)^FROM .* AS (\w+)$`).FindAllSubmatch(dockerfile, -1)
	if len(stages) == 0 || string(stages[len(stages)-1][1]) != "server" {
		t.Fatal("the Dockerfile's last stage must be server, which previews build")
	}
	last := dockerfile[regexp.MustCompile(`(?m)^FROM .* AS server$`).FindIndex(dockerfile)[0]:]
	if setup := result.Config.Setup; len(setup) == 0 || !regexp.MustCompile(`/out/bin/`+regexp.QuoteMeta(path.Base(setup[0]))+`\b`).Match(last) {
		t.Fatalf("setup %q is not copied into the server stage", setup)
	}
	if result.Config.Port != 8080 {
		t.Fatalf("port = %d, want the server image's 8080", result.Config.Port)
	}
}
