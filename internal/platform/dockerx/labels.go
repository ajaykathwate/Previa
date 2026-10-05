package dockerx

import "regexp"

// Labels identify every Docker object Previa creates. Retries find earlier
// results by them, removal verifies ownership with them before deleting
// anything, and housekeeping finds crash leftovers through them.
const (
	LabelManaged     = "io.previa.managed"  // always "true"
	LabelInstance    = "io.previa.instance" // the Previa database that owns it (platform_instance)
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
	LabelHealthPath  = "io.previa.health-path"  // a preview's readiness path; absent for a TCP check
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

var instancePattern = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

// ValidInstance reports whether instance can name and label Docker objects:
// 8 to 32 lowercase letters and digits.
func ValidInstance(instance string) bool { return instancePattern.MatchString(instance) }

// OwnedBy reports whether labels mark an object of one of kinds that this
// Previa instance created. Instances sharing a Docker host (a test database
// next to a development one, for example) never touch each other's objects,
// even when their pull request IDs coincide.
func OwnedBy(labels map[string]string, instance string, kinds ...string) bool {
	return ValidInstance(instance) && labels[LabelInstance] == instance && Owned(labels, kinds...)
}

// Owned reports whether labels mark a Previa object of one of kinds, from any
// instance.
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
