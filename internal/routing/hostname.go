package routing

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

const (
	tokenLength   = 8
	tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NewLabel allocates the hostname label of a PR's lifetime environment:
// pr-<number>-<GitHub repository ID in base 36>-<8 random characters>, for
// example pr-42-k2j4ld-7xq9m2fa. The repository ID is immutable, so renamed or
// same-named repositories never collide; the random part keeps previews of
// private repositories unguessable. It is an environments.HostnameAllocator.
func NewLabel(number int32, githubRepoID int64) (string, error) {
	if number <= 0 || githubRepoID <= 0 {
		return "", errors.New("a preview hostname needs a positive PR number and GitHub repository ID")
	}
	token, err := randomToken(tokenLength)
	if err != nil {
		return "", err
	}
	label := "pr-" + strconv.FormatInt(int64(number), 10) + "-" + strconv.FormatInt(githubRepoID, 36) + "-" + token
	if !ValidLabel(label) {
		return "", fmt.Errorf("allocated preview hostname %q is not a DNS label", label)
	}
	return label, nil
}

// randomToken draws n characters uniformly from tokenAlphabet.
func randomToken(n int) (string, error) {
	token := make([]byte, 0, n)
	var b [32]byte
	for len(token) < n {
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("read randomness for a preview hostname: %w", err)
		}
		for _, v := range b {
			// 252 is the largest multiple of 36 below 256: no modulo bias.
			if v < 252 && len(token) < n {
				token = append(token, tokenAlphabet[v%36])
			}
		}
	}
	return string(token), nil
}

// ValidLabel reports whether s is a single lowercase DNS label.
func ValidLabel(s string) bool { return labelPattern.MatchString(s) }

// Host is a preview's full hostname below the preview base domain.
func Host(label, baseDomain string) string { return label + "." + baseDomain }
