package home

import (
	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/web/components"
)

// turn is one exchange in the home page's "How it works" story: Kai asks,
// Previa answers with an example card, and the rail names the step. IDs are
// stable anchors (#story-<ID>) even where the step's name has changed.
type turn struct {
	ID    string // anchor: #story-<ID>
	Stage string
	Hint  string // a few words under the step name
	Ask   string
	Icon  templ.Component
	Reply templ.Component
	Card  templ.Component
}

func storyTurns() []turn {
	return []turn{
		{ID: "discover", Stage: "Notice", Hint: "You open a pull request", Ask: "I just opened a pull request. Do you even notice?",
			Icon: components.PullRequestIcon(), Reply: replyDiscover(), Card: cardDiscover()},
		{ID: "gate", Stage: "Check", Hint: "Only when it should", Ask: "So when does it actually build?",
			Icon: components.ShieldIcon(), Reply: replyGate(), Card: cardGate()},
		{ID: "build", Stage: "Build", Hint: "Your Dockerfile, that commit", Ask: "And then you build my Dockerfile?",
			Icon: components.BuildIcon(), Reply: replyBuild(), Card: cardBuild()},
		{ID: "run", Stage: "Run", Hint: "In its own space", Ask: "Where does it run? Can it see my other services?",
			Icon: components.EnvironmentIcon(), Reply: replyRun(), Card: cardRun()},
		{ID: "route", Stage: "Share", Hint: "One private HTTPS link", Ask: "How do reviewers open it?",
			Icon: components.LinkIcon(), Reply: replyRoute(), Card: cardRoute()},
		{ID: "report", Stage: "Report", Hint: "Comment and check", Ask: "How will my team know it's ready?",
			Icon: components.MessageIcon(), Reply: replyReport(), Card: cardReport()},
		{ID: "teardown", Stage: "Clean up", Hint: "On merge or expiry", Ask: "And when the pull request is merged?",
			Icon: components.TrashIcon(), Reply: replyTeardown(), Card: cardTeardown()},
	}
}
