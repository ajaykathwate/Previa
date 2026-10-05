package runtime

import (
	"errors"
	"fmt"

	"github.com/ajaykathwatee/previa/internal/environments"
)

// Failure codes. Messages are safe to show the PR owner; causes stay in Err
// and never include credentials or secret values.
const (
	CodeNotCurrent   = "not_current"         // a newer commit, request or close replaced this attempt
	CodeInaccessible = "inaccessible"        // repository access or the installation was revoked
	CodeSource       = "source_failed"       // the exact commit could not be prepared
	CodeBuild        = "build_failed"        // the image build failed
	CodeSecrets      = "secrets_unavailable" // secrets exist but could not be decrypted
	CodeCapacity     = "capacity"            // previews already use the host's budget
	CodeDiskLow      = "disk_low"            // the build host is short of disk space
	CodeUnhealthy    = "unhealthy"           // not ready before the startup deadline
	CodeCrashed      = "crashed"             // exited repeatedly while starting
	CodeTimeout      = "timeout"
	CodeCancelled    = "cancelled"
	CodeUnavailable  = "runtime_unavailable" // Docker or the egress gateway failed
	CodeNotOwned     = "not_owned"           // removal refused: not a Previa object
	CodeMissing      = "missing"             // the candidate no longer exists
	CodeInUse        = "in_use"              // still used by a container; removal retries later
)

// Error is a classified runtime failure.
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

// CodeOf returns err's runtime failure code, or "" for an unclassified error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// AsOperationFailure gives a runtime failure the stable code and permanence a
// durable operation records; other errors stay retryable.
func AsOperationFailure(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return &environments.OperationFailure{Code: e.Code, Permanent: !e.Retryable, Err: err}
	}
	return &environments.OperationFailure{Code: CodeUnavailable, Err: err}
}

// FailureCode is the stable category the environment records and reports
// show for a failed attempt; the message stays in the build log.
func (e *Error) FailureCode() string { return e.Code }
