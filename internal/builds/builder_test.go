package builds

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

type discardSink struct{ io.Writer }

func (discardSink) System(string) {}

const testInstance = "0a1b2c3d4e5f"

func validJob() Job {
	return Job{Instance: testInstance, PullRequestID: 7, Generation: 3, Revision: 2, HeadSHA: testCommit, BaseRepoID: 100, HeadRepoID: 100,
		ContextDir: "/work/source", Dockerfile: "docker/Dockerfile", Logs: discardSink{io.Discard}}
}

func TestJobValidation(t *testing.T) {
	if err := validJob().validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Job){
		"no PR":               func(j *Job) { j.PullRequestID = 0 },
		"no revision":         func(j *Job) { j.Revision = 0 },
		"short SHA":           func(j *Job) { j.HeadSHA = testCommit[:12] },
		"uppercase SHA":       func(j *Job) { j.HeadSHA = strings.ToUpper(testCommit) },
		"branch name":         func(j *Job) { j.HeadSHA = "main" },
		"no base repository":  func(j *Job) { j.BaseRepoID = 0 },
		"relative context":    func(j *Job) { j.ContextDir = "source" },
		"escaping Dockerfile": func(j *Job) { j.Dockerfile = "../Dockerfile" },
		"absolute Dockerfile": func(j *Job) { j.Dockerfile = "/etc/Dockerfile" },
		"no log sink":         func(j *Job) { j.Logs = nil },
		"no instance":         func(j *Job) { j.Instance = "" },
	} {
		j := validJob()
		change(&j)
		if err := j.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestImageIdentityAndCacheScopes(t *testing.T) {
	j := validJob()
	if got := j.ImageRef(); got != "previa/"+testInstance+"-pr-7:g3-r2-0123456789ab" {
		t.Fatalf("image ref %q", got)
	}
	retry := j
	retry.Revision = 3 // Same-SHA rebuild: a new revision, a new image.
	if retry.ImageRef() == j.ImageRef() {
		t.Fatal("revisions must not share an image")
	}
	labels := j.identity(dockerx.KindImage)
	if labels[dockerx.LabelInstance] != testInstance || labels[dockerx.LabelPR] != "7" || labels[dockerx.LabelGeneration] != "3" || labels[dockerx.LabelRevision] != "2" || labels[dockerx.LabelSHA] != testCommit || labels[dockerx.LabelKind] != dockerx.KindImage {
		t.Fatalf("labels %v", labels)
	}
	fork := j
	fork.HeadRepoID = 555
	otherPR := j
	otherPR.PullRequestID = 8
	unknown := j
	unknown.HeadRepoID = 0
	if j.cacheScope() != otherPR.cacheScope() {
		t.Fatal("same-repository PRs should share a cache")
	}
	if j.cacheScope() == fork.cacheScope() || fork.cacheScope() != "r100-h555" {
		t.Fatalf("a fork must get its own cache: %s %s", j.cacheScope(), fork.cacheScope())
	}
	if unknown.cacheScope() == j.cacheScope() || unknown.cacheScope() != "r100-pr7" {
		t.Fatalf("an unknown head must not share a cache: %s", unknown.cacheScope())
	}
}

func TestParseDFAvailable(t *testing.T) {
	out := "Filesystem     1024-blocks      Used Available Capacity Mounted on\n/dev/sde        1055762868   4831788 997227608       1% /home/user/.local/share/buildkit"
	got, err := parseDFAvailable(out)
	if err != nil || got != 997227608<<10 {
		t.Fatalf("available %d %v", got, err)
	}
	for _, bad := range []string{"", "header only", "a b c\nx y z", "h\n/dev 1 2 -5 1% /"} {
		if _, err := parseDFAvailable(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestCappedWriterStopsTheExportAtTheLimit(t *testing.T) {
	reader, writer := io.Pipe()
	capped := &cappedWriter{w: writer, limit: 10}
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(reader)
		read <- err
	}()
	if _, err := capped.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := capped.Write([]byte("678901")); !errors.Is(err, errImageTooLarge) {
		t.Fatalf("over-limit write: %v", err)
	}
	if err := <-read; !errors.Is(err, errImageTooLarge) {
		t.Fatalf("the reader must see the failure: %v", err)
	}
}

func TestFailureClassificationAndMessages(t *testing.T) {
	b := &Builder{cfg: Config{DiskBudget: 20 << 30, Memory: 4 << 30, MaxImageSize: 4 << 30}}
	fallback := failure(CodeBuildFailed, "fallback", false, nil)
	check := func(ctx context.Context, want string) {
		t.Helper()
		if got := b.interrupted(ctx, errors.New("cause"), fallback); got.Code != want {
			t.Fatalf("code %q, want %q", got.Code, want)
		}
	}
	check(context.Background(), CodeBuildFailed)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errSupersededAttempt)
	check(ctx, CodeSuperseded)
	ctx, cancel = context.WithCancelCause(context.Background())
	cancel(errDiskBudget)
	check(ctx, CodeDiskBudget)
	ctx, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	<-ctx.Done()
	check(ctx, CodeTimeout)
	ctx, cancelPlain := context.WithCancel(context.Background())
	cancelPlain()
	check(ctx, CodeCancelled)
	if got := summarize(errors.New("rpc error: code = Unknown desc = process \"/bin/sh -c false\" did not complete successfully: exit code: 1\nstack")); got != `process "/bin/sh -c false" did not complete successfully: exit code: 1` {
		t.Fatalf("summary %q", got)
	}
	if got := summarize(errors.New(strings.Repeat("x", 900))); len(got) > 510 {
		t.Fatalf("summary not bounded: %d", len(got))
	}
	if CodeOf(failure(CodeDiskLow, "m", true, nil)) != CodeDiskLow || CodeOf(errors.New("plain")) != "" {
		t.Fatal("CodeOf")
	}
	if humanBytes(512) != "512 B" || humanBytes(4<<30) != "4.0 GiB" || humanBytes(1536) != "1.5 KiB" {
		t.Fatal("humanBytes")
	}
}

func TestBuilderConfiguration(t *testing.T) {
	c := ConfigFrom(config.BuilderConfig{BuildKitImage: "bk", BuildCPUs: 2, BuildMemory: 4 << 30, BuildPIDs: 2048,
		BuildDiskBudget: 20 << 30, BuildMaxImageSize: 4 << 30, BuildMinFreeDisk: 15 << 30}, testInstance, 12*time.Minute)
	if c.Memory != 4<<30 || c.MinFreeDisk != 15<<30 || c.MaxDuration != 12*time.Minute || c.BuildKitImage != "bk" {
		t.Fatalf("config %+v", c)
	}
	if _, err := NewBuilder(nil, nil, c, nil); err == nil {
		t.Fatal("a builder needs Docker")
	}
	b := &Builder{cfg: c}
	if b.staleAfter() != 27*time.Minute {
		t.Fatalf("stale after %v", b.staleAfter())
	}
}
