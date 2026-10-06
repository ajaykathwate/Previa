package dashboard

import (
	"strings"
	"testing"

	prpages "github.com/ajaykathwatee/previa/web/pages/pullrequests"
)

func TestSetupChecklistFollowsRealState(t *testing.T) {
	var s Summary
	if s.SetupDone() || len(s.Setup()) != 5 || !s.Setup()[0].Done {
		t.Fatal("a new workspace shows the checklist, with sign-in done")
	}
	s = Summary{Installations: 1, Repositories: 2, OpenPRs: 3}
	if s.SetupDone() {
		t.Fatal("no build yet: the checklist stays")
	}
	s.Builds = []prpages.ActivityRow{{Number: 1}}
	if !s.SetupDone() {
		t.Fatal("every step done: the checklist goes away")
	}
}

func TestWorkerNoteSaysWhatFixesBuilding(t *testing.T) {
	for workers, want := range map[Workers]string{
		{Execution: true, Discovery: true}: "builds, publishes and reports",
		{Paused: true, Discovery: true}:    "BUILDS_PAUSED",
		{Discovery: true, Reporting: true}: "PREVIEW_BASE_DOMAIN",
		{}:                                 "make run-worker",
	} {
		note, attention := Summary{Workers: workers}.WorkerNote()
		if !strings.Contains(note, want) || attention == workers.Execution {
			t.Errorf("%+v: %q (attention %v)", workers, note, attention)
		}
	}
}
