package environments

import (
	"slices"
	"strings"
	"testing"
)

func TestStartingDataSaysWhereDataStartedAndWhy(t *testing.T) {
	kept := ParseStartingData("kept", []byte(`{"kept":{"sha":"d4e5f6a7b8","deployment":3},"services":[{"kind":"postgres","version":"17","source":"kept"}]}`), false)
	if kept.Title() != "Previous build (d4e5f6a)" || kept.Comment() != "kept from previous build" || len(kept.Reasons()) != 0 {
		t.Fatalf("kept: %q %q %v", kept.Title(), kept.Comment(), kept.Reasons())
	}
	baseline := ParseStartingData("baseline", []byte(`{"baseline":{"branch":"main","sha":"a1b2c3d4e5","built_at":"2026-10-16T10:00:00Z"},
		"fallbacks":[{"source":"kept","reason":"copy_failed"}]}`), false)
	if baseline.Title() != "Repository baseline (main@a1b2c3d)" || !slices.Equal(baseline.Reasons(), []string{"The previous build's data could not be copied."}) {
		t.Fatalf("baseline: %q %v", baseline.Title(), baseline.Reasons())
	}
	empty := ParseStartingData("empty", []byte(`{"fallbacks":[{"source":"baseline","reason":"setup_failed"}],
		"services":[{"kind":"mysql","version":"8.0","source":"empty","note":"version_incompatible"}]}`), true)
	reasons := empty.Reasons()
	if empty.Title() != "Empty database and seed" || empty.Comment() != "empty database and seed" || len(reasons) != 2 ||
		reasons[0] != "The repository baseline did not work with this branch (setup failed)." || reasons[1] != "MySQL 8.0: data from a newer version was not copied." {
		t.Fatalf("empty: %q %v", empty.Title(), reasons)
	}
	// Setup that failed on copied data without a seed command points to seed.
	unseeded := ParseStartingData("empty", []byte(`{"fallbacks":[{"source":"kept","reason":"setup_failed"}]}`), false)
	if r := unseeded.Reasons(); unseeded.Title() != "Empty database" || len(r) != 2 || !strings.Contains(r[1], "move the seeding into seed") {
		t.Fatalf("unseeded: %v", r)
	}
	if none := ParseStartingData("", []byte(`not json`), false); none.Title() != "" || none.Comment() != "" || none.Reasons() != nil {
		t.Fatalf("no SQL database: %+v", none)
	}
}
