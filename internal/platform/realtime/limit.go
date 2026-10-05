package realtime

import (
	"sync"

	"github.com/ajaykathwatee/previa/internal/platform/metrics"
)

// Limiter caps concurrent streams overall and per key (for example per user),
// so open connections cannot exhaust the server.
type Limiter struct {
	mu            sync.Mutex
	total, perKey int
	open          int
	byKey         map[string]int
}

// NewLimiter allows total streams at once and perKey for any one key.
func NewLimiter(total, perKey int) *Limiter {
	return &Limiter{total: max(total, 1), perKey: max(perKey, 1), byKey: map[string]int{}}
}

// Acquire reserves a slot for key. When ok, release must be called once the
// stream ends; calling it again is harmless.
func (l *Limiter) Acquire(key string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open >= l.total || l.byKey[key] >= l.perKey {
		return func() {}, false
	}
	l.open++
	l.byKey[key]++
	closed := metrics.StreamOpened()
	var once sync.Once
	return func() {
		once.Do(func() {
			closed()
			l.mu.Lock()
			defer l.mu.Unlock()
			l.open--
			if l.byKey[key]--; l.byKey[key] <= 0 {
				delete(l.byKey, key)
			}
		})
	}, true
}
