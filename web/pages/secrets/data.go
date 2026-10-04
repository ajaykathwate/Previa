package secrets

import (
	"strings"
	"time"
)

// Secret is a stored name; values are never part of any view model.
type Secret struct {
	Name      string
	UpdatedAt time.Time
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
	Error      string
	ErrorField string // "name", "value" or "" for form-level errors
	FormName   string // prefilled name when replacing or after a rejected submit
	Replacing  bool
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

func (p Page) AtLimit() bool { return len(p.Secrets) >= p.Limit && !p.Replacing }

// UpdatedLabel is a compact UTC timestamp for a table cell.
func (s Secret) UpdatedLabel() string { return s.UpdatedAt.UTC().Format("2 Jan 2006, 15:04 UTC") }

func (s Secret) UpdatedISO() string { return s.UpdatedAt.UTC().Format(time.RFC3339) }
