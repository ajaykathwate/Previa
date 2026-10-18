package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func record(t *testing.T, log func(*slog.Logger)) map[string]any {
	t.Helper()
	var out bytes.Buffer
	logger, err := New(&out, "debug")
	if err != nil {
		t.Fatal(err)
	}
	log(logger)
	var fields map[string]any
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return fields
}

func TestSecretKeysAndShapesAreRedacted(t *testing.T) {
	token := "ghs_" + strings.Repeat("A1b2", 9)
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----"
	fields := record(t, func(l *slog.Logger) {
		l.With("session_secret", "hunter2").Info("fetched with "+token,
			"github_token", "anything",
			"Authorization", "Bearer abcdefghijkl",
			"error", errors.New("dial postgres://previa:s3cret@db:5432/previa failed"),
			"detail", "header Authorization: token abcdefgh12345 and "+pem,
			slog.Group("request", "cookie", "a=b", "path", "/ok"),
			"repo_id", 42)
	})
	out, _ := json.Marshal(fields)
	for _, leaked := range []string{"hunter2", token, "abcdefghijkl", "s3cret", "abcdefgh12345", "MIIEow", "a=b"} {
		if strings.Contains(string(out), leaked) {
			t.Fatalf("%q leaked: %s", leaked, out)
		}
	}
	if fields["repo_id"] != float64(42) || fields["request"].(map[string]any)["path"] != "/ok" {
		t.Fatalf("ordinary fields lost: %s", out)
	}
	if !strings.Contains(fields["error"].(string), "postgres://[redacted]@db:5432/previa") {
		t.Fatalf("URL credentials not redacted precisely: %v", fields["error"])
	}
}

func TestCorrelationFieldsComeFromTheContext(t *testing.T) {
	ctx := With(context.Background(), slog.String("delivery_id", "d-1"), slog.Int64("pr_id", 7))
	ctx = With(ctx, slog.Int64("pr_id", 8), slog.String("job_kind", "build"))
	fields := record(t, func(l *slog.Logger) { l.InfoContext(ctx, "work") })
	if fields["delivery_id"] != "d-1" || fields["pr_id"] != float64(8) || fields["job_kind"] != "build" {
		t.Fatalf("correlation fields: %v", fields)
	}
}

func TestLevels(t *testing.T) {
	if _, err := ParseLevel("loud"); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	var out bytes.Buffer
	logger, err := New(&out, "warn")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("quiet")
	logger.Warn("loud")
	if strings.Contains(out.String(), "quiet") || !strings.Contains(out.String(), "loud") {
		t.Fatalf("level not applied: %s", out.String())
	}
}

// collector keeps the records it is enabled for, as an error reporter would.
type collector struct {
	min     slog.Level
	attrs   []slog.Attr
	records *[]slog.Record
}

func (c collector) Enabled(_ context.Context, level slog.Level) bool { return level >= c.min }

func (c collector) Handle(_ context.Context, r slog.Record) error {
	r.AddAttrs(c.attrs...)
	*c.records = append(*c.records, r)
	return nil
}

func (c collector) WithAttrs(attrs []slog.Attr) slog.Handler {
	c.attrs = append(append([]slog.Attr{}, c.attrs...), attrs...)
	return c
}

func (c collector) WithGroup(string) slog.Handler { return c }

func TestExtraHandlersGetRedactedRecordsAtTheirOwnLevel(t *testing.T) {
	var out bytes.Buffer
	var reported []slog.Record
	logger, err := New(&out, "error", collector{min: slog.LevelWarn, records: &reported})
	if err != nil {
		t.Fatal(err)
	}
	ctx := With(context.Background(), slog.String("job_kind", "build"))
	logger = logger.With("api_token", "hunter2")
	logger.InfoContext(ctx, "quiet")
	logger.WarnContext(ctx, "slow")
	logger.ErrorContext(ctx, "failed", "error", errors.New("dial postgres://previa:s3cret@db/previa"))

	if strings.Contains(out.String(), "slow") || !strings.Contains(out.String(), "failed") {
		t.Fatalf("the JSON output ignored its own level: %s", out.String())
	}
	if len(reported) != 2 || reported[0].Message != "slow" || reported[1].Message != "failed" {
		t.Fatalf("the extra handler got %d records; want the warning and the error", len(reported))
	}
	fields := map[string]string{}
	reported[1].Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.String()
		return true
	})
	if fields["job_kind"] != "build" || fields["api_token"] != Redacted || strings.Contains(fields["error"], "s3cret") {
		t.Fatalf("the extra handler saw unredacted or uncorrelated fields: %v", fields)
	}
}
