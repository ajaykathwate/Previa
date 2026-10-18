package errortracking

import (
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

const testDSN = "https://public@o1.ingest.sentry.io/2"

func TestNoDSNReportsNothing(t *testing.T) {
	reporter, err := Start(Settings{Process: "server"})
	if err != nil {
		t.Fatal(err)
	}
	if handlers := reporter.Handlers(); len(handlers) != 0 {
		t.Fatalf("reporting is off but %d handlers were returned", len(handlers))
	}
	reporter.Flush(time.Millisecond)
	var none *Reporter
	none.Flush(time.Millisecond)
	if none.Handlers() != nil {
		t.Fatal("a nil reporter returned handlers")
	}
}

func TestCheckRejectsAMalformedDSN(t *testing.T) {
	if err := Check(Settings{DSN: "not a dsn"}); err == nil {
		t.Fatal("a malformed DSN was accepted")
	}
	if err := Check(Settings{DSN: testDSN}); err != nil {
		t.Fatalf("a valid DSN was refused: %v", err)
	}
	reporter, err := Start(Settings{DSN: "not a dsn", Process: "worker"})
	if err == nil || len(reporter.Handlers()) != 0 {
		t.Fatalf("a malformed DSN started reporting (err %v)", err)
	}
}

func TestFromEnvTakesTheEnvironmentFromAppEnv(t *testing.T) {
	t.Setenv("SENTRY_DSN", " "+testDSN+" ")
	t.Setenv("APP_ENV", "")
	s := FromEnv("worker", "v1.2.3")
	if s.DSN != testDSN || s.Environment != "development" || s.Release != "v1.2.3" || s.Process != "worker" {
		t.Fatalf("settings: %+v", s)
	}
	t.Setenv("APP_ENV", "production")
	if s := FromEnv("server", "v1"); s.Environment != "production" {
		t.Fatalf("environment %q, want production", s.Environment)
	}
}

func TestErrorsBecomeIssuesAndWarningsOnlyLogs(t *testing.T) {
	transport := &sentry.MockTransport{}
	var mu sync.Mutex
	var logs []string
	reporter, err := start(Settings{DSN: testDSN, Environment: "test", Release: "v1", Process: "worker"},
		sentry.ClientOptions{
			Transport: transport,
			BeforeSendLog: func(l *sentry.Log) *sentry.Log {
				mu.Lock()
				defer mu.Unlock()
				logs = append(logs, l.Body)
				return nil
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	handlers := reporter.Handlers()
	if len(handlers) != 1 {
		t.Fatalf("%d handlers, want 1", len(handlers))
	}
	logger := slog.New(handlers[0]).With("job_kind", "build").WithGroup("build")
	logger.Info("progress")
	logger.Warn("slow", "seconds", 12)
	logger.Error("build failed", "error", "exit status 1", slog.Group("image", "size", 3))
	reporter.Flush(time.Second)

	var issues []*sentry.Event
	for _, event := range transport.Events() {
		if event.Type == "" {
			issues = append(issues, event)
		}
	}
	if len(issues) != 1 {
		t.Fatalf("%d issues, want only the error", len(issues))
	}
	issue := issues[0]
	if issue.Message != "build failed" || issue.Level != sentry.LevelError || issue.Tags["process"] != "worker" ||
		issue.Environment != "test" || issue.Release != "v1" || issue.ServerName != "previa-worker" {
		t.Fatalf("issue: message %q level %q tags %v environment %q release %q server %q",
			issue.Message, issue.Level, issue.Tags, issue.Environment, issue.Release, issue.ServerName)
	}
	fields := issue.Contexts["log"]
	if fields["job_kind"] != "build" || fields["build.error"] != "exit status 1" || fields["build.image.size"] != "3" {
		t.Fatalf("issue context: %v", fields)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(logs, []string{"slow", "build failed"}) {
		t.Fatalf("Sentry logs %q, want the warning and the error only", logs)
	}
}

func TestSendTestDeliversOneIssue(t *testing.T) {
	if err := Test(Settings{Process: "server"}); err == nil {
		t.Fatal("a test without a DSN reported success")
	}
	transport := &sentry.MockTransport{}
	reporter, err := start(Settings{DSN: testDSN, Release: "v1", Process: "server"}, sentry.ClientOptions{
		Transport:     transport,
		BeforeSendLog: func(*sentry.Log) *sentry.Log { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.sendTest("server", time.Second); err != nil {
		t.Fatal(err)
	}
	var issues []*sentry.Event
	for _, event := range transport.Events() {
		if event.Type == "" {
			issues = append(issues, event)
		}
	}
	if len(issues) != 1 || issues[0].Message != "Sentry test from previa-server" {
		t.Fatalf("issues: %d, want the one test error", len(issues))
	}
	if err := (&Reporter{}).sendTest("server", time.Second); err == nil {
		t.Fatal("a reporter that is off claimed to send a test")
	}
}
