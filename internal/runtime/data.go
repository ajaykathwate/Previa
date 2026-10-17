package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/moby/moby/client"
)

// Persistent test data (docs/32-persistent-test-data.md). A candidate's SQL
// databases start from one source, tried in order: a copy of the data the
// pull request serves now (kept), the repository baseline, or empty. setup
// runs after every source; seed only on empty databases. When setup fails
// on copied data, or a copy fails, the candidate's database containers are
// restarted (their tmpfs starts empty again) and the next source is tried.
// The empty attempt decides: its failure is the build's.

// Starting-data sources, as recorded with the candidate's components.
const (
	SourceKept     = "kept"
	SourceBaseline = "baseline"
	SourceEmpty    = "empty"
)

// StartingData supplies a candidate's test-data plan and baseline dumps;
// the sampledata feature implements it.
type StartingData interface {
	// Plan reads the repository's Test data settings and current baselines.
	// It is read when the build starts, so a settings change during a build
	// applies to the next one.
	Plan(ctx context.Context, repoID int64) (DataPlan, error)
	// OpenBaseline opens a baseline's decrypted, decompressed dump. Reading
	// fails if the file is missing, corrupt or not this baseline's.
	OpenBaseline(ctx context.Context, repoID int64, b Baseline) (io.ReadCloser, error)
	// BaselineUnusable reports a baseline whose file could not be read: it is
	// marked missing and a refresh is scheduled.
	BaselineUnusable(ctx context.Context, repoID int64, b Baseline) error
}

// DataPlan is a repository's Test data settings and current baselines.
type DataPlan struct {
	Enabled, Keep, UseBaseline bool
	Baselines                  []Baseline
}

// Baseline is one current baseline dump of a SQL service.
type Baseline struct {
	ID                   int64
	Kind, Version        string
	Branch, CommitSHA    string
	StorageKey, Checksum string
	KeyVersion           int32
	Size                 int64
	BuiltAt              time.Time
}

// Deployments finds what a pull request serves now; *TargetStore implements
// it.
type Deployments interface {
	Serving(ctx context.Context, pullRequestID int64) (Serving, bool, error)
}

// Serving is the deployment a pull request serves now.
type Serving struct {
	ContainerID string // its app container
	HeadSHA     string
	Sequence    int32 // its deployment number; 0 when unrecorded
}

// dataPlan is what one candidate may start from, decided when its build
// started; reset is a Reset test data request's chosen source.
type dataPlan struct {
	DataPlan
	reset string
}

// serviceStart is where one SQL service of a candidate starts.
type serviceStart struct {
	run      serviceRun
	source   string
	from     string    // the serving candidate's service container, when kept
	baseline *Baseline // when the baseline
	note     string    // why a preferred source was skipped
}

// dataRecord is what the candidate's components record about its data.
type dataRecord struct {
	source string
	detail map[string]any
}

type fallback struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// sqlServices are the candidate's services whose data can start somewhere.
func (b backend) sqlServices() []serviceRun {
	var sql []serviceRun
	for _, s := range b.services {
		if s.Kind == "postgres" || s.Kind == "mysql" {
			sql = append(sql, s)
		}
	}
	return sql
}

// prepareData gives the candidate's databases their starting data and runs
// setup, and seed on empty databases, falling back source by source.
func (l *launch) prepareData(ctx context.Context) error {
	sql := l.plan.sqlServices()
	if len(sql) == 0 {
		return l.setUp(ctx)
	}
	serving, servingServices := l.servingServices(ctx)
	var fallbacks []fallback
	attempts := l.attempts(sql, servingServices)
	for i, attempt := range attempts {
		if i > 0 {
			l.logs.System("Restarting the databases so they start empty before trying " + sourceTitle(attempt.label) + ".")
			if err := l.restartServices(ctx); err != nil {
				return err
			}
		}
		last := i == len(attempts)-1
		if problem := l.load(ctx, attempt.starts, serving); problem != "" {
			fallbacks = append(fallbacks, fallback{Source: attempt.label, Reason: problem})
			if last {
				return failure(CodeUnavailable, "The databases could not be prepared.", true, nil)
			}
			continue
		}
		empty := attempt.label == SourceEmpty
		if err := l.setUp(ctx); err != nil {
			if !empty && codeOf(err) == CodeSetupFailed {
				l.logs.System("The setup command failed on the copied data; Previa tries the next starting data.")
				fallbacks = append(fallbacks, fallback{Source: attempt.label, Reason: "setup_failed"})
				continue
			}
			return err
		}
		if empty && len(l.plan.seed) > 0 {
			if err := l.seedData(ctx); err != nil {
				return err
			}
		}
		// Only a repository with Test data on records where its data started:
		// with it off, builds stay exactly as before (every database empty).
		if l.startPlan.Enabled {
			l.data = dataRecord{source: attempt.label, detail: attempt.detail(serving, fallbacks)}
		}
		return nil
	}
	return failure(CodeUnavailable, "The databases could not be prepared.", true, nil)
}

// dataAttempt is one try: where each SQL service starts, and the source it
// counts as (the most preferred one any service used).
type dataAttempt struct {
	label  string
	starts []serviceStart
}

// attempts lists the distinct tries in order: kept, baseline, empty. Each
// service takes the first source it can; a try identical to an earlier one
// is skipped, and the last is always every database empty, which decides.
func (l *launch) attempts(sql []serviceRun, serving map[string]servingService) []dataAttempt {
	baselines := map[string]*Baseline{}
	for i := range l.startPlan.Baselines {
		b := &l.startPlan.Baselines[i]
		baselines[b.Kind] = b
	}
	empty := dataAttempt{label: SourceEmpty}
	for _, s := range sql {
		empty.starts = append(empty.starts, serviceStart{run: s, source: SourceEmpty})
	}
	var attempts []dataAttempt
	for _, preferred := range l.startPlan.order() {
		var a dataAttempt
		for _, s := range sql {
			var from *servingService
			if f, ok := serving[s.Kind]; ok {
				from = &f
			}
			a.starts = append(a.starts, l.startPlan.startFor(s, preferred, from, baselines[s.Kind]))
		}
		a.label = a.preferred()
		if a.label == SourceEmpty || slices.ContainsFunc(attempts, a.same) {
			continue
		}
		attempts = append(attempts, a)
	}
	return append(attempts, empty)
}

// order is the sources tried before empty databases: a Reset's choice, or
// the settings' kept data and baseline.
func (p dataPlan) order() []string {
	switch {
	case p.reset == SourceEmpty:
		return nil
	case p.reset == SourceBaseline:
		return []string{SourceBaseline}
	case !p.Enabled:
		return nil
	}
	var order []string
	if p.Keep {
		order = append(order, SourceKept)
	}
	if p.UseBaseline {
		order = append(order, SourceBaseline)
	}
	return order
}

// startFor is where one SQL service starts in a try that prefers preferred:
// the serving candidate's data (from), else the baseline (b), else empty.
// Data moves only into the same or a newer version; a refused copy is noted.
func (p dataPlan) startFor(s serviceRun, preferred string, from *servingService, b *Baseline) serviceStart {
	start := serviceStart{run: s, source: SourceEmpty}
	if preferred == SourceKept && from != nil {
		if newerOrSame(s.Version, from.version) {
			start.source, start.from = SourceKept, from.id
			return start
		}
		start.note = "version_incompatible"
	}
	if b == nil || !p.UseBaseline && preferred != SourceBaseline {
		return start
	}
	if newerOrSame(s.Version, b.Version) {
		start.source, start.baseline = SourceBaseline, b
	} else if start.note == "" {
		start.note = "version_incompatible"
	}
	return start
}

// preferred is the most preferred source any service of the try uses.
func (a dataAttempt) preferred() string {
	label := SourceEmpty
	for _, s := range a.starts {
		switch s.source {
		case SourceKept:
			return SourceKept
		case SourceBaseline:
			label = SourceBaseline
		}
	}
	return label
}

func (a dataAttempt) same(b dataAttempt) bool {
	if len(a.starts) != len(b.starts) {
		return false
	}
	for i := range a.starts {
		if a.starts[i].source != b.starts[i].source {
			return false
		}
	}
	return true
}

// detail is the candidate's recorded data provenance; it names no data.
func (a dataAttempt) detail(serving Serving, fallbacks []fallback) map[string]any {
	services := make([]map[string]string, 0, len(a.starts))
	detail := map[string]any{}
	for _, s := range a.starts {
		entry := map[string]string{"kind": s.run.Kind, "version": s.run.Version, "source": s.source}
		if s.note != "" {
			entry["note"] = s.note
		}
		services = append(services, entry)
		switch s.source {
		case SourceKept:
			detail["kept"] = map[string]any{"sha": serving.HeadSHA, "deployment": serving.Sequence}
		case SourceBaseline:
			detail["baseline"] = map[string]any{"branch": s.baseline.Branch, "sha": s.baseline.CommitSHA, "built_at": s.baseline.BuiltAt.UTC().Format(time.RFC3339)}
		}
	}
	detail["services"] = services
	if len(fallbacks) > 0 {
		detail["fallbacks"] = fallbacks
	}
	return detail
}

// newerOrSame reports whether data from version from may be restored into
// version to: the same or a newer release, compared number by number.
func newerOrSame(to, from string) bool {
	a, b := strings.Split(to, "."), strings.Split(from, ".")
	for i := range max(len(a), len(b)) {
		x, y := 0, 0
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		if i < len(b) {
			y, _ = strconv.Atoi(b[i])
		}
		if x != y {
			return x > y
		}
	}
	return true
}

type servingService struct{ id, version string }

// servingServices finds the SQL services of the candidate the pull request
// serves now, by its app container's candidate label. Sleeping previews keep
// their services running.
func (l *launch) servingServices(ctx context.Context) (Serving, map[string]servingService) {
	plan := l.startPlan
	if l.r.deployments == nil || plan.reset != "" || !plan.Enabled || !plan.Keep {
		return Serving{}, nil
	}
	serving, ok, err := l.r.deployments.Serving(ctx, l.id.PullRequestID)
	if err != nil {
		l.r.logger.WarnContext(ctx, "find the serving deployment", "pr_id", l.id.PullRequestID, "error", err)
	}
	if !ok || err != nil {
		return Serving{}, nil
	}
	inspected, err := l.r.docker.ContainerInspect(ctx, serving.ContainerID, client.ContainerInspectOptions{})
	if err != nil || inspected.Container.Config == nil {
		return Serving{}, nil
	}
	labels := inspected.Container.Config.Labels
	candidateKey := labels[dockerx.LabelCandidate]
	if candidateKey == "" || candidateKey == l.plan.key || !l.r.ownedByPR(labels, l.id.PullRequestID, dockerx.KindPreview) {
		return Serving{}, nil
	}
	listed, err := l.r.docker.ContainerList(ctx, client.ContainerListOptions{Filters: client.Filters{}.Add("label",
		dockerx.LabelInstance+"="+l.r.instance, dockerx.LabelCandidate+"="+candidateKey, dockerx.LabelKind+"="+dockerx.KindService)})
	if err != nil {
		return Serving{}, nil
	}
	services := map[string]servingService{}
	for _, c := range listed.Items {
		kind := c.Labels[dockerx.LabelComponent]
		if (kind == "postgres" || kind == "mysql") && c.Labels[dockerx.LabelPR] == strconv.FormatInt(l.id.PullRequestID, 10) && c.State == "running" {
			services[kind] = servingService{id: c.ID, version: c.Labels[dockerx.LabelVersion]}
		}
	}
	return serving, services
}

// load copies or restores each service's starting data. It returns the
// reason the try failed (copy_failed, restore_failed or too_large), or "".
func (l *launch) load(ctx context.Context, starts []serviceStart, serving Serving) string {
	for _, s := range starts {
		title := s.run.profile.title + " " + s.run.Version
		started := l.r.now()
		switch s.source {
		case SourceKept:
			from := "the deployment that serves now"
			if serving.Sequence > 0 {
				from = fmt.Sprintf("Deployment %d (commit %s)", serving.Sequence, shortCommit(serving.HeadSHA))
			}
			l.logs.System(fmt.Sprintf("Copying the %s data of %s.", title, from))
			copied, problem := l.r.copyService(ctx, s.from, l.created[s.run.Kind], s.run.Kind)
			if problem != "" {
				l.logs.System(fmt.Sprintf("Copying the %s data failed (%s).", title, problemText(problem)))
				return problem
			}
			l.logs.System(fmt.Sprintf("Copied %s of %s data in %s. Changes made to the serving preview after the copy are not in this deployment.",
				bytesText(copied), title, l.r.now().Sub(started).Round(100*time.Millisecond)))
		case SourceBaseline:
			b := s.baseline
			l.logs.System(fmt.Sprintf("Restoring the %s repository baseline (branch %s, commit %s).", title, b.Branch, shortCommit(b.CommitSHA)))
			restored, problem := l.r.restoreBaseline(ctx, l.repoID, *b, l.created[s.run.Kind], s.run.Kind)
			if problem != "" {
				l.logs.System(fmt.Sprintf("Restoring the %s baseline failed (%s).", title, problemText(problem)))
				return problem
			}
			l.logs.System(fmt.Sprintf("Restored %s of %s baseline data in %s.", bytesText(restored), title, l.r.now().Sub(started).Round(100*time.Millisecond)))
		default:
			l.logs.System(fmt.Sprintf("%s starts empty.", title))
		}
	}
	return ""
}

// restartServices stops and starts every managed service of the candidate
// so their data (on tmpfs) starts empty again, then waits until they answer.
// Their credentials stay the same.
func (l *launch) restartServices(ctx context.Context) error {
	ids := map[string]string{}
	for _, s := range l.plan.services {
		timeout := 5
		if _, err := l.r.docker.ContainerStop(ctx, l.created[s.Kind], client.ContainerStopOptions{Timeout: &timeout}); err != nil {
			l.failed = s.Kind
			return l.r.interrupted(ctx, failure(CodeServiceFailed, fmt.Sprintf("%s %s could not be restarted.", s.profile.title, s.Version), true, err))
		}
		if _, err := l.r.docker.ContainerStart(ctx, l.created[s.Kind], client.ContainerStartOptions{}); err != nil {
			l.failed = s.Kind
			return l.r.interrupted(ctx, failure(CodeServiceFailed, fmt.Sprintf("%s %s could not be restarted.", s.profile.title, s.Version), true, err))
		}
		ids[s.Kind] = l.created[s.Kind]
	}
	if err := l.r.waitServicesReady(ctx, l.plan.services, ids, l.logs); err != nil {
		l.failed = l.failedService(ctx, ids)
		return err
	}
	return nil
}

// dumpCommand writes a service's database to standard output, run inside
// its own container with its own credentials from its environment: no
// credential leaves the container and nothing is written to its disk.
func dumpCommand(kind string) []string {
	if kind == "mysql" {
		return []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump --protocol=socket --socket=/tmp/mysql.sock -uroot ` +
			`--single-transaction --routines --triggers --events --no-tablespaces --hex-blob --set-gtid-purged=OFF "$MYSQL_DATABASE"`}
	}
	return []string{"sh", "-c", `PGPASSWORD="$POSTGRES_PASSWORD" exec pg_dump --format=custom --no-owner --no-privileges -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"`}
}

// restoreCommand loads a dump from standard input into a service's empty
// database, in one transaction where the database supports it.
func restoreCommand(kind string) []string {
	if kind == "mysql" {
		return []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=socket --socket=/tmp/mysql.sock -uroot "$MYSQL_DATABASE"`}
	}
	return []string{"sh", "-c", `PGPASSWORD="$POSTGRES_PASSWORD" exec pg_restore --no-owner --no-privileges --exit-on-error --single-transaction -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"`}
}

func (r *Runtime) copyTimeout() time.Duration {
	if r.policy.DataCopyTimeout > 0 {
		return r.policy.DataCopyTimeout
	}
	return 2 * time.Minute
}

// copyService streams a dump from one service container straight into
// another's restore, within the copy limit. It never writes the data to
// disk and opens no network path between the two candidates. It returns the
// bytes copied, or why it failed. Dump and restore output is never logged.
func (r *Runtime) copyService(ctx context.Context, fromID, toID, kind string) (int64, string) {
	copyCtx, cancel := context.WithTimeout(ctx, r.copyTimeout())
	defer cancel()
	out, err := dockerx.StreamOutput(copyCtx, r.docker, fromID, dumpCommand(kind)...)
	if err != nil {
		r.logger.WarnContext(ctx, "start data copy", "error", err)
		return 0, "copy_failed"
	}
	counted := &countingReader{r: out}
	restored, feedErr := dockerx.FeedInput(copyCtx, r.docker, toID, counted, restoreCommand(kind)...)
	dumped, waitErr := out.Wait(copyCtx)
	switch {
	case copyCtx.Err() != nil:
		return counted.n, "copy_failed"
	case waitErr != nil || dumped.ExitCode != 0:
		return counted.n, "copy_failed"
	case restored.ExitCode != 0:
		return counted.n, restoreProblem(restored)
	case feedErr != nil:
		return counted.n, "copy_failed"
	}
	return counted.n, ""
}

// restoreBaseline loads a baseline dump into a service container within the
// copy limit. A file that cannot be read is reported unusable, so a refresh
// rebuilds it.
func (r *Runtime) restoreBaseline(ctx context.Context, repoID int64, b Baseline, toID, kind string) (int64, string) {
	if r.startingData == nil {
		return 0, "restore_failed"
	}
	copyCtx, cancel := context.WithTimeout(ctx, r.copyTimeout())
	defer cancel()
	dump, err := r.startingData.OpenBaseline(copyCtx, repoID, b)
	if err != nil {
		r.unusable(ctx, repoID, b, err)
		return 0, "restore_failed"
	}
	defer func() { _ = dump.Close() }()
	counted := &countingReader{r: dump}
	restored, feedErr := dockerx.FeedInput(copyCtx, r.docker, toID, counted, restoreCommand(kind)...)
	switch {
	case counted.err != nil && !errors.Is(counted.err, io.EOF):
		r.unusable(ctx, repoID, b, counted.err)
		return counted.n, "restore_failed"
	case copyCtx.Err() != nil:
		return counted.n, "restore_failed"
	case restored.ExitCode != 0:
		return counted.n, restoreProblem(restored)
	case feedErr != nil:
		return counted.n, "restore_failed"
	}
	return counted.n, ""
}

func (r *Runtime) unusable(ctx context.Context, repoID int64, b Baseline, cause error) {
	r.logger.WarnContext(ctx, "baseline unusable", "repo_id", repoID, "baseline_id", b.ID, "error", cause)
	if err := r.startingData.BaselineUnusable(context.WithoutCancel(ctx), repoID, b); err != nil {
		r.logger.WarnContext(ctx, "report unusable baseline", "repo_id", repoID, "baseline_id", b.ID, "error", err)
	}
}

// restoreProblem tells a database that ran out of its tmpfs from any other
// restore failure; the restore's own output is never logged.
func restoreProblem(result dockerx.ExecResult) string {
	text := strings.ToLower(result.Stderr + " " + result.Stdout)
	for _, full := range []string{"no space left on device", "could not extend file", "is full", "errno: 28", "error 28"} {
		if strings.Contains(text, full) {
			return "too_large"
		}
	}
	return "restore_failed"
}

func problemText(problem string) string {
	switch problem {
	case "too_large":
		return "the data does not fit in the preview's database"
	case "copy_failed":
		return "the data could not be read in time"
	}
	return "the data could not be loaded"
}

func sourceTitle(source string) string {
	switch source {
	case SourceKept:
		return "the serving preview's data"
	case SourceBaseline:
		return "the repository baseline"
	}
	return "empty databases"
}

type countingReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil {
		c.err = err
	}
	return n, err
}

func shortCommit(sha string) string { return sha[:min(7, len(sha))] }

// bytesText is a size for people, such as "12.3 MB".
func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

func codeOf(err error) string {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Code
	}
	return ""
}
