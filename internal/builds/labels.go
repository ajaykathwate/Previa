package builds

import (
	"fmt"
	"strconv"
	"time"
)

// Labels identify everything a build creates, so retries can find earlier
// results and housekeeping can find leftovers after a crash.
const (
	LabelManaged    = "io.previa.managed" // always "true"
	LabelKind       = "io.previa.kind"
	LabelPR         = "io.previa.pull-request-id"
	LabelGeneration = "io.previa.generation"
	LabelRevision   = "io.previa.revision"
	LabelSHA        = "io.previa.head-sha"
	LabelBaseRepo   = "io.previa.base-repository-id"
	LabelHeadRepo   = "io.previa.head-repository-id"
	LabelStarted    = "io.previa.started-at"

	KindImage   = "image"
	KindBuilder = "builder"
	KindNetwork = "build-network"
	KindCache   = "build-cache"
	KindEgress  = "build-egress"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// identity labels an artifact of one build attempt.
func (j Job) identity(kind string) map[string]string {
	return map[string]string{
		LabelManaged: "true", LabelKind: kind,
		LabelPR: itoa(j.PullRequestID), LabelGeneration: itoa(j.Generation), LabelRevision: itoa(j.Revision),
		LabelSHA: j.HeadSHA, LabelBaseRepo: itoa(j.BaseRepoID), LabelHeadRepo: itoa(j.HeadRepoID),
	}
}

// ImageRef is the unique local tag of the image built for one PR revision.
func (j Job) ImageRef() string {
	return fmt.Sprintf("previa/pr-%d:g%d-r%d-%s", j.PullRequestID, j.Generation, j.Revision, j.HeadSHA[:12])
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

func cacheVolumeName(scope string) string   { return "previa-buildcache-" + scope }
func cachedBuilderName(scope string) string { return "previa-builder-" + scope }

// attemptName is unique per build attempt, for networks and uncached builders.
func (j Job) attemptName(prefix string, now time.Time) string {
	return fmt.Sprintf("%s-pr%d-g%d-r%d-%d", prefix, j.PullRequestID, j.Generation, j.Revision, now.UnixNano())
}
