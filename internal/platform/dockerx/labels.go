package dockerx

// Labels identify every Docker object Previa creates. Retries find earlier
// results by them, removal verifies ownership with them before deleting
// anything, and housekeeping finds crash leftovers through them.
const (
	LabelManaged     = "io.previa.managed" // always "true"
	LabelKind        = "io.previa.kind"
	LabelPR          = "io.previa.pull-request-id"
	LabelGeneration  = "io.previa.generation"
	LabelRevision    = "io.previa.revision"
	LabelSHA         = "io.previa.head-sha"
	LabelBaseRepo    = "io.previa.base-repository-id"
	LabelHeadRepo    = "io.previa.head-repository-id"
	LabelEnvironment = "io.previa.environment-id"
	LabelImage       = "io.previa.image"        // a preview's image reference
	LabelPort        = "io.previa.port"         // a preview's container port
	LabelCPUMillis   = "io.previa.cpu-millis"   // a preview's CPU limit
	LabelMemory      = "io.previa.memory-bytes" // a preview's memory limit
	LabelStarted     = "io.previa.started-at"

	KindImage          = "image"
	KindBuilder        = "builder"
	KindBuildNetwork   = "build-network"
	KindBuildCache     = "build-cache"
	KindEgress         = "egress"
	KindPreview        = "preview"
	KindPreviewNetwork = "preview-network"
)

// Owned reports whether labels mark a Previa object of one of kinds.
func Owned(labels map[string]string, kinds ...string) bool {
	if labels[LabelManaged] != "true" {
		return false
	}
	for _, kind := range kinds {
		if labels[LabelKind] == kind {
			return true
		}
	}
	return false
}
