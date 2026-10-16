package dashboard

import (
	"strings"
	"testing"
)

func TestSetupChecklistFollowsTheJourneyForEachRole(t *testing.T) {
	var s Summary
	steps := s.Setup()
	if s.SetupDone() || len(steps) != 4 || steps[0].Done || steps[0].Title != "Get access to a repository" || steps[0].Href != "" {
		t.Fatalf("a Developer without repositories is given access, not asked to connect: %+v", steps)
	}
	s.Journey.Managers = []Person{{Login: "alice", Name: "Alice Smith"}, {Login: "bo", Name: "bo"}}
	if detail := s.Setup()[0].Detail; !strings.Contains(detail, "Alice Smith (@alice) or bo (@bo)") {
		t.Fatalf("the Developer learns whom to ask: %q", detail)
	}
	s.Journey = Journey{Manager: true}
	if first := s.Setup()[0]; first.Title != "Connect a repository" || first.Href != "/repositories" {
		t.Fatalf("Owners and Admins connect code: %+v", first)
	}
	if s.Setup()[1].Href != "/repositories?access=available" {
		t.Fatal("with no repository to continue in, the steps lead to the directory")
	}
	s.Journey = Journey{Manager: true, Repositories: 2, Available: 2, FocusID: 7, FocusName: "acme/api"}
	steps = s.Setup()
	if !steps[0].Done || steps[1].Done || steps[1].Href != "/repositories/7/setup" || steps[1].Action != "Open setup" || !strings.Contains(steps[1].Detail, "acme/api") {
		t.Fatalf("setup continues on the repository's Setup tab: %+v", steps[1])
	}
	s.Journey.Checked, s.Journey.Published = true, true
	if s.SetupDone() {
		t.Fatal("a published preview nobody opened from Previa is not the end of setup")
	}
	s.Journey.Opened = true
	if !s.SetupDone() {
		t.Fatal("every step done: the checklist goes away")
	}
	if refs := s.PublicReferences(); len(refs) == 0 || refs[0].Kind != "repository" || refs[0].ID != 7 {
		t.Fatalf("the Setup link's repository gets a public address: %+v", refs)
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
