package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces any value the handler withholds.
const Redacted = "[redacted]"

// New returns the process logger: JSON lines on w at level ("debug", "info",
// "warn" or "error"; empty means info). Every record carries the correlation
// fields in its context (With) and passes through redaction.
func New(w io.Writer, level string) (*slog.Logger, error) {
	l, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(NewHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))), nil
}

// ParseLevel reads LOG_LEVEL.
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error")
	}
}

type contextKey struct{}

// With returns ctx carrying correlation attributes (delivery, repository, PR,
// environment, job, commit, request version) that every record logged with
// it includes. Later attributes with the same key replace earlier ones.
func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	existing, _ := ctx.Value(contextKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	for _, a := range existing {
		replaced := false
		for _, b := range attrs {
			if a.Key == b.Key {
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, a)
		}
	}
	return context.WithValue(ctx, contextKey{}, append(merged, attrs...))
}

// Attrs returns the correlation attributes ctx carries.
func Attrs(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(contextKey{}).([]slog.Attr)
	return attrs
}

// Handler adds correlation attributes from the record's context and redacts
// secrets: values of secret-looking keys, and token, key and credential
// shapes inside any string or error. It is the last line of defence; code
// still must not log secrets.
type Handler struct{ inner slog.Handler }

// NewHandler wraps inner with correlation and redaction.
func NewHandler(inner slog.Handler) *Handler { return &Handler{inner: inner} }

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, RedactString(r.Message), r.PC)
	own := make(map[string]bool, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		own[a.Key] = true
		return true
	})
	// The record's own attributes win over the context's, so no key repeats.
	for _, a := range Attrs(ctx) {
		if !own[a.Key] {
			clean.AddAttrs(redactAttr(a))
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = redactAttr(a)
	}
	return &Handler{inner: h.inner.WithAttrs(clean)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name)}
}

// secretKey matches attribute keys whose values are credentials.
var secretKey = regexp.MustCompile(`(?i)(secret|token|password|passwd|authorization|cookie|private[_-]?key|api[_-]?key|credential|dsn|database_url)`)

// secretValue matches credential shapes in free text: GitHub tokens, bearer
// and basic credentials, PEM private keys, and URL user information.
var secretValue = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted},
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|v1\.[0-9a-f]{40})\b`), Redacted},
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 " + Redacted},
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@`), "${1}" + Redacted + "@"},
}

// RedactString removes credential shapes from s.
func RedactString(s string) string {
	for _, v := range secretValue {
		s = v.pattern.ReplaceAllString(s, v.replace)
	}
	return s
}

func redactAttr(a slog.Attr) slog.Attr {
	if secretKey.MatchString(a.Key) && a.Value.Kind() != slog.KindGroup {
		return slog.String(a.Key, Redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactString(v.String()))
	case slog.KindGroup:
		group := v.Group()
		clean := make([]any, len(group))
		for i, g := range group {
			clean[i] = redactAttr(g)
		}
		return slog.Group(a.Key, clean...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, RedactString(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, RedactString(x.String()))
		case []byte:
			return slog.String(a.Key, RedactString(string(x)))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}
