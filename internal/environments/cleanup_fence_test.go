package environments

import "testing"

func TestCleanupFenceAllowsOnlyUnreferencedSettledResources(t *testing.T) {
	current := Request{PullRequestID: 7, Generation: 3, Revision: 2, State: StateOpen}
	building := cleanupFence{current: current, intentState: string(IntentRunning), keep: []Artifact{{ContainerID: "active", ImageTag: "active:tag"}}}
	settled := building
	settled.intentState = string(IntentSucceeded)
	older := Request{PullRequestID: 7, Generation: 3, Revision: 1, State: StateOpen}
	resource := func(pr, generation, revision int64, container string) Resource {
		return Resource{PullRequestID: pr, Generation: generation, Revision: revision, Artifact: Artifact{ContainerID: container}}
	}
	for name, tt := range map[string]struct {
		fence       cleanupFence
		resource    Resource
		scheduledBy Request
		want        bool
	}{
		"an older unreferenced candidate goes":        {building, resource(7, 2, 5, "old"), older, true},
		"another PR's resource stays":                 {building, resource(8, 2, 5, "old"), older, false},
		"a resource with invalid labels stays":        {building, resource(7, -1, 0, "old"), older, false},
		"a newer generation's resource stays":         {building, resource(7, 4, 0, "new"), older, false},
		"a newer revision's resource stays":           {building, resource(7, 3, 3, "new"), older, false},
		"the active preview stays":                    {building, resource(7, 2, 5, "active"), older, false},
		"the running build's own resource stays":      {building, resource(7, 3, 2, "candidate"), older, false},
		"the build may clean up its own resource":     {building, resource(7, 3, 2, "candidate"), current, true},
		"a finished build's leftover goes":            {settled, resource(7, 3, 2, "candidate"), older, true},
		"a network stays while a kept container runs": {building, Resource{PullRequestID: 7, Generation: 2, Network: "net"}, older, false},
		"a network goes once nothing is kept":         {cleanupFence{current: current, intentState: string(IntentSucceeded)}, Resource{PullRequestID: 7, Generation: 2, Network: "net"}, older, true},
	} {
		if got := tt.fence.allows(tt.resource, tt.scheduledBy); got != tt.want {
			t.Errorf("%s: allows = %v, want %v", name, got, tt.want)
		}
	}
}

func TestIntentStateMachine(t *testing.T) {
	states := []IntentState{IntentPending, IntentQueued, IntentRunning, IntentSucceeded, IntentFailed, IntentCancelled, IntentSuperseded}
	allowed := map[[2]IntentState]bool{
		{IntentPending, IntentQueued}:    true,
		{IntentQueued, IntentRunning}:    true,
		{IntentRunning, IntentSucceeded}: true,
		{IntentRunning, IntentFailed}:    true,
	}
	for _, from := range states {
		for _, to := range states {
			if got := transitionAllowed(from, to); got != allowed[[2]IntentState{from, to}] {
				t.Errorf("%s -> %s: allowed = %v", from, to, got)
			}
		}
	}
}

func TestAutomaticIntentNeedsAutomaticModeUntilItRuns(t *testing.T) {
	for name, tt := range map[string]struct {
		intent BuildIntent
		mode   string
		want   bool
	}{
		"a manual request in Manual mode":          {BuildIntent{Source: modeManual, State: IntentQueued}, modeManual, true},
		"an automatic request in Automatic mode":   {BuildIntent{Source: modeAutomatic, State: IntentPending}, modeAutomatic, true},
		"an automatic request after a switch":      {BuildIntent{Source: modeAutomatic, State: IntentQueued}, modeManual, false},
		"a running automatic build after a switch": {BuildIntent{Source: modeAutomatic, State: IntentRunning}, modeManual, true},
	} {
		if got := tt.intent.allowedIn(tt.mode); got != tt.want {
			t.Errorf("%s: allowedIn = %v, want %v", name, got, tt.want)
		}
	}
}
