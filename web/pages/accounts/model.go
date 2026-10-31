// Package accounts contains display data and templates for the accounts pages.
package accounts

type Workspace struct{ ID, Name, Role, Revision string }
type Installation struct{ ID, Name, Revision, Workspace string }
type Operation struct{ ID, Kind, Stage, Created string }
type Progress struct {
	ID, Kind, Stage, Error, Created, Completed string
	Journal                                    bool
}
type Page struct {
	Name, CSRF, Error           string
	Workspaces                  []Workspace
	Installations, Repositories []Installation
	Operations                  []Operation
	Progress                    *Progress
}
