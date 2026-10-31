package starterkits

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// commentColumn aligns the inline comments that explain each key.
const commentColumn = 30

// renderConfig writes .previewrc.yaml with a short comment on every key. The
// keys and value formats are the ones Previa's own parser accepts; a test
// runs every kit through it.
func renderConfig(s Stack, c Choices, setup, seed, worker []string, workerName string) string {
	var b strings.Builder
	b.WriteString("# .previewrc.yaml: how Previa runs this repository's previews.\n")
	b.WriteString("# Starter kit: " + s.Name + ".\n\n")
	if s.OwnFile && c.Detail != "Dockerfile" {
		entry(&b, "dockerfile: "+scalar(c.Detail), "your Dockerfile, from the repository root")
	}
	entry(&b, "port: "+strconv.Itoa(c.Port), "your app listens here, on 0.0.0.0")
	if c.Health != "" {
		entry(&b, "health_check_path: "+scalar(c.Health), "ready once this answers 200 to 399")
	} else {
		b.WriteString("# health_check_path: /health   # optional; else Previa waits for the port\n")
	}
	entry(&b, "resource_tier: "+c.Tier, "small, standard or large")
	if c.Postgres != "" || c.Redis != "" || c.MySQL != "" {
		b.WriteString("\n")
		entry(&b, "services:", "empty, or test data kept by Previa")
		if c.Postgres != "" {
			entry(&b, "  postgres: "+c.Postgres, "sets DATABASE_URL and PGHOST, PGUSER…")
		}
		if c.Redis != "" {
			entry(&b, "  redis: "+c.Redis, "sets REDIS_URL")
		}
		if c.MySQL != "" {
			entry(&b, "  mysql: "+c.MySQL, "sets MYSQL_URL and MYSQL_HOST, MYSQL_USER…")
		}
	}
	if len(setup) > 0 {
		b.WriteString("\n# Runs before the app in every deployment, also on kept data: keep it\n# safe to repeat, such as migrations.\n")
		b.WriteString("setup: " + flow(setup) + "\n")
	}
	if len(seed) > 0 {
		b.WriteString("\n# Runs after setup only on an empty database: synthetic starting data.\n")
		b.WriteString("seed: " + flow(seed) + "\n")
	}
	if len(worker) > 0 {
		b.WriteString("\n")
		entry(&b, "processes:", "up to 3, from the same image")
		b.WriteString("  - name: " + workerName + "\n")
		b.WriteString("    command: " + flow(worker) + "\n")
	}
	if c.Docs && c.DocsPath != "" {
		b.WriteString("\n")
		entry(&b, "docs_path: "+scalar(c.DocsPath), "linked from the pull request")
	}
	b.WriteString("\n# Plain settings only. Keep API keys and passwords in Secrets.\n")
	b.WriteString("# env:\n#   - FEATURE_FLAGS=preview\n")
	return b.String()
}

// entry writes one line with its comment aligned.
func entry(b *strings.Builder, line, comment string) {
	pad := commentColumn - len([]rune(line))
	if pad < 2 {
		pad = 2
	}
	b.WriteString(line + strings.Repeat(" ", pad) + "# " + comment + "\n")
}

var plainScalar = regexp.MustCompile(`^[A-Za-z0-9/._-]+$`)

// scalar writes a path or name plainly when YAML reads it unchanged, and
// quoted otherwise.
func scalar(s string) string {
	if plainScalar.MatchString(s) {
		return s
	}
	return quote(s)
}

// flow writes argv as a YAML flow sequence of double-quoted strings. JSON
// string escapes are valid YAML, so any argument round-trips.
func flow(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = quote(arg)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func quote(s string) string {
	var b strings.Builder
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false) // keep && and < readable
	_ = e.Encode(s)        // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// notesFor lists what to check in the app itself: what Previa provides,
// then the stack's own advice.
func notesFor(s Stack, c Choices) []string {
	var notes []string
	if c.Postgres != "" {
		notes = append(notes, "Your app, setup, seed and processes get DATABASE_URL (and PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE). Keep migrations in setup and synthetic starting data in seed: seed runs only on an empty database, so data a repository's Test data keeps is never seeded twice.")
	}
	if c.Redis != "" {
		notes = append(notes, "They also get REDIS_URL for Redis.")
	}
	if c.MySQL != "" {
		notes = append(notes, "MySQL sets MYSQL_URL, MYSQL_HOST, MYSQL_PORT, MYSQL_USER, MYSQL_PASSWORD and MYSQL_DATABASE. DATABASE_URL also points to MySQL unless PostgreSQL is selected. Each rebuild starts empty; install your stack's MySQL driver and read these settings before running migrations.")
		if s.ID == "rails" {
			notes = append(notes, "For Rails, add the mysql2 gem and configure database.yml with adapter: mysql2 and the MYSQL_* settings. Rails expects mysql2:// rather than the supplied mysql:// URL.")
		}
	}
	if s.notes != nil {
		notes = append(notes, s.notes(c)...)
	}
	return notes
}
