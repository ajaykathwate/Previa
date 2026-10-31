package errortracking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	sentryslog "github.com/getsentry/sentry-go/slog"
)

// Settings configure a Reporter.
type Settings struct {
	// DSN is the Sentry project's client key (SENTRY_DSN); empty turns
	// reporting off.
	DSN string
	// Environment separates production from development reports (APP_ENV).
	Environment string
	// Release is the build's version.
	Release string
	// Process names the reporting process: "server" or "worker".
	Process string
}

// FromEnv reads SENTRY_DSN and APP_ENV for process.
func FromEnv(process, release string) Settings {
	environment := strings.TrimSpace(os.Getenv("APP_ENV"))
	if environment == "" {
		environment = "development"
	}
	return Settings{
		DSN:         strings.TrimSpace(os.Getenv("SENTRY_DSN")),
		Environment: environment,
		Release:     release,
		Process:     process,
	}
}

// Check validates s without contacting Sentry (check-config).
func Check(s Settings) error {
	if s.DSN == "" {
		return nil
	}
	if _, err := sentry.NewDsn(s.DSN); err != nil {
		return fmt.Errorf("SENTRY_DSN is not a Sentry DSN (copy it from the project's Client Keys page): %w", err)
	}
	return nil
}

// Reporter forwards log records to Sentry. A nil Reporter, or one started
// without a DSN, reports nothing.
type Reporter struct {
	client *sentry.Client
	hub    *sentry.Hub
}

// Start creates the Sentry client for s. Without a DSN it returns a Reporter
// that reports nothing. On error the returned Reporter reports nothing either,
// so the process can keep running and log why.
func Start(s Settings) (*Reporter, error) {
	if s.DSN == "" {
		return &Reporter{}, nil
	}
	return start(s, sentry.ClientOptions{})
}

// start is Start with base client options (tests supply a transport).
func start(s Settings, options sentry.ClientOptions) (*Reporter, error) {
	if err := Check(s); err != nil {
		return &Reporter{}, err
	}
	options.Dsn = s.DSN
	options.Environment = s.Environment
	options.Release = s.Release
	options.ServerName = "previa-" + s.Process
	client, err := sentry.NewClient(options)
	if err != nil {
		return &Reporter{}, fmt.Errorf("start Sentry: %w", err)
	}
	scope := sentry.NewScope()
	scope.SetTag("process", s.Process)
	return &Reporter{client: client, hub: sentry.NewHub(client, scope)}, nil
}

// Handlers returns the log handlers to add beside the process's own output,
// or none when reporting is off.
func (r *Reporter) Handlers() []slog.Handler {
	if r == nil || r.hub == nil {
		return nil
	}
	logs := sentryslog.Option{LogLevel: []slog.Level{slog.LevelWarn, slog.LevelError}}.
		NewSentryHandler(sentry.SetHubOnContext(context.Background(), r.hub))
	return []slog.Handler{&handler{hub: r.hub, logs: logs}}
}

// Flush waits up to timeout for queued reports to be sent.
func (r *Reporter) Flush(timeout time.Duration) {
	if r == nil || r.client == nil {
		return
	}
	r.client.Flush(timeout)
}

// Test sends one test error the way real errors travel and waits until Sentry
// has it, so an operator can prove delivery and the alert email
// (`previa-server sentry-test`).
func Test(s Settings) error {
	if s.DSN == "" {
		return errors.New("SENTRY_DSN is not set")
	}
	reporter, err := Start(s)
	if err != nil {
		return err
	}
	return reporter.sendTest(s.Process, 10*time.Second)
}

func (r *Reporter) sendTest(process string, timeout time.Duration) error {
	handlers := r.Handlers()
	if len(handlers) == 0 {
		return errors.New("error reporting is off")
	}
	slog.New(handlers[0]).Error("Sentry test from previa-"+process,
		"sent_by", "previa-"+process+" sentry-test")
	if !r.client.Flush(timeout) {
		return fmt.Errorf("the test error was not delivered within %s; check that the server reaches Sentry over HTTPS", timeout)
	}
	return nil
}

// handler sends every warning and error to Sentry Logs, and every error also
// as an event that Sentry groups into issues by message.
type handler struct {
	hub    *sentry.Hub
	logs   slog.Handler
	attrs  []slog.Attr
	groups []string
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.logs.Handle(ctx, r); err != nil {
		return fmt.Errorf("send log to Sentry: %w", err)
	}
	if r.Level >= slog.LevelError {
		h.hub.CaptureEvent(h.event(r))
	}
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	prefix := strings.Join(h.groups, ".")
	qualified := slices.Clip(slices.Clone(h.attrs))
	for _, a := range attrs {
		if prefix != "" && a.Key != "" {
			a.Key = prefix + "." + a.Key
		}
		qualified = append(qualified, a)
	}
	return &handler{hub: h.hub, logs: h.logs.WithAttrs(attrs), attrs: qualified, groups: h.groups}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{hub: h.hub, logs: h.logs.WithGroup(name), attrs: h.attrs,
		groups: append(slices.Clip(slices.Clone(h.groups)), name)}
}

// event turns an error record into a Sentry event: the message names the
// issue, and every attribute (correlation fields included) is kept as context.
func (h *handler) event(r slog.Record) *sentry.Event {
	fields := sentry.Context{}
	for _, a := range h.attrs {
		addField(fields, "", a)
	}
	prefix := strings.Join(h.groups, ".")
	r.Attrs(func(a slog.Attr) bool {
		addField(fields, prefix, a)
		return true
	})
	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Message = r.Message
	event.Logger = "slog"
	if !r.Time.IsZero() {
		event.Timestamp = r.Time
	}
	if len(fields) > 0 {
		event.Contexts["log"] = fields
	}
	return event
}

func addField(fields sentry.Context, prefix string, a slog.Attr) {
	key := a.Key
	switch {
	case key == "":
		key = prefix
	case prefix != "":
		key = prefix + "." + key
	}
	value := a.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		for _, member := range value.Group() {
			addField(fields, key, member)
		}
		return
	}
	if key != "" {
		fields[key] = value.String()
	}
}
