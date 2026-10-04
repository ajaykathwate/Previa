// Package runtime turns an admitted build request into a healthy preview
// container that is isolated and not yet routed. It implements
// environments.Runtime by composing source preparation, the image builder and
// repository secrets; lifecycle transactions stay in internal/environments.
//
// Every preview runs on its own internal network (shared only with the egress
// proxy and, later, the router) as an unprivileged user with a read-only root
// filesystem, no capabilities, no-new-privileges, IPv6 disabled and bounded
// CPU, memory, processes, temporary space and logs. Outbound traffic can reach
// only public addresses on ports 80 and 443, through the proxy. Secrets are
// decrypted after a fresh access and fork recheck, never for forks. Readiness
// is probed from the egress container; failures capture redacted output and
// remove the candidate; ownership labels drive inventory and removal.
package runtime
