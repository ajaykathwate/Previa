package layouts_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/web/components"
	"github.com/ajaykathwatee/previa/web/layouts"
)

func TestNavigationSelectionDoesNotDependOnTitle(t *testing.T) {
	for _, tt := range []struct {
		name     string
		title    string
		active   components.NavItem
		selected bool
	}{
		{"overview with a different title", "Workspace", components.NavOverview, true},
		{"session with dashboard title", "Dashboard", components.NavNone, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output strings.Builder
			if err := layouts.App(tt.title, "octocat", "fixture-csrf", tt.active).Render(context.Background(), &output); err != nil {
				t.Fatalf("render layout: %v", err)
			}
			if got := strings.Contains(output.String(), `aria-current="page"`); got != tt.selected {
				t.Fatalf("selected navigation = %v; want %v", got, tt.selected)
			}
		})
	}
}
