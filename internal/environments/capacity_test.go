package environments

import (
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"
)

func TestWaitDelayBacksOffToFiveMinutes(t *testing.T) {
	for snoozes, want := range map[string]time.Duration{
		``:               30 * time.Second,
		`{}`:             30 * time.Second,
		`{"snoozes":1}`:  time.Minute,
		`{"snoozes":3}`:  4 * time.Minute,
		`{"snoozes":4}`:  5 * time.Minute,
		`{"snoozes":40}`: 5 * time.Minute,
	} {
		if got := waitDelay(&rivertype.JobRow{Metadata: []byte(snoozes)}); got != want {
			t.Errorf("%q: %v, want %v", snoozes, got, want)
		}
	}
}

type codedFailure string

func (c codedFailure) Error() string       { return string(c) }
func (c codedFailure) FailureCode() string { return string(c) }

func TestWaitingReasonOnlyForCapacityAndDisk(t *testing.T) {
	for err, want := range map[error]string{
		codedFailure("capacity"):        WaitCapacity,
		codedFailure("disk_low"):        WaitDisk,
		codedFailure("unhealthy"):       "",
		&WaitingError{Reason: WaitDisk}: WaitDisk,
		codedFailure("build_failed"):    "",
		nil:                             "",
	} {
		if got := waitingReason(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}
