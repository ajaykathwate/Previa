package routing

import (
	"strings"
	"testing"
)

func TestNewLabelIsAStableDNSSafeFormat(t *testing.T) {
	label, err := NewLabel(42, 1296)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "pr-42-100-" // 1296 is 100 in base 36
	if !strings.HasPrefix(label, prefix) || len(label) != len(prefix)+tokenLength || !ValidLabel(label) {
		t.Fatalf("label %q", label)
	}
	for _, r := range strings.TrimPrefix(label, prefix) {
		if !strings.ContainsRune(tokenAlphabet, r) {
			t.Fatalf("token character %q outside the alphabet", r)
		}
	}
	// The largest PR number and repository ID still fit one DNS label.
	longest, err := NewLabel(2147483647, 9223372036854775807)
	if err != nil || len(longest) > 63 || !ValidLabel(longest) {
		t.Fatalf("longest label %q, %v", longest, err)
	}
}

func TestNewLabelTokensDiffer(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		label, err := NewLabel(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if seen[label] {
			t.Fatalf("repeated label %q", label)
		}
		seen[label] = true
	}
}

func TestNewLabelRejectsMissingIdentity(t *testing.T) {
	for _, c := range []struct {
		number int32
		repo   int64
	}{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		if _, err := NewLabel(c.number, c.repo); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestValidLabel(t *testing.T) {
	for label, want := range map[string]bool{
		"pr-1-1-abcdefgh": true, "a": true, "-a": false, "a-": false, "A": false,
		"a.b": false, "a_b": false, "": false, strings.Repeat("a", 63): true, strings.Repeat("a", 64): false,
		"a`b": false,
	} {
		if ValidLabel(label) != want {
			t.Fatalf("ValidLabel(%q) = %v", label, !want)
		}
	}
}
