package environments

import (
	"encoding/json"
	"strings"
)

// StartingData is where a deployment's SQL databases started
// (docs/32-persistent-test-data.md), read from its components' record. It
// names sources and reasons, never data.
type StartingData struct {
	Source string // "kept", "baseline" or "empty"; "" without a SQL database
	// Kept: the deployment its data was copied from.
	KeptSHA        string
	KeptDeployment int
	// Baseline: the baseline it restored.
	BaselineBranch, BaselineSHA, BaselineBuiltAt string
	Fallbacks                                    []DataFallback
	// Incompatible lists services that refused data copied from a newer
	// version, such as "PostgreSQL 15".
	Incompatible []string
	Seeded       bool // the seed command ran
}

// DataFallback is a source the build tried first, and why it moved on.
type DataFallback struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// ParseStartingData reads a preview_components row's data_source,
// data_detail and seed timing; malformed detail reads as none.
func ParseStartingData(source string, detail []byte, seeded bool) StartingData {
	d := StartingData{Source: source, Seeded: seeded}
	var raw struct {
		Kept struct {
			SHA        string `json:"sha"`
			Deployment int    `json:"deployment"`
		} `json:"kept"`
		Baseline struct {
			Branch  string `json:"branch"`
			SHA     string `json:"sha"`
			BuiltAt string `json:"built_at"`
		} `json:"baseline"`
		Services []struct {
			Kind    string `json:"kind"`
			Version string `json:"version"`
			Note    string `json:"note"`
		} `json:"services"`
		Fallbacks []DataFallback `json:"fallbacks"`
	}
	if len(detail) > 0 && json.Unmarshal(detail, &raw) == nil {
		d.KeptSHA, d.KeptDeployment = raw.Kept.SHA, raw.Kept.Deployment
		d.BaselineBranch, d.BaselineSHA, d.BaselineBuiltAt = raw.Baseline.Branch, raw.Baseline.SHA, raw.Baseline.BuiltAt
		d.Fallbacks = raw.Fallbacks
		for _, s := range raw.Services {
			if s.Note == "version_incompatible" {
				d.Incompatible = append(d.Incompatible, ServiceTitle(s.Kind, s.Version))
			}
		}
	}
	return d
}

func short(sha string) string { return sha[:min(7, len(sha))] }

// Title names the source for the pull request page, such as "Previous
// build (d4e5f6)" or "Repository baseline (main@a1b2c3)".
func (d StartingData) Title() string {
	switch d.Source {
	case "kept":
		if d.KeptSHA != "" {
			return "Previous build (" + short(d.KeptSHA) + ")"
		}
		return "Previous build"
	case "baseline":
		if d.BaselineBranch != "" && d.BaselineSHA != "" {
			return "Repository baseline (" + d.BaselineBranch + "@" + short(d.BaselineSHA) + ")"
		}
		return "Repository baseline"
	case "empty":
		if d.Seeded {
			return "Empty database and seed"
		}
		return "Empty database"
	}
	return ""
}

// Reasons explains, in order, every source the build could not use.
func (d StartingData) Reasons() []string {
	var out []string
	for _, f := range d.Fallbacks {
		what := "The previous build's data"
		if f.Source == "baseline" {
			what = "The repository baseline"
		}
		why := "could not be used"
		switch f.Reason {
		case "setup_failed":
			why = "did not work with this branch (setup failed)"
		case "copy_failed":
			why = "could not be copied"
		case "restore_failed":
			why = "could not be restored"
		case "too_large":
			why = "did not fit the database's storage"
		}
		out = append(out, what+" "+why+".")
	}
	if len(d.Incompatible) > 0 {
		out = append(out, strings.Join(d.Incompatible, " and ")+": data from a newer version was not copied.")
	}
	for _, f := range d.Fallbacks {
		if f.Reason == "setup_failed" && d.Source == "empty" && !d.Seeded {
			out = append(out, "If setup also seeds data, move the seeding into seed: it runs only on empty databases.")
			break
		}
	}
	return out
}

// Comment is the source for the GitHub comment, such as "kept from previous
// build".
func (d StartingData) Comment() string {
	switch d.Source {
	case "kept":
		return "kept from previous build"
	case "baseline":
		return "repository baseline"
	case "empty":
		if d.Seeded {
			return "empty database and seed"
		}
		return "empty database"
	}
	return ""
}
