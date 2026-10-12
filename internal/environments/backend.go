package environments

import (
	"encoding/json"
	"strings"
)

// Backend is what a deployed preview runs next to its app (recorded in
// preview_components when its candidate became ready), for reports and the
// dashboard. It never holds credentials.
type Backend struct {
	Services         []BackendService
	Setup            bool
	Processes        []string
	DocsPath         string
	Outbound         int
	OutboundWithheld bool
}

// BackendService is one managed service of a deployed preview.
type BackendService struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// ParseBackend reads a preview_components row's JSON columns; malformed or
// missing values read as none.
func ParseBackend(services, processes []byte, setup bool, docsPath string, outbound int32, withheld bool) Backend {
	b := Backend{Setup: setup, DocsPath: docsPath, Outbound: int(outbound), OutboundWithheld: withheld}
	_ = json.Unmarshal(services, &b.Services)
	_ = json.Unmarshal(processes, &b.Processes)
	return b
}

// Empty reports whether the preview runs nothing next to its app.
func (b Backend) Empty() bool {
	return len(b.Services) == 0 && !b.Setup && len(b.Processes) == 0 && b.DocsPath == "" && b.Outbound == 0 && !b.OutboundWithheld
}

// ServiceTitle names a managed service for people, such as "PostgreSQL 17".
func ServiceTitle(kind, version string) string {
	switch kind {
	case "postgres":
		return "PostgreSQL " + version
	case "redis":
		return "Redis " + version
	case "mysql":
		return "MySQL " + version
	}
	return kind + " " + version
}

// Summary is one line, such as "PostgreSQL 17 and Redis 7 · setup ran ·
// process worker"; empty when there is nothing to say.
func (b Backend) Summary() string {
	var parts []string
	if len(b.Services) > 0 {
		titles := make([]string, len(b.Services))
		for i, s := range b.Services {
			titles[i] = ServiceTitle(s.Kind, s.Version)
		}
		parts = append(parts, strings.Join(titles, " and "))
	}
	if b.Setup {
		parts = append(parts, "setup ran")
	}
	switch len(b.Processes) {
	case 0:
	case 1:
		parts = append(parts, "process "+b.Processes[0])
	default:
		parts = append(parts, "processes "+strings.Join(b.Processes, ", "))
	}
	return strings.Join(parts, " · ")
}
