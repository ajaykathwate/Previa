package secrets

import (
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/web/components"
)

// Secret is a name the .env file sets, with its build access. Values are
// never part of a view model except the editor's Text.
type Secret struct {
	Name      string
	UpdatedAt time.Time
	Build     bool // Dockerfile steps of trusted builds may read it
}

// BuildLabel says how builds use an allowed secret.
func (s Secret) BuildLabel() string {
	if s.Name == "REGISTRY_AUTH" {
		return "Registry login"
	}
	return "Allowed"
}

// BuildTitle explains the label.
func (s Secret) BuildTitle() string {
	if s.Name == "REGISTRY_AUTH" {
		return "Builds log in to the registries it lists to pull private base images. Dockerfile steps never see it."
	}
	return "Dockerfile steps of this repository's own branches may read it with RUN --mount=type=secret,id=" + s.Name + "; forks never do."
}

// UpdatedLabel is a compact UTC timestamp for a table cell.
func (s Secret) UpdatedLabel() string { return s.UpdatedAt.UTC().Format("2 Jan 2006, 15:04 UTC") }

func (s Secret) UpdatedISO() string { return s.UpdatedAt.UTC().Format(time.RFC3339) }

// Line is one line of the .env file shown with its value hidden: a comment or
// blank line as written, or a name and whether it has a value.
type Line struct {
	Comment  string
	Name     string
	HasValue bool
}

// Page is the Secrets tab of one owned repository: its .env file.
type Page struct {
	RepoID     int64
	FullName   string
	Available  bool // GitHub access is current; new or changed values need it
	Configured bool // the server has an encryption keyring
	Secrets    []Secret
	Notice     string
	Error      string // why a save was refused
	// Editing shows the file with its values in the editor. Text is the file
	// as edited and is set only while editing.
	Editing  bool
	Text     string
	Revision int64 // the revision the editor opened; 0 before the first save
	Lines    []Line
	// UpdatedAt and UpdatedBy describe the last save; zero before the first.
	UpdatedAt time.Time
	UpdatedBy string
	// Removals are the names a save would remove, awaiting confirmation.
	Removals []string
	// Size is the file's length in bytes; MaxKiB its limit.
	Size   int
	MaxKiB int
}

func (p Page) Owner() string {
	owner, _, _ := strings.Cut(p.FullName, "/")
	return owner
}

func (p Page) RepositoryName() string {
	_, name, found := strings.Cut(p.FullName, "/")
	if !found {
		return p.FullName
	}
	return name
}

// CanEdit reports whether the file can be opened for editing.
func (p Page) CanEdit() bool { return p.Configured }

func (p Page) URL() string { return "/repositories/" + strconv.FormatInt(p.RepoID, 10) + "/secrets" }

// Summary describes the file under its heading.
func (p Page) Summary() string {
	set := len(p.Secrets)
	what := strconv.Itoa(set) + " variables"
	if set == 1 {
		what = "1 variable"
	}
	switch {
	case !p.Configured:
		return "Encryption is not configured, so the file cannot be opened; " + what + " are stored."
	case p.UpdatedAt.IsZero() && set == 0:
		return "No variables yet · " + p.SizeLabel()
	case p.UpdatedAt.IsZero():
		return "Sets " + what + " · " + p.SizeLabel() + " · saved one at a time before .env files. Save it once to add comments."
	case p.UpdatedBy != "":
		return "Sets " + what + " · " + p.SizeLabel() + " · saved " + p.UpdatedAt.UTC().Format("2 Jan 2006, 15:04 UTC") + " by " + p.UpdatedBy
	default:
		return "Sets " + what + " · " + p.SizeLabel() + " · saved " + p.UpdatedAt.UTC().Format("2 Jan 2006, 15:04 UTC")
	}
}

// SizeLabel is how much of the limit the file uses, as "1.2 KiB of 64 KiB".
func (p Page) SizeLabel() string {
	return FormatSize(p.Size) + " of " + strconv.Itoa(p.MaxKiB) + " KiB"
}

// FormatSize writes a byte count as ui.js does while the file is edited.
func FormatSize(n int) string {
	if n < 1024 {
		return strconv.Itoa(n) + " B"
	}
	return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KiB"
}

// OverLimit reports a file too large to save.
func (p Page) OverLimit() bool { return p.Size > p.MaxKiB*1024 }

// RemovalTitle asks to confirm the names a save removes.
func (p Page) RemovalTitle() string {
	if len(p.Removals) == 1 {
		return "Remove " + p.Removals[0] + "?"
	}
	return "Remove " + strconv.Itoa(len(p.Removals)) + " secrets?"
}

func (p Page) PublicReferences() []components.ResourceRef {
	return components.ResourceRefs("repository", p.RepoID)
}
