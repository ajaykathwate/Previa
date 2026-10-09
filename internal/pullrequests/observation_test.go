package pullrequests

import (
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/pullrequests/dbgen"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestJudgeObservationOrdersObservations(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	recorded := dbgen.PullRequest{HeadSha: "aaa", State: "open", SourceUpdatedAt: pgtype.Timestamptz{Time: at, Valid: true}}
	legacy := dbgen.PullRequest{HeadSha: "aaa", State: "open"} // from before source timestamps
	fenced := recorded
	fenced.NeedsRefresh = true
	observe := func(sha, state string, updated time.Time) Observation {
		return Observation{SHA: sha, State: state, UpdatedAt: updated}
	}
	for name, tt := range map[string]struct {
		old     dbgen.PullRequest
		exists  bool
		o       Observation
		webhook bool
		want    verdict
	}{
		"a new open PR is recorded":                         {exists: false, o: observe("aaa", "open", at), webhook: true, want: applyObservation},
		"a close of an untracked PR is ignored":             {exists: false, o: observe("aaa", "closed", at), webhook: true, want: ignoreObservation},
		"an older observation is ignored":                   {old: recorded, exists: true, o: observe("bbb", "open", at.Add(-time.Second)), webhook: true, want: ignoreObservation},
		"a newer push is applied":                           {old: recorded, exists: true, o: observe("bbb", "open", at.Add(time.Second)), webhook: true, want: applyObservation},
		"a same-time conflicting push is fenced":            {old: recorded, exists: true, o: observe("bbb", "open", at), webhook: true, want: fenceObservation},
		"a same-time close of an open PR is applied":        {old: recorded, exists: true, o: observe("aaa", "closed", at), webhook: true, want: applyObservation},
		"a same-time agreeing webhook on a fenced PR waits": {old: fenced, exists: true, o: observe("aaa", "open", at), webhook: true, want: ignoreObservation},
		"a same-time snapshot is applied":                   {old: recorded, exists: true, o: observe("bbb", "open", at), webhook: false, want: applyObservation},
		"a conflicting webhook on a legacy row is fenced":   {old: legacy, exists: true, o: observe("bbb", "open", at), webhook: true, want: fenceObservation},
		"a webhook closing a legacy open row is applied":    {old: legacy, exists: true, o: observe("aaa", "closed", at), webhook: true, want: applyObservation},
		"a snapshot of a legacy row is its baseline":        {old: legacy, exists: true, o: observe("bbb", "open", at), webhook: false, want: applyObservation},
		"an agreeing webhook on a legacy row is applied":    {old: legacy, exists: true, o: observe("aaa", "open", at), webhook: true, want: applyObservation},
	} {
		if got := judgeObservation(tt.old, tt.exists, tt.o, tt.webhook); got != tt.want {
			t.Errorf("%s: verdict %d, want %d", name, got, tt.want)
		}
	}
}
