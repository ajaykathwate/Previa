package components

import (
	"hash/fnv"
	"strconv"
)

// TipID derives a stable element ID for a tooltip from its text.
func TipID(text string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	return "tip-" + strconv.FormatUint(uint64(h.Sum32()), 36)
}
