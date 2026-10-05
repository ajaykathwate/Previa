package builds

import (
	"fmt"
	"strconv"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// identity labels an artifact of one build attempt (see dockerx labels).
func (j Job) identity(kind string) map[string]string {
	return map[string]string{
		dockerx.LabelManaged: "true", dockerx.LabelInstance: j.Instance, dockerx.LabelKind: kind,
		dockerx.LabelPR: itoa(j.PullRequestID), dockerx.LabelGeneration: itoa(j.Generation), dockerx.LabelRevision: itoa(j.Revision),
		dockerx.LabelSHA: j.HeadSHA, dockerx.LabelBaseRepo: itoa(j.BaseRepoID), dockerx.LabelHeadRepo: itoa(j.HeadRepoID),
	}
}

// ImageRef is the unique local tag of the image built for one PR revision.
func (j Job) ImageRef() string {
	return fmt.Sprintf("previa/%s-pr-%d:g%d-r%d-%s", j.Instance, j.PullRequestID, j.Generation, j.Revision, j.HeadSHA[:12])
}

// cacheScope separates build caches by base repository and by the repository
// the PR's code comes from. Same-repository PRs share one cache; each fork gets
// its own, so a fork's build can never plant layers or cache-mount files that a
// trusted build (which later receives secrets) would reuse. An unknown head is
// scoped to the PR itself.
func (j Job) cacheScope() string {
	if j.HeadRepoID <= 0 {
		return fmt.Sprintf("r%d-pr%d", j.BaseRepoID, j.PullRequestID)
	}
	return fmt.Sprintf("r%d-h%d", j.BaseRepoID, j.HeadRepoID)
}

// CacheVolumeName is the scope's cache volume; housekeeping prunes these.
func CacheVolumeName(instance, scope string) string {
	return "previa-buildcache-" + instance + "-" + scope
}
func cachedBuilderName(instance, scope string) string {
	return "previa-builder-" + instance + "-" + scope
}

// attemptName is unique per build attempt, for networks and uncached builders.
func (j Job) attemptName(prefix string, now time.Time) string {
	return fmt.Sprintf("%s-%s-pr%d-g%d-r%d-%d", prefix, j.Instance, j.PullRequestID, j.Generation, j.Revision, now.UnixNano())
}
