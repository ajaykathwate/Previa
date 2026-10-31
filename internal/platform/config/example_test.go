package config

import (
	"bufio"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// exampleSetting is one NAME=value line of .env.example and its label.
type exampleSetting struct {
	name, value string
	optional    bool
}

var exampleLabel = regexp.MustCompile(`^# \[(Required|Required for [^\]]+|Optional, default [^\]]+)\] \S`)

func readExample(t *testing.T) []exampleSetting {
	t.Helper()
	file, err := os.Open("../../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var settings []exampleSetting
	label := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			if m := exampleLabel.FindStringSubmatch(line); m != nil {
				label = m[1]
			}
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				t.Fatalf("invalid quoted example value for %s", name)
			}
			value = unquoted
		}
		if label == "" {
			t.Fatalf(".env.example: %s has no one-line [Required]/[Optional, default …] comment above it", name)
		}
		settings = append(settings, exampleSetting{name: name, value: value, optional: strings.HasPrefix(label, "Optional")})
		label = ""
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return settings
}

// TestEnvExampleOptionalValuesAreTheDefaults loads the server and worker from
// .env.example twice, once with every line and once without the optional ones:
// the results must match, so the file never changes a default by writing it out.
func TestEnvExampleOptionalValuesAreTheDefaults(t *testing.T) {
	settings := readExample(t)
	routerDir := t.TempDir()
	load := func(withOptional bool) (ServerConfig, WorkerConfig) {
		t.Helper()
		for _, s := range settings {
			if s.optional && !withOptional {
				unsetEnvironment(t, s.name)
				continue
			}
			t.Setenv(s.name, s.value)
		}
		// Placeholders that must point at something real to validate.
		t.Setenv("ROUTER_DYNAMIC_DIR", routerDir)
		server, err := LoadServer()
		if err != nil {
			t.Fatalf("server from .env.example (optional lines %v): %v", withOptional, err)
		}
		worker, err := LoadWorker()
		if err != nil {
			t.Fatalf("worker from .env.example (optional lines %v): %v", withOptional, err)
		}
		return server, worker
	}
	written, writtenWorker := load(true)
	defaults, defaultWorker := load(false)
	if !reflect.DeepEqual(written, defaults) {
		t.Fatalf("server: optional .env.example values differ from the defaults:\n%+v\n%+v", written, defaults)
	}
	if !reflect.DeepEqual(writtenWorker, defaultWorker) {
		t.Fatalf("worker: optional .env.example values differ from the defaults:\n%+v\n%+v", writtenWorker, defaultWorker)
	}
	var optional int
	for _, s := range settings {
		if s.optional {
			optional++
		}
	}
	if optional < 30 {
		t.Fatalf("only %d optional settings found; are the labels intact?", optional)
	}
}
