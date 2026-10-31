package components

import (
	"testing"
	"time"
)

func TestTimeLeftCountsDownAndStopsAtZero(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for iso, want := range map[string]string{
		at(5 * time.Minute):                            "5:00",
		at(4*time.Minute + 32*time.Second):             "4:32",
		at(9 * time.Second):                            "0:09",
		at(time.Hour + 4*time.Minute + 32*time.Second): "1:04:32",
		at(-time.Second):                               "0:00",
		"not a time":                                   "0:00",
	} {
		if got := TimeLeft(iso, now); got != want {
			t.Errorf("TimeLeft(%s) = %q, want %q", iso, got, want)
		}
	}
	if !Upcoming(at(time.Second), now) || Upcoming(at(0), now) || Upcoming("", now) {
		t.Error("Upcoming is true only for times still ahead")
	}
}
