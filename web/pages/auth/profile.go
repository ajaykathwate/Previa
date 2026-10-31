// Package auth presents sign-in and the signed-in member profile.
package auth

import (
	"strings"

	"github.com/ajaykathwatee/previa/web/components"
)

// Profile separates the member's Previa identity (editable name and photo,
// session) from the read-only GitHub account it is linked to.
type Profile struct {
	GoogleEmail                                                                string
	GoogleConnected, GoogleEnabled                                             bool
	Name, Login, Avatar, PublicID, FirstName, LastName, FirstHint, LastHint    string
	GitHubName, Email, ProfileURL, Bio, Company, Location                      string
	Joined, GitHubJoined, Synced, Expires, CSRF, NameError, AvatarError, Toast string
	GitHubID, Revision                                                         int64
	EmailSyncError, CustomAvatar, ActivityUnavailable                          bool
	Activity                                                                   []components.ActivityItem
}

// PublicReferences lists the workspaces, repositories and pull requests the
// activity feed links to.
func (p Profile) PublicReferences() []components.ResourceRef {
	return components.ActivityRefs(p.Activity)
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
