// Package source prepares the exact commit a preview will build: it downloads the
// base repository's archive for one full commit SHA, verifies the archive's
// declared commit, extracts it defensively into a fresh worker-owned workspace,
// and loads .previewrc.yaml from that same tree.
//
// Fork PRs are fetched by SHA through the base repository, so Previa never needs
// permission to read the fork itself. Installation credentials are sent only to
// the GitHub API; the archive itself is downloaded from an allowlisted host with
// no credentials attached. Nothing here builds images or runs code.
package source
