// Package faq renders the questions people ask about Previa as a conversation:
// the person asking on one side, Previa answering on the other. The same
// content appears on the public home page and on the workspace FAQ page. It is
// static copy describing what works today, at the level a user needs; it
// reads no saved state.
package faq

import "github.com/a-h/templ"

// Asker is who asks the questions: Kai, a guide, on the public home page, and
// the signed-in owner (you) in the workspace.
type Asker struct {
	Initial string // the avatar letter
	You     bool   // use the workspace account avatar style
}

// Guide is the asker on the public home page.
var Guide = Asker{Initial: "K"}

// Owner is the asker in the workspace: the signed-in user.
func Owner(initial string) Asker { return Asker{Initial: initial, You: true} }

// Topic groups related questions; filters and headings follow this order.
// Short labels the topic's filter button.
type Topic struct {
	ID, Title, Short string
}

// Question is one entry. Its anchor is #faq-<ID>; Answer is Previa's reply.
type Question struct {
	ID, Topic, Ask string
	Answer         templ.Component
}

// Topics lists the topics in reading order.
func Topics() []Topic {
	return []Topic{
		{ID: "start", Title: "Getting started", Short: "Getting started"},
		{ID: "builds", Title: "Builds and previews", Short: "Builds"},
		{ID: "security", Title: "Security and secrets", Short: "Security"},
		{ID: "data", Title: "Your code and data", Short: "Your data"},
	}
}

// Questions lists every question in reading order, grouped by topic.
func Questions() []Question {
	return []Question{
		{ID: "what-is-previa", Topic: "start", Ask: "What is Previa, in one sentence?", Answer: whatIsPrevia()},
		{ID: "is-it-free", Topic: "start", Ask: "Is Previa free?", Answer: isItFree()},
		{ID: "repository-files", Topic: "start", Ask: "What does my repository need?", Answer: repositoryFiles()},
		{ID: "languages", Topic: "start", Ask: "Which languages and frameworks work?", Answer: languages()},
		{ID: "sign-in-access", Topic: "start", Ask: "Does signing in give you access to my code?", Answer: signInAccess()},
		{ID: "connect-repository", Topic: "start", Ask: "How do I connect a repository?", Answer: connectRepository()},
		{ID: "teams", Topic: "start", Ask: "Can my team share Previa?", Answer: teams()},

		{ID: "when-it-builds", Topic: "builds", Ask: "When does a preview get built?", Answer: whenItBuilds()},
		{ID: "preview-link", Topic: "builds", Ask: "Where do I find the preview link?", Answer: previewLink()},
		{ID: "review-feedback", Topic: "builds", Ask: "How do reviewers give feedback?", Answer: reviewFeedback()},
		{ID: "new-commit", Topic: "builds", Ask: "What happens when I push a new commit?", Answer: newCommit()},
		{ID: "preview-lifetime", Topic: "builds", Ask: "How long does a preview live?", Answer: previewLifetime()},
		{ID: "backend-apps", Topic: "builds", Ask: "Does it work for APIs and full-stack apps?", Answer: backendApps()},
		{ID: "build-failed", Topic: "builds", Ask: "What if a build fails?", Answer: buildFailed()},
		{ID: "build-disabled", Topic: "builds", Ask: "Why can't I build a pull request?", Answer: buildDisabled()},
		{ID: "waiting-for-capacity", Topic: "builds", Ask: "What does “Waiting for capacity” mean?", Answer: waitingForCapacity()},

		{ID: "fork-pull-requests", Topic: "security", Ask: "Is it safe to build pull requests from forks?", Answer: forkPullRequests()},
		{ID: "secrets", Topic: "security", Ask: "How are repository secrets protected?", Answer: secretsProtected()},
		{ID: "internal-network", Topic: "security", Ask: "Can a preview reach my private systems?", Answer: internalNetwork()},
		{ID: "who-can-open", Topic: "security", Ask: "Who can open a preview?", Answer: whoCanOpen()},
		{ID: "dashboard-safety", Topic: "security", Ask: "Could a preview reach my Previa account?", Answer: dashboardSafety()},

		{ID: "where-code-goes", Topic: "data", Ask: "What happens to my code?", Answer: whereCodeGoes()},
		{ID: "interrupted-build", Topic: "data", Ask: "What if something goes wrong in the middle of a build?", Answer: interruptedBuild()},
		{ID: "build-history", Topic: "data", Ask: "Do you keep a history of builds?", Answer: buildHistory()},
	}
}

// InTopic returns the questions of one topic, in reading order.
func InTopic(topic string) []Question {
	var questions []Question
	for _, q := range Questions() {
		if q.Topic == topic {
			questions = append(questions, q)
		}
	}
	return questions
}
