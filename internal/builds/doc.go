// Package builds turns a prepared exact-commit source into a uniquely tagged
// Docker image without trusting the Dockerfile.
//
// Every build runs in a fresh rootless BuildKit container on its own internal
// network whose only exit is the egress proxy (EgressProxy, run by
// cmd/build-egress), which reaches ports 80 and 443 on public addresses only.
// CPU, memory and process limits cover every step; disk use, free disk and
// image size are bounded; caches are shared only within one base and source
// repository. Images and leftovers carry identity labels for retries and
// cleanup. Nothing here starts a preview container.
package builds
