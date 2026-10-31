package components

import (
	"strconv"
	"strings"
)

// ActivityItem is one recorded action in a member's recent activity. Kinds:
// build (Detail is build, retry or rebuild), created, joined, invited (Login
// and Detail name the invitee and role), left and removed (Login names the
// removed person when someone else was removed).
type ActivityItem struct {
	Kind, Detail, Login, Workspace, Repo, PRTitle, At string
	WorkspaceID, RepoID                               int64
	PRNumber                                          int32
}

// ActivityRefs lists the resources an activity feed links to, so handlers can
// supply their public addresses before rendering.
func ActivityRefs(items []ActivityItem) []ResourceRef {
	var refs []ResourceRef
	for _, v := range items {
		if v.WorkspaceID > 0 {
			refs = append(refs, ResourceRef{Kind: "workspace", ID: v.WorkspaceID})
		}
		if v.RepoID > 0 {
			refs = append(refs, ResourceRef{Kind: "repository", ID: v.RepoID})
			if v.PRNumber > 0 {
				refs = append(refs, ResourceRef{Kind: "pull-request", ID: int64(v.PRNumber), Parent: v.RepoID})
			}
		}
	}
	return refs
}

func activityPRURL(v ActivityItem) string {
	return "/repositories/" + strconv.FormatInt(v.RepoID, 10) + "/pull-requests/" + strconv.Itoa(int(v.PRNumber))
}

func activityBuildVerb(detail string) string {
	switch detail {
	case "retry":
		return "Retried the build of"
	case "rebuild":
		return "Rebuilt"
	default:
		return "Requested a build of"
	}
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

func activityRecipient(login string) string {
	if strings.ContainsRune(login, '@') {
		return login
	}
	return "@" + login
}
