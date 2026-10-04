package realtime

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
)

// Kind distinguishes a topic change from a request to refetch everything.
type Kind string

const (
	// KindChange means committed state for the event's topic changed.
	KindChange Kind = "change"
	// KindResync means events may have been missed; clients should refetch.
	KindResync Kind = "resync"
)

// Event is one invalidation signal delivered to a subscriber.
type Event struct {
	Kind   Kind
	Topic  string
	Source string
	Cursor Cursor
}

// Cursor identifies a position in one Hub's event sequence. Instance changes on
// every process start, so a cursor from another process always forces a resync.
type Cursor struct {
	Instance string
	Seq      uint64
}

// String encodes the cursor for SSE ids and query parameters.
func (c Cursor) String() string {
	if c.Instance == "" {
		return ""
	}
	return c.Instance + "." + strconv.FormatUint(c.Seq, 10)
}

// ParseCursor decodes a value produced by Cursor.String.
func ParseCursor(value string) (Cursor, bool) {
	instance, seq, found := strings.Cut(value, ".")
	if !found || instance == "" || len(instance) > 64 {
		return Cursor{}, false
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return Cursor{}, false
	}
	return Cursor{Instance: instance, Seq: n}, true
}

// historySize bounds how far back a reconnecting subscriber can resume.
const historySize = 1024

type entry struct {
	seq   uint64
	topic string // empty marks a resync point that matches every topic
}

// Hub fans events out to in-process subscribers and remembers recent topics so
// subscribers can resume from a cursor without missing a change.
type Hub struct {
	instance string
	mu       sync.Mutex
	seq      uint64
	history  []entry // ring buffer, oldest at next when full
	next     int
	subs     map[string]map[*Subscription]struct{}
	closed   bool
}

// NewHub returns an empty hub with a random instance identity.
func NewHub() *Hub {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("realtime: read random instance id: " + err.Error())
	}
	return &Hub{instance: hex.EncodeToString(b), history: make([]entry, 0, historySize), subs: map[string]map[*Subscription]struct{}{}}
}

// Cursor returns the current position. Read it before loading the state a page
// renders, so any later commit produces an event after this cursor.
func (h *Hub) Cursor() Cursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Cursor{Instance: h.instance, Seq: h.seq}
}

// Publish records a change for topic and wakes its subscribers.
func (h *Hub) Publish(topic, source string) {
	if topic == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	event := h.record(topic, KindChange, source)
	for sub := range h.subs[topic] {
		sub.offer(event)
	}
}

// Resync tells every subscriber to refetch, for example after the database
// listener reconnects and notifications may have been lost.
func (h *Hub) Resync() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	event := h.record("", KindResync, "")
	for topic, subs := range h.subs {
		e := event
		e.Topic = topic
		for sub := range subs {
			sub.offer(e)
		}
	}
}

func (h *Hub) record(topic string, kind Kind, source string) Event {
	h.seq++
	e := entry{seq: h.seq, topic: topic}
	if len(h.history) < historySize {
		h.history = append(h.history, e)
	} else {
		h.history[h.next] = e
		h.next = (h.next + 1) % historySize
	}
	return Event{Kind: kind, Topic: topic, Source: source, Cursor: Cursor{Instance: h.instance, Seq: h.seq}}
}

// Subscribe registers for topic. A valid since cursor replays a missed change
// (or a resync when the gap cannot be proven) as the first event; the zero
// cursor starts from now.
func (h *Hub) Subscribe(topic string, since Cursor) *Subscription {
	sub := &Subscription{hub: h, topic: topic, signal: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	sub.start = Cursor{Instance: h.instance, Seq: h.seq}
	if h.closed {
		sub.done = true
		close(sub.signal)
		return sub
	}
	if since != (Cursor{}) {
		if kind, ok := h.missed(topic, since); ok {
			sub.offer(Event{Kind: kind, Topic: topic, Cursor: Cursor{Instance: h.instance, Seq: h.seq}})
		}
	}
	if h.subs[topic] == nil {
		h.subs[topic] = map[*Subscription]struct{}{}
	}
	h.subs[topic][sub] = struct{}{}
	return sub
}

// missed reports what a subscriber at since needs to catch up, if anything.
func (h *Hub) missed(topic string, since Cursor) (Kind, bool) {
	if since.Instance != h.instance || since.Seq > h.seq {
		return KindResync, true
	}
	if since.Seq == h.seq {
		return "", false
	}
	oldest := h.seq - uint64(len(h.history)) + 1
	if since.Seq+1 < oldest {
		return KindResync, true
	}
	kind, found := Kind(""), false
	for _, e := range h.history {
		if e.seq <= since.Seq {
			continue
		}
		if e.topic == "" {
			return KindResync, true
		}
		if e.topic == topic {
			kind, found = KindChange, true
		}
	}
	return kind, found
}

// Close ends every subscription; later subscriptions start closed. Register it
// with http.Server.RegisterOnShutdown so streaming handlers return promptly.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, subs := range h.subs {
		for sub := range subs {
			sub.finish()
		}
	}
	h.subs = nil
}

// Subscription receives coalesced events for one topic. Bursts collapse into a
// single pending event carrying the newest cursor; a resync is never downgraded.
type Subscription struct {
	hub     *Hub
	topic   string
	start   Cursor
	signal  chan struct{}
	pending *Event
	done    bool
}

// Start is the hub position at which the subscription began.
func (s *Subscription) Start() Cursor { return s.start }

// Ready is signalled when Next has an event, and closed when the subscription ends.
func (s *Subscription) Ready() <-chan struct{} { return s.signal }

// Next returns the pending event, if any.
func (s *Subscription) Next() (Event, bool) {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.pending == nil {
		return Event{}, false
	}
	e := *s.pending
	s.pending = nil
	return e, true
}

// Close unregisters the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if subs := s.hub.subs[s.topic]; subs != nil {
		delete(subs, s)
		if len(subs) == 0 {
			delete(s.hub.subs, s.topic)
		}
	}
	s.finish()
}

// offer and finish require the hub lock.
func (s *Subscription) offer(e Event) {
	if s.done {
		return
	}
	if s.pending != nil && s.pending.Kind == KindResync {
		e.Kind = KindResync
	}
	s.pending = &e
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *Subscription) finish() {
	if !s.done {
		s.done = true
		close(s.signal)
	}
}
