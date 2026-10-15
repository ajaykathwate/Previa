package secrets

import (
	"github.com/ajaykathwatee/previa/web/components"
	"strconv"
	"strings"
	"time"
)

// Secret is a stored name; values are never part of any view model.
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

// Row is one line of the secrets editor. Only the name is kept after a
// rejected save; values are never rendered back.
type Row struct {
	Name, Error string
}

// Page is the Secrets tab of one owned repository.
type Page struct {
	RepoID     int64
	FullName   string
	Available  bool // GitHub access is current; adding and replacing require it
	Configured bool // the server has an encryption keyring
	Secrets    []Secret
	Limit      int
	Notice     string
	Error      string // why a save or delete was refused, for the whole form
	// Rows are the editor's lines after a rejected save, or the name being
	// replaced; empty shows one blank line.
	Rows []Row
	// DotenvError explains why pasted .env text was rejected; the text itself
	// is never rendered back.
	DotenvError string
	Replacing   string // name chosen with a row's Replace link
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

// CanEdit reports whether new values can be saved.
func (p Page) CanEdit() bool { return p.Configured && p.Available }

// AtLimit: no new names fit; existing ones can still be replaced.
func (p Page) AtLimit() bool { return len(p.Secrets) >= p.Limit }

// Remaining is how many new names still fit.
func (p Page) Remaining() int { return max(0, p.Limit-len(p.Secrets)) }

// EditorRows are the lines the editor starts with.
func (p Page) EditorRows() []Row {
	if len(p.Rows) == 0 {
		return []Row{{}}
	}
	return p.Rows
}

// Exists reports whether name is already stored, so saving replaces it.
func (p Page) Exists(name string) bool {
	for _, s := range p.Secrets {
		if s.Name == name {
			return true
		}
	}
	return false
}

// ExistingNames lists stored names for the editor's "replaces" hints.
func (p Page) ExistingNames() string {
	names := make([]string, len(p.Secrets))
	for i, s := range p.Secrets {
		names[i] = s.Name
	}
	return strings.Join(names, " ")
}

// RemainingLabel says how many new names fit.
func (p Page) RemainingLabel() string {
	switch n := p.Remaining(); n {
	case 0:
		return "The limit is reached: you can replace existing names but not add new ones."
	case 1:
		return "1 more name fits."
	default:
		return strconv.Itoa(n) + " more names fit."
	}
}

// UpdatedLabel is a compact UTC timestamp for a table cell.
func (s Secret) UpdatedLabel() string { return s.UpdatedAt.UTC().Format("2 Jan 2006, 15:04 UTC") }

func (s Secret) UpdatedISO() string { return s.UpdatedAt.UTC().Format(time.RFC3339) }

func (p Page) PublicReferences() []components.ResourceRef {
	return components.ResourceRefs("repository", p.RepoID)
}
