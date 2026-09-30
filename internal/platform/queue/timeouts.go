package queue

import "time"

const (
	PingTimeout         = 5 * time.Second
	DefaultBuildTimeout = 12 * time.Minute
	TeardownTimeout     = 2 * time.Minute
	// Reserve time after execution cancellation for bounded publication/cleanup.
	CompletionGrace = time.Minute
	RescueScanFloor = 30 * time.Second
)

// RescueAfter is the effective minimum age at which River may rescue this kind.
// River's rescuer checks the registered worker's Timeout after its scan floor.
func RescueAfter(workerTimeout time.Duration) time.Duration {
	return max(RescueScanFloor, workerTimeout)
}
