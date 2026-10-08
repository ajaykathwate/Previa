// Package outbound lets a repository's owner approve the outbound
// connections its previews may open to an external database or message
// broker (docs/11-backend-previews.md): a short list of host:port
// destinations managed on the repository's Outbound tab, never in
// PR-controlled YAML. The operator limits the ports (PREVIEW_OUTBOUND_PORTS)
// and can turn the feature off. The worker gives the approvals of trusted
// (same-repository) pull requests to the egress proxy, which forwards only to
// them; forks get none.
package outbound
