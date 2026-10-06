package faq

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

var anchor = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestEveryQuestionIsAnsweredFiledAndLinkable(t *testing.T) {
	topics := map[string]bool{}
	for _, topic := range Topics() {
		if topic.ID == "" || topic.Title == "" || topic.Short == "" || topics[topic.ID] {
			t.Fatalf("invalid or duplicate topic %+v", topic)
		}
		topics[topic.ID] = true
		if len(InTopic(topic.ID)) == 0 {
			t.Errorf("topic %q has no questions", topic.ID)
		}
	}
	seen := map[string]bool{}
	for _, q := range Questions() {
		if !anchor.MatchString(q.ID) || seen[q.ID] || !topics[q.Topic] || q.Ask == "" || q.Answer == nil {
			t.Errorf("question %q: needs a unique anchor, a known topic, a question and an answer", q.ID)
		}
		seen[q.ID] = true
	}

	var page strings.Builder
	if err := Conversation(Guide).Render(context.Background(), &page); err != nil {
		t.Fatal(err)
	}
	for id := range seen {
		if n := strings.Count(page.String(), `id="faq-`+id+`"`); n != 1 {
			t.Errorf("question %q rendered %d times", id, n)
		}
	}
	if !strings.Contains(page.String(), `data-faq-filter="all" aria-pressed="true"`) {
		t.Error("the All topics filter must start pressed")
	}
}
