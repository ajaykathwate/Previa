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
	LabelAccount     = "io.previa.account-id" // the installation owner's Previa user, for per-account limits
	LabelHeadRepo    = "io.previa.head-repository-id"
	LabelEnvironment = "io.previa.environment-id"
	LabelImage       = "io.previa.image"        // a preview's image reference
	LabelPort        = "io.previa.port"         // a preview's container port
	LabelHealthPath  = "io.previa.health-path"  // a preview's readiness path; absent for a TCP check
	LabelCPUMillis   = "io.previa.cpu-millis"   // a preview's CPU limit
	LabelMemory      = "io.previa.memory-bytes" // a preview's memory limit
	LabelStarted     = "io.previa.started-at"
	// LabelRedactEnv names, comma-separated, the environment variables whose
	// values a preview's output must never show (repository secrets and
	// Previa's service credentials), so output read after startup is redacted
	// like the startup log. Names only, never values.
	LabelRedactEnv = "io.previa.redact-env"
	// LabelCandidate groups one candidate's containers: its app (the preview)
	// and its companions, the backend services, setup and processes. The value
	// is the app container's name, chosen before anything is created.
	LabelCandidate = "io.previa.candidate"
	// LabelComponent names a companion: postgres, redis, setup or a process.
	LabelComponent = "io.previa.component"
	LabelVersion   = "io.previa.service-version" // a backend service's version

	KindImage          = "image"
	KindBuilder        = "builder"
	KindBuildNetwork   = "build-network"
	KindBuildCache     = "build-cache"
	KindEgress         = "egress"
	KindPreview        = "preview"
	KindPreviewNetwork = "preview-network"
	// Companions of a candidate: removed, counted and inventoried with it.
	KindService        = "service"         // a managed PostgreSQL or Redis container
	KindSetup          = "setup"           // the one-off setup container
	KindProcess        = "process"         // an unrouted process from the app's image
	KindServiceNetwork = "service-network" // the PR's network shared with its services
)

// CandidateKinds are the container kinds that belong to a candidate.
var CandidateKinds = []string{KindPreview, KindService, KindSetup, KindProcess}

// CompanionKinds are a candidate's containers other than its app.
var CompanionKinds = []string{KindService, KindSetup, KindProcess}

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
