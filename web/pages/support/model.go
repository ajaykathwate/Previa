// Package support contains display data and templates for the support pages.
package support

type Request struct{ ID, Category, Message, Diagnostic, Repository, PR, State, Created, Revision string }
type Incident struct{ ID, Component, Summary, State, Revision, Updated string }
type Availability struct{ Component, State, Checked string }
type Page struct {
	Cleanup                                                                                                 []Cleanup
	Name, CSRF, Email, OperatorName, Error, Saved, MaxBytes, PerDay, RetentionDays, BackupDays, JournalDays string
	Operator, Console                                                                                       bool
	Requests                                                                                                []Request
	Detail                                                                                                  *Request
	Incidents                                                                                               []Incident
	Availability                                                                                            []Availability
}
type Cleanup struct{ ID, Kind, Stage, Reason, Created string }
