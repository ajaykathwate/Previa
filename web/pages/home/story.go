package home

import (
	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/web/components"
)

// turn is one exchange in the home page's "How it works" story: Kai asks,
// Previa answers with an example card, and the pipeline rail names the stage
// (the same stage names as the workspace Overview).
type turn struct {
	ID    string // anchor: #story-<ID>
	Stage string
	Hint  string // a few words under the stage name
	Ask   string
	Icon  templ.Component
	Reply templ.Component
	Card  templ.Component
}

func storyTurns() []turn {
	return []turn{
		{ID: "discover", Stage: "Discover", Hint: "Webhooks and Sync", Ask: "I just opened a pull request. Do you even notice?",
			Icon: components.PullRequestIcon(), Reply: replyDiscover(), Card: cardDiscover()},
		{ID: "gate", Stage: "Gate", Hint: "Checks before any work", Ask: "So when does it actually build?",
			Icon: components.ShieldIcon(), Reply: replyGate(), Card: cardGate()},
		{ID: "build", Stage: "Build", Hint: "Exact commit, rootless", Ask: "And then you build my Dockerfile?",
			Icon: components.BuildIcon(), Reply: replyBuild(), Card: cardBuild()},
		{ID: "run", Stage: "Run", Hint: "Isolated container", Ask: "Where does it run? Can it see my other services?",
			Icon: components.EnvironmentIcon(), Reply: replyRun(), Card: cardRun()},
		{ID: "route", Stage: "Route", Hint: "One stable HTTPS address", Ask: "How do reviewers open it?",
			Icon: components.LinkIcon(), Reply: replyRoute(), Card: cardRoute()},
		{ID: "report", Stage: "Report", Hint: "Comment and check run", Ask: "How will my team know it's ready?",
			Icon: components.MessageIcon(), Reply: replyReport(), Card: cardReport()},
		{ID: "teardown", Stage: "Teardown", Hint: "On merge or expiry", Ask: "And when the pull request is merged?",
			Icon: components.TrashIcon(), Reply: replyTeardown(), Card: cardTeardown()},
	}
}
