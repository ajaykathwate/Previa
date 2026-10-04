package realtime

import (
	"testing"
)

func next(t *testing.T, sub *Subscription) (Event, bool) {
	t.Helper()
	select {
	case <-sub.Ready():
	default:
		return Event{}, false
	}
	return sub.Next()
}

func TestHubRoutesByTopicAndCoalescesBursts(t *testing.T) {
	h := NewHub()
	one := h.Subscribe("repository:1", Cursor{})
	two := h.Subscribe("repository:2", Cursor{})
	defer one.Close()
	defer two.Close()
	h.Publish("repository:1", "pull_requests")
	h.Publish("repository:1", "pr_syncs")
	h.Publish("repository:1", "pull_requests")
	e, ok := next(t, one)
	if !ok || e.Kind != KindChange || e.Topic != "repository:1" || e.Cursor != h.Cursor() {
		t.Fatalf("coalesced event = %+v, %v; want newest change", e, ok)
	}
	if _, ok := next(t, one); ok {
		t.Fatal("burst delivered more than one pending event")
	}
	if _, ok := next(t, two); ok {
		t.Fatal("other topic received an event")
	}
}

func TestHubResyncIsNeverDowngraded(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("repository:1", Cursor{})
	defer sub.Close()
	h.Resync()
	h.Publish("repository:1", "pull_requests")
	if e, ok := next(t, sub); !ok || e.Kind != KindResync || e.Topic != "repository:1" {
		t.Fatalf("event = %+v, %v; want resync", e, ok)
	}
}

func TestHubSubscribeReplaysFromCursor(t *testing.T) {
	h := NewHub()
	start := h.Cursor()
	h.Publish("repository:2", "pull_requests")
	mid := h.Cursor()
	h.Publish("repository:1", "pull_requests")

	for _, tt := range []struct {
		name  string
		since Cursor
		want  Kind
	}{
		{"missed change for topic", start, KindChange},
		{"up to date", h.Cursor(), ""},
		{"only other topics missed", mid, KindChange},
		{"other process", Cursor{Instance: "elsewhere", Seq: 1}, KindResync},
		{"future cursor", Cursor{Instance: h.instance, Seq: 99}, KindResync},
		{"zero cursor starts now", Cursor{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sub := h.Subscribe("repository:1", tt.since)
			defer sub.Close()
			e, ok := next(t, sub)
			if tt.want == "" {
				if ok {
					t.Fatalf("unexpected catch-up %+v", e)
				}
				return
			}
			if !ok || e.Kind != tt.want || e.Cursor != h.Cursor() {
				t.Fatalf("catch-up = %+v, %v; want %s at %v", e, ok, tt.want, h.Cursor())
			}
		})
	}

	after := h.Cursor()
	h.Publish("repository:2", "pull_requests")
	if sub := h.Subscribe("repository:1", after); sub != nil {
		if e, ok := next(t, sub); ok {
			t.Fatalf("change on another topic replayed as %+v", e)
		}
		sub.Close()
	}
}

func TestHubResyncsWhenHistoryCannotCoverGapOrMarksResync(t *testing.T) {
	h := NewHub()
	old := h.Cursor()
	for range historySize + 5 {
		h.Publish("repository:9", "pull_requests")
	}
	sub := h.Subscribe("repository:1", old)
	if e, ok := next(t, sub); !ok || e.Kind != KindResync {
		t.Fatalf("gap beyond history = %+v, %v; want resync", e, ok)
	}
	sub.Close()

	before := h.Cursor()
	h.Resync()
	sub = h.Subscribe("repository:1", before)
	if e, ok := next(t, sub); !ok || e.Kind != KindResync {
		t.Fatalf("resync marker = %+v, %v; want resync", e, ok)
	}
	sub.Close()
}

func TestHubCloseEndsSubscriptions(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe("repository:1", Cursor{})
	h.Publish("repository:1", "pull_requests")
	h.Close()
	if e, ok := sub.Next(); !ok || e.Kind != KindChange {
		t.Fatal("pending event lost on close")
	}
	for range sub.Ready() { // Drain the wake-up left by Publish; the channel must then close.
	}
	sub.Close()
	late := h.Subscribe("repository:1", Cursor{})
	if _, open := <-late.Ready(); open {
		t.Fatal("subscription after close should start closed")
	}
	h.Publish("repository:1", "pull_requests")
}

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{Instance: "abc123", Seq: 42}
	got, ok := ParseCursor(c.String())
	if !ok || got != c {
		t.Fatalf("round trip = %+v, %v", got, ok)
	}
	for _, bad := range []string{"", "abc", ".1", "abc.x", "abc.-1"} {
		if _, ok := ParseCursor(bad); ok {
			t.Errorf("ParseCursor(%q) accepted", bad)
		}
	}
}
