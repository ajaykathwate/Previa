// Package usage renders workspace usage from authorized display data.
package usage

import (
	"fmt"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/web/components"
)

type Page struct {
	PreviewExpiry, IdleSleep, LogRetention                                 string
	OperatorKnown                                                          bool
	WorkspaceID                                                            int64
	Name, CSRF, PeriodStart, PeriodEnd, Measured, Cutover, OperatorChecked string
	Manager                                                                bool
	Included, Consumed, Reserved, Daily                                    string
	Builds, BuildLimit, Slots, SlotLimit, Provisional, Pending             int
	Repositories                                                           []Repository
	Blocked                                                                []Work
}
type Repository struct {
	ID                       int64
	Name, Consumed, Reserved string
	Attempts, Provisional    int
}
type Work struct {
	RepoID                                   int64
	Repository, Label, Reason, Action, Until string
}

func URL(id int64) string     { return "/workspaces/" + strconv.FormatInt(id, 10) + "/usage" }
func Minutes(us int64) string { return fmt.Sprintf("%.2f", float64(us)/60000000) }
func Number(n int) string     { return strconv.Itoa(n) }
func DateOnly(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2 Jan 2006")
}
func DateTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2 Jan 2006, 15:04 UTC")
}
func (p Page) PublicReferences() []components.ResourceRef {
	refs := components.ResourceRefs("workspace", p.WorkspaceID)
	for _, r := range p.Repositories {
		refs = append(refs, components.ResourceRefs("repository", r.ID)...)
	}
	for _, w := range p.Blocked {
		refs = append(refs, components.ResourceRefs("repository", w.RepoID)...)
	}
	return refs
}

func Explanation(reason string) (string, string) {
	switch reason {
	case "workspace_accounting":
		return "The worker could not renew its execution reservation.", "This work stopped at its reserved deadline and retries automatically when accounting is available."
	case "workspace_builds", "account":
		return "The workspace is using its concurrent build limit.", "Wait for another build to finish. This work retries automatically."
	case "workspace_minutes", "quota_blocked", "quota":
		return "The workspace's build minutes are consumed or reserved.", "Wait for the allowance reset or for reserved time to be released. This work retries automatically."
	case "workspace_daily":
		return "The operator's daily safety ceiling is reached.", "Wait until 00:00 UTC. A plan change cannot bypass this ceiling."
	case "workspace_previews":
		return "All preview slots are in use.", "Tear down an unused preview or wait for its normal expiry. A replacement of the same PR reuses its slot."
	case "workspace_restricted":
		return "Compute is restricted for this workspace.", "Ask the workspace owner to contact the operator."
	case "paused":
		return "The operator paused builds.", "This work retries when the operator resumes builds."
	case "disk":
		return "The build host is short of disk space.", "This work retries when disk space is available."
	}
	return "Waiting for host capacity.", "This work retries automatically when capacity is available."
}
