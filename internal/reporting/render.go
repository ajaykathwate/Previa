package reporting

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/ajaykathwatee/previa/internal/environments"
	"regexp"
	"strconv"
	"strings"
)

// Marker identifies Previa's living comment among a PR's comments. Discovery
// also requires the comment to be written by this GitHub App.
const Marker = "<!-- previa:living-comment -->"

var versionPattern = regexp.MustCompile(`<!-- previa:version (\d+) -->`)

var phaseTitle = map[Phase]string{
	PhaseNone:     "No preview requested",
	PhaseWaiting:  "Waiting",
	PhaseQueued:   "Queued",
	PhaseBlocked:  "Waiting to build",
	PhaseCapacity: "Waiting for capacity",
	PhaseBuilding: "Building",
	PhaseReady:    "Ready",
	PhaseFailed:   "Failed",
	PhaseRemoving: "Removing",
	PhaseRemoved:  "Preview removed",
	PhaseExpired:  "Preview expired",
}

// CommentContent is the living comment's text without its version line, so
// an unchanged state renders identically and costs no GitHub write.
func CommentContent(v View) string {
	var b strings.Builder
	title := phaseTitle[v.Phase]
	if v.Phase == PhaseCapacity {
		title = environments.WaitingTitle(v.Waiting) // capacity, disk space or paused builds
	}
	b.WriteString("### Previa preview: " + title + "\n\n")
	short := func(sha string) string { return "`" + sha[:min(7, len(sha))] + "`" }
	switch v.Phase {
	case PhaseReady:
		if v.PreviewURL != "" {
			b.WriteString("**Preview:** " + v.PreviewURL + "\n\n")
		}
		if v.DocsURL != "" {
			b.WriteString("**Docs:** " + v.DocsURL + "\n\n")
		}
		b.WriteString("Commit " + short(v.Deployed) + ".\n")
		if v.Backend != "" {
			b.WriteString("Runs with " + v.Backend + ". Data does not survive a rebuild.\n")
		}
	case PhaseWaiting:
		b.WriteString("Commit " + short(v.HeadSHA) + " builds after five quiet minutes, at " + v.EligibleAt.UTC().Format("15:04 MST") + ".\n")
	case PhaseQueued:
		b.WriteString("Commit " + short(v.HeadSHA) + " is queued and builds when a worker is free.\n")
	case PhaseBlocked:
		b.WriteString(v.Blocked + "\n")
	case PhaseCapacity:
		b.WriteString("Commit " + short(v.HeadSHA) + ": " + environments.WaitingMessage(v.Waiting) + "\n")
	case PhaseBuilding:
		b.WriteString("Building commit " + short(v.HeadSHA) + ".\n")
	case PhaseFailed:
		b.WriteString("Commit " + short(v.HeadSHA) + " failed: " + v.Failure + "\n")
	case PhaseRemoving:
		b.WriteString("The preview is being removed.\n")
	case PhaseRemoved:
		b.WriteString("This pull request no longer has a preview.\n")
	case PhaseExpired:
		b.WriteString("The preview expired. A new build request, or a new commit with Automatic builds, brings it back at the same address.\n")
	case PhaseNone:
		b.WriteString("Nothing is requested for commit " + short(v.HeadSHA) + ".\n")
	}
	if v.Phase != PhaseReady && v.Deployed != "" && v.PreviewURL != "" {
		b.WriteString("\n**Current preview** (commit " + short(v.Deployed) + "): " + v.PreviewURL + "\n")
	}
	if v.Deployed != "" && !v.ExpiresAt.IsZero() {
		b.WriteString("\nThe preview expires at " + v.ExpiresAt.UTC().Format("15:04 MST on 2 Jan 2006") + " unless a newer one is published.\n")
	}
	for _, notice := range v.Notices {
		b.WriteString("\n> " + notice + "\n")
	}
	if v.LogsURL != "" && v.Phase != PhaseRemoved && v.Phase != PhaseExpired {
		b.WriteString("\n[Build log](" + v.LogsURL + ") (repository owner sign-in)\n")
	}
	return b.String()
}

// CommentBody is the full comment: marker, the report version that wrote it,
// and the content.
func CommentBody(v View, version int64) string {
	return Marker + "\n<!-- previa:version " + strconv.FormatInt(version, 10) + " -->\n" + CommentContent(v)
}

// CommentVersion reads the report version a comment was written for; 0 when
// it has none.
func CommentVersion(body string) int64 {
	m := versionPattern.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	version, _ := strconv.ParseInt(m[1], 10, 64)
	return version
}

// ExternalID is a check run's stable identity: one run per PR, commit and
// request revision, so a same-commit Retry or Rebuild gets its own run and a
// repeated report never duplicates one.
func ExternalID(pullRequestID int64, c *Check) string {
	return fmt.Sprintf("previa:pr:%d:%s:r%d", pullRequestID, c.HeadSHA, c.Revision)
}

// Digest fingerprints rendered content.
func Digest(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))[:32]
}

// CheckDigest fingerprints what a check run shows.
func CheckDigest(c *Check) string {
	return Digest(c.HeadSHA, strconv.FormatInt(c.Revision, 10), c.Status, c.Conclusion, c.Title, c.Summary, c.DetailsURL)
}
