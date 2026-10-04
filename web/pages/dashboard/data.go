package dashboard

// Summary contains database-derived workspace counts, scoped to the signed-in user.
type Summary struct{ Installations, Repositories, Available int64 }

// Unavailable counts linked repositories whose access or installation is inactive.
func (s Summary) Unavailable() int64 { return max(0, s.Repositories-s.Available) }

// Stage describes one preview pipeline step and whether it is connected today.
type Stage struct {
	Name, Detail string
	Live         bool
}

// PipelineStages reflects implemented capabilities, not activity or health.
var PipelineStages = []Stage{
	{"Discover", "Webhooks and sync track open PRs", true},
	{"Gate", "Mergeability checks before any work", true},
	{"Build", "Image from the exact commit", false},
	{"Run", "Isolated, resource-limited container", false},
	{"Route", "Subdomain and TLS per PR", false},
	{"Report", "PR comment and check run", false},
	{"Teardown", "Cleanup on close and expiry", false},
}
