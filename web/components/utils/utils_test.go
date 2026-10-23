package utils

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Every page render merges button classes, and concurrent requests share the
// merger's cache. More distinct class lists than the cache holds force
// evictions while other goroutines read: run with -race, which flagged the
// unsynchronized cache of tailwind-merge-go before v0.2.2.
func TestTwMergeIsSafeForConcurrentRenders(t *testing.T) {
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 1500 {
				n := (g*1500 + i) % 2500
				// The merged order varies; the classes kept must not.
				got := strings.Fields(TwMerge("px-2 py-1", fmt.Sprintf("px-[%dpx]", n)))
				slices.Sort(got)
				if want := []string{fmt.Sprintf("px-[%dpx]", n), "py-1"}; !slices.Equal(got, want) {
					t.Errorf("TwMerge kept %q, want %q", got, want)
					return
				}
			}
		})
	}
	wg.Wait()
}
