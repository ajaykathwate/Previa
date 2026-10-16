package reviews

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Content limits (README-MVP-PRODUCT-EXPANSION.md §13). Removing a message
// or finding does not give its allowance back.
const (
	MaxMessageBytes      = 8 << 10  // a message's text, and separately its reproduction details
	MaxInstructionsBytes = 32 << 10 // all of a pull request's instruction fields together
	MaxSummaryRunes      = 500
	MaxSteps             = 30
	MaxStepRunes         = 500
	MaxPathLength        = 300
	MaxSearchRunes       = 100
	MaxThreads           = 500  // findings per pull request
	MaxMessages          = 5000 // messages per pull request, opening messages included
	MaxThreadMessages    = 200  // messages per finding
	// DefaultWriteAllowance is how many review changes one member may make
	// per hour, across all pull requests.
	DefaultWriteAllowance = 300
)

var requestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// A shareable path is rejected when a query parameter's name contains one of
// secretParts, or one of its _-. separated words is a secretWord: such
// parameters carry credentials.
var (
	secretParts = []string{"token", "password", "passwd", "secret", "apikey", "api_key", "access_key", "signature", "credential"}
	secretWords = map[string]bool{"auth": true, "authorization": true, "session": true, "sessionid": true, "sid": true, "pwd": true, "jwt": true, "otp": true, "sig": true}
)

// secretParameter reports whether a query parameter name carries credentials.
func secretParameter(name string) bool {
	lower := strings.ToLower(name)
	for _, part := range secretParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	for _, word := range strings.FieldsFunc(lower, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if secretWords[word] {
			return true
		}
	}
	return false
}

// cleanText normalizes line endings and trims surrounding space. It rejects
// invalid UTF-8, control characters other than tab and newline, and the
// invisible direction overrides that can disguise text, then enforces the
// byte limit. The second result is a person-facing problem, or empty.
func cleanText(raw string, maxBytes int) (string, string) {
	if !utf8.ValidString(raw) {
		return raw, "Use plain text."
	}
	text := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n"))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
		case unicode.IsControl(r):
			return text, "Remove control characters."
		case r >= '‪' && r <= '‮', r >= '⁦' && r <= '⁩':
			return text, "Remove invisible text-direction characters."
		}
	}
	if len(text) > maxBytes {
		return text, "Keep this under " + sizeLabel(maxBytes) + "."
	}
	return text, ""
}

func sizeLabel(n int) string {
	if n%1024 == 0 {
		return strconv.Itoa(n/1024) + " KB"
	}
	return strconv.Itoa(n) + " bytes"
}

// relativePath accepts an address inside the preview, such as
// "/checkout?step=2", and rejects anything that leaves it or carries
// credentials. Empty stays empty.
func relativePath(raw string) (string, string) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", ""
	}
	const hint = "Use a path inside the preview that starts with /, such as /checkout."
	if len(path) > MaxPathLength || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.ContainsAny(path, "\\ \t\n\r") {
		return path, hint
	}
	for _, r := range path {
		if unicode.IsControl(r) || r > unicode.MaxASCII && !unicode.IsPrint(r) {
			return path, hint
		}
	}
	u, err := url.Parse(path)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return path, hint
	}
	for name := range u.Query() {
		if secretParameter(name) {
			return path, "Remove the " + name + " parameter: shared paths must not carry tokens or passwords."
		}
	}
	return path, ""
}

// validRequestKey reports whether a form's idempotency key is well formed.
func validRequestKey(key string) bool { return requestKeyPattern.MatchString(key) }

// validReason reports a known moderation reason.
func validReason(reason string) bool {
	switch reason {
	case "spam", "abuse", "sensitive", "mistake":
		return true
	}
	return false
}
