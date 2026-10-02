// Package utils contains the helpers required by the copied templUI components.
package utils

import (
	"crypto/rand"
	"fmt"
	twmerge "github.com/Oudwins/tailwind-merge-go"
)

func TwMerge(classes ...string) string {
	return twmerge.Merge(classes...)
}

func If[T any](condition bool, value T) T {
	var empty T
	if condition {
		return value
	}
	return empty
}

func RandomID() string {
	return fmt.Sprintf("id-%s", rand.Text())
}
