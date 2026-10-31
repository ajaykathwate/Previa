package runtime

import (
	"slices"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

func sqlRun(kind, version string) serviceRun {
	return serviceRun{BackendService: repoconfig.BackendService{Kind: kind, Version: version}, profile: serviceProfiles[kind]}
}

// labels lists each try's source and, per service, where it starts.
func labels(attempts []dataAttempt) [][]string {
	var out [][]string
	for _, a := range attempts {
		row := []string{a.label}
		for _, s := range a.starts {
			row = append(row, s.run.Kind+":"+s.source)
		}
		out = append(out, row)
	}
	return out
}

func TestStartingDataTriesKeptThenBaselineThenEmpty(t *testing.T) {
	pg := sqlRun("postgres", "17")
	my := sqlRun("mysql", "8.4")
	baselines := []Baseline{{ID: 1, Kind: "postgres", Version: "17", Branch: "main", CommitSHA: "abc", BuiltAt: time.Now()}}
	serving := map[string]servingService{"postgres": {id: "old-pg", version: "17"}}
	for _, tc := range []struct {
		name    string
		plan    dataPlan
		sql     []serviceRun
		serving map[string]servingService
		want    [][]string
	}{
		{"Test data off: empty only", dataPlan{}, []serviceRun{pg}, serving, [][]string{{"empty", "postgres:empty"}}},
		{"keep, baseline, empty", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, UseBaseline: true, Baselines: baselines}}, []serviceRun{pg}, serving,
			[][]string{{"kept", "postgres:kept"}, {"baseline", "postgres:baseline"}, {"empty", "postgres:empty"}}},
		{"nothing serves: baseline, empty", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, UseBaseline: true, Baselines: baselines}}, []serviceRun{pg}, nil,
			[][]string{{"baseline", "postgres:baseline"}, {"empty", "postgres:empty"}}},
		{"keep off", dataPlan{DataPlan: DataPlan{Enabled: true, UseBaseline: true, Baselines: baselines}}, []serviceRun{pg}, serving,
			[][]string{{"baseline", "postgres:baseline"}, {"empty", "postgres:empty"}}},
		{"baseline off", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, Baselines: baselines}}, []serviceRun{pg}, serving,
			[][]string{{"kept", "postgres:kept"}, {"empty", "postgres:empty"}}},
		{"no baseline yet", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, UseBaseline: true}}, []serviceRun{pg}, serving,
			[][]string{{"kept", "postgres:kept"}, {"empty", "postgres:empty"}}},
		{"a downgraded service is not kept", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, UseBaseline: true, Baselines: baselines}}, []serviceRun{sqlRun("postgres", "16")},
			serving, [][]string{{"empty", "postgres:empty"}}},
		{"an added service starts from the baseline beside kept data", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, UseBaseline: true,
			Baselines: append(slices.Clone(baselines), Baseline{ID: 2, Kind: "mysql", Version: "8.4"})}}, []serviceRun{my, pg}, serving,
			[][]string{{"kept", "mysql:baseline", "postgres:kept"}, {"baseline", "mysql:baseline", "postgres:baseline"}, {"empty", "mysql:empty", "postgres:empty"}}},
		{"Reset to the baseline never keeps", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, UseBaseline: true, Baselines: baselines}, reset: SourceBaseline}, []serviceRun{pg}, serving,
			[][]string{{"baseline", "postgres:baseline"}, {"empty", "postgres:empty"}}},
		{"Reset to empty and seed", dataPlan{DataPlan: DataPlan{Enabled: true, Keep: true, UseBaseline: true, Baselines: baselines}, reset: SourceEmpty}, []serviceRun{pg}, serving,
			[][]string{{"empty", "postgres:empty"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &launch{startPlan: tc.plan}
			got := labels(l.attempts(tc.sql, tc.serving))
			if !slices.EqualFunc(got, tc.want, slices.Equal[[]string]) {
				t.Fatalf("attempts %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDataMovesOnlyToTheSameOrANewerVersion(t *testing.T) {
	for _, tc := range []struct {
		to, from string
		ok       bool
	}{
		{"17", "17", true}, {"17", "16", true}, {"16", "17", false},
		{"8.4", "8.4", true}, {"8.4", "8.0", true}, {"8.0", "8.4", false}, {"9", "8.4", true},
	} {
		if got := newerOrSame(tc.to, tc.from); got != tc.ok {
			t.Errorf("%s from %s: %v", tc.to, tc.from, got)
		}
	}
}

func TestRecordedDetailNamesSourcesNotData(t *testing.T) {
	b := &Baseline{Branch: "main", CommitSHA: "abc1234", BuiltAt: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}
	a := dataAttempt{label: SourceKept, starts: []serviceStart{
		{run: sqlRun("postgres", "17"), source: SourceKept},
		{run: sqlRun("mysql", "8.4"), source: SourceBaseline, baseline: b, note: "version_incompatible"},
	}}
	detail := a.detail(Serving{HeadSHA: "def5678", Sequence: 3}, []fallback{{Source: SourceKept, Reason: "setup_failed"}})
	if detail["kept"].(map[string]any)["deployment"] != int32(3) || detail["baseline"].(map[string]any)["branch"] != "main" {
		t.Fatalf("detail %v", detail)
	}
	services := detail["services"].([]map[string]string)
	if len(services) != 2 || services[1]["note"] != "version_incompatible" || detail["fallbacks"].([]fallback)[0].Reason != "setup_failed" {
		t.Fatalf("services %v fallbacks %v", services, detail["fallbacks"])
	}
}

func TestRestoreProblemsAreClassifiedWithoutShowingOutput(t *testing.T) {
	if restoreProblem(execResult("pg_restore: error: could not extend file \"base/1/2\": No space left on device")) != "too_large" {
		t.Fatal("a full tmpfs is too_large")
	}
	if restoreProblem(execResult("ERROR 1114 (HY000) at line 40: The table 'orders' is full")) != "too_large" {
		t.Fatal("a full MySQL table is too_large")
	}
	if restoreProblem(execResult("pg_restore: error: input file does not appear to be a valid archive")) != "restore_failed" {
		t.Fatal("anything else is restore_failed")
	}
}

func execResult(stderr string) dockerx.ExecResult {
	return dockerx.ExecResult{Stderr: stderr, ExitCode: 1}
}
