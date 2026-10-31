// Package earlyaccess presents workspace approval without backend imports.
package earlyaccess

type Workspace struct {
	ID, Name, Owner, State string
	Revision               int64
}
type Page struct {
	Name, CSRF, Next  string
	Operator, Enabled bool
	Workspace         Workspace
	Pending           []Workspace
}
