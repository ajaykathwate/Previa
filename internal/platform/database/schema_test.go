package database

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The constant moves with every new migration, or processes would start
// against a schema they do not know.
func TestSchemaVersionMatchesTheNewestMigration(t *testing.T) {
	files, err := filepath.Glob("../../../db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	var newest int64
	for _, f := range files {
		prefix, _, _ := strings.Cut(filepath.Base(f), "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("%s: unnumbered migration", f)
		}
		newest = max(newest, v)
	}
	if newest != SchemaVersion {
		t.Fatalf("newest migration is %d but SchemaVersion is %d", newest, SchemaVersion)
	}
}
