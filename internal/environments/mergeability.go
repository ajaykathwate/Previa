package environments

import "errors"

var ErrPRConflicts = errors.New("build refused: resolve merge conflicts and push a new commit first")
var ErrPRChecking = errors.New("build paused: GitHub is checking mergeability; wait for the check or sync pull requests again")
var ErrPRRefresh = errors.New("build paused: refresh this repository to resolve conflicting upstream observations")

// BuildBlockReason is repeated under the generation/admission lock immediately
// before Runtime.Build for both manual and scheduled requests.
func BuildBlockReason(status string, needsRefresh bool) error {
	if status == "conflicts" {
		return ErrPRConflicts
	}
	if needsRefresh {
		return ErrPRRefresh
	}
	if status != "open" {
		return ErrPRChecking
	}
	return nil
}
