package dashboard

// Summary contains database-derived workspace counts, scoped to the signed-in
// user, and what the running workers can do.
type Summary struct {
	Installations, Repositories, Available int64
	// Current requests and previews on open pull requests.
	Waiting, Building, Failed, Live int64
	// Capabilities of the workers that are running now.
	Workers Workers
}

// Workers says which pipeline stages a running worker provides.
type Workers struct{ Discovery, Execution, Reporting, Housekeeping, Paused bool }

// Unavailable counts linked repositories whose access or installation is inactive.
func (s Summary) Unavailable() int64 { return max(0, s.Repositories-s.Available) }

// Stage describes one preview pipeline step and whether a running worker
// provides it now.
type Stage struct {
	Name, Detail string
	Live         bool
}

// Stages reflects the running workers, not activity or health: a stage is
// connected while some worker provides it.
func (s Summary) Stages() []Stage {
	w := s.Workers
	return []Stage{
		{"Discover", "Webhooks and sync track open PRs", w.Discovery},
		{"Gate", "Mergeability checks before any work", w.Discovery},
		{"Build", "Image from the exact commit", w.Execution},
		{"Run", "Isolated, resource-limited container", w.Execution},
		{"Route", "Subdomain and TLS per PR", w.Execution},
		{"Report", "PR comment and check run", w.Reporting},
		{"Teardown", "Cleanup on close and expiry", w.Execution && w.Housekeeping},
	}
}
