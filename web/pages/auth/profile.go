// Package auth presents sign-in and the signed-in member profile.
package auth

import "strings"

// Profile separates the member's Previa identity (editable name and photo,
// session) from the read-only GitHub account it is linked to.
type Profile struct {
	Name, Login, Avatar, PublicID, FirstName, LastName, FirstHint, LastHint    string
	GitHubName, Email, ProfileURL, Bio, Company, Location                      string
	Joined, GitHubJoined, Synced, Expires, CSRF, NameError, AvatarError, Toast string
	GitHubID, Revision                                                         int64
	EmailSyncError, CustomAvatar                                               bool
}

func placeholder(hint, fallback string) string {
	if hint != "" {
		return hint
	}
	return fallback
}

// displayURL drops the scheme from a profile link for a compact label.
func displayURL(raw string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://"), "/")
}
