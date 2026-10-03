package repoconfig

import (
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
)

type Issue struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

// ValidationError contains only safe messages and locations, never source text.
type ValidationError struct {
	Issues []Issue `json:"issues"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		location := issue.Field
		if issue.Line > 0 {
			location += fmt.Sprintf(" (line %d, column %d)", issue.Line, issue.Column)
		}
		parts = append(parts, location+": "+issue.Message)
	}
	return "Invalid " + Filename + ": " + strings.Join(parts, "; ")
}

func validationIssue(field, code, message string) error {
	return &ValidationError{Issues: []Issue{{Field: field, Code: code, Message: message}}}
}

// Upstream YAML errors include source snippets. Keep only category and location.
func safeYAMLError(err error) error {
	issue := Issue{Field: "$", Code: "invalid_yaml", Message: "Use a single valid YAML mapping with supported field types."}
	var duplicate *yaml.DuplicateKeyError
	if errors.As(err, &duplicate) {
		issue.Code, issue.Message = "duplicate_key", "Remove the duplicate YAML key."
	}
	var yamlError yaml.Error
	if errors.As(err, &yamlError) && yamlError.GetToken() != nil && yamlError.GetToken().Position != nil {
		issue.Line = yamlError.GetToken().Position.Line
		issue.Column = yamlError.GetToken().Position.Column
	}
	return &ValidationError{Issues: []Issue{issue}}
}

// FetchError distinguishes acquisition failures from invalid file contents.
// It intentionally does not retain raw GitHub error bodies or credentials.
type FetchError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	cause     error
}

func (e *FetchError) Error() string { return "Fetch " + Filename + ": " + e.Message }
func (e *FetchError) Unwrap() error { return e.cause }
