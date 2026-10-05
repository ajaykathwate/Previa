//go:build !linux && !darwin

package routing

import "errors"

// lockFile is unavailable here; the route controller runs on Linux workers.
func lockFile(string) (func(), error) {
	return nil, errors.New("route table files need a Linux or macOS worker")
}
