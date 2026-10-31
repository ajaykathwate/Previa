package reviews

import (
	"errors"
	"time"

	pages "github.com/ajaykathwatee/previa/web/pages/reviews"
)

var (
	// ErrUnavailable: the pull request, finding or message does not exist for
	// this person, or they may not make this change.
	ErrUnavailable = errors.New("review unavailable")
	// ErrClosed: the pull request is closed or merged, so its review is
	// read-only.
	ErrClosed = errors.New("pull request closed")
	// ErrGone: the finding or message was removed meanwhile.
	ErrGone = errors.New("review content removed")
	// ErrStale: someone resolved, reopened or removed the finding meanwhile.
	ErrStale = errors.New("finding changed")
)

// InputError is a problem with what a form sent. The form is shown again with
// the draft, the field messages beside their fields and Message above them.
type InputError struct {
	Fields  map[string]string
	Message string
}

func (e *InputError) Error() string { return "invalid review input" }

// ConflictError means a draft was written against something that changed:
// a newer deployment was published (version), the message was changed
// (edited) or someone saved the instructions (instructions).
type ConflictError struct {
	Kind            string
	Tested, Current *pages.Deployment
	Saved           pages.Instructions
	Revision        int64 // the current revision, to save against on purpose
}

func (e *ConflictError) Error() string { return "review conflict: " + e.Kind }

// LimitError means the member made their hourly allowance of review changes.
type LimitError struct{ ResetsAt time.Time }

func (e *LimitError) Error() string { return "review write allowance spent" }
