package components

import (
	"fmt"
	"time"
)

// Upcoming reports whether iso (RFC 3339) is still ahead of now.
func Upcoming(iso string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, iso)
	return err == nil && at.After(now)
}

// TimeLeft is a countdown's text: "4:32", "1:04:32" past an hour, and "0:00"
// once iso has passed or cannot be read. ui.js formats it the same way.
func TimeLeft(iso string, now time.Time) string {
	at, err := time.Parse(time.RFC3339, iso)
	if err != nil || !at.After(now) {
		return "0:00"
	}
	left := int((at.Sub(now) + time.Second - 1) / time.Second)
	if hours := left / 3600; hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, left/60%60, left%60)
	}
	return fmt.Sprintf("%d:%02d", left/60, left%60)
}
