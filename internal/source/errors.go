package source

import (
	"errors"
	"time"
)

// Error is a classified, safe source-preparation failure. Messages never contain
// URLs (archive links carry short-lived tokens), credentials, archive contents or
// raw provider responses.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	cause     error
}

func (e *Error) Error() string { return "prepare source: " + e.Message }
func (e *Error) Unwrap() error { return e.cause }

// Codes returned in Error.Code.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeAuthUnavailable   = "authentication_unavailable"
	CodeAccessDenied      = "access_denied"
	CodeUnavailable       = "resource_unavailable"
	CodeRateLimited       = "rate_limited"
	CodeAPIUnavailable    = "api_unavailable"
	CodeAPIError          = "api_error"
	CodeRedirectRejected  = "redirect_rejected"
	CodeCancelled         = "request_cancelled"
	CodeTooLarge          = "too_large"
	CodeTooManyFiles      = "too_many_files"
	CodeUnsafeArchive     = "unsafe_archive"
	CodeInvalidArchive    = "invalid_archive"
	CodeIdentityMismatch  = "identity_mismatch"
	CodeMissingConfig     = "missing_file"
	CodeInvalidConfig     = "invalid_config"
	CodeMissingDockerfile = "missing_dockerfile"
	CodeSubmodules        = "submodules_unsupported"
	CodeLFS               = "lfs_unsupported"
	CodeWorkspaceFailure  = "workspace_unavailable"
)

func failure(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

func retryable(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Retryable: true, cause: cause}
}

// RetryAfter carries GitHub's rate-limit delay alongside a retryable Error.
type RetryAfter struct {
	Delay time.Duration
	Err   *Error
}

func (e *RetryAfter) Error() string { return e.Err.Error() }
func (e *RetryAfter) Unwrap() error { return e.Err }

// CodeOf returns the failure code, or "" for nil and unclassified errors.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Limits bound one preparation. Zero values are replaced by DefaultLimits.
type Limits struct {
	MaxArchiveBytes   int64         // compressed bytes downloaded
	MaxExtractedBytes int64         // total regular-file bytes after decompression
	MaxEntries        int           // directories, files and symlinks
	MaxPathBytes      int           // one entry path below the archive's top directory
	MaxRedirects      int           // archive download redirects, each to an allowlisted host
	Timeout           time.Duration // whole download, extraction and config load
}

// DefaultLimits suit typical Dockerfile-based web applications.
func DefaultLimits() Limits {
	return Limits{MaxArchiveBytes: 256 << 20, MaxExtractedBytes: 1 << 30, MaxEntries: 100_000, MaxPathBytes: 2048, MaxRedirects: 3, Timeout: 5 * time.Minute}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxArchiveBytes <= 0 {
		l.MaxArchiveBytes = d.MaxArchiveBytes
	}
	if l.MaxExtractedBytes <= 0 {
		l.MaxExtractedBytes = d.MaxExtractedBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxPathBytes <= 0 {
		l.MaxPathBytes = d.MaxPathBytes
	}
	if l.MaxRedirects <= 0 {
		l.MaxRedirects = d.MaxRedirects
	}
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	return l
}
