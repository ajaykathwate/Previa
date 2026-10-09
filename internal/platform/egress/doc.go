// Package egress runs Previa's egress proxy, the only route out of build and
// preview networks: its container's lifecycle (Gateway), the HTTP(S) proxy,
// owner-approved raw TCP forwarding, and readiness probes into previews.
package egress
