package builds

import (
	"errors"
	"fmt"
)

// Failure codes. Messages are safe to show to the PR owner; causes stay in
// Err for operators and never include credentials.
const (
	CodeInvalidJob         = "invalid_job"
	CodeBuilderUnavailable = "builder_unavailable" // Docker, BuildKit or egress setup failed
	CodeDiskLow            = "disk_low"            // too little free disk to start
	CodeDiskBudget         = "disk_budget"         // the build used more disk than allowed
	CodeBuildFailed        = "build_failed"        // the Dockerfile or one of its steps failed
	CodeTimeout            = "timeout"
	CodeCancelled          = "cancelled"
	CodeImageTooLarge      = "image_too_large"
	CodeSuperseded         = "superseded" // a newer attempt owns the logs
)

// Error is a classified build failure.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func failure(code, message string, retryable bool, err error) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable, Err: err}
}

// CodeOf returns err's failure code, or "" for an unclassified error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
