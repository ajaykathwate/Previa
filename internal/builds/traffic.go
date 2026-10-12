package builds

import (
	"context"
	"fmt"
	"strings"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/platform/metrics"
)

// RegistryHosts are what a build may reach with BUILD_EGRESS=registries:
// common package registries, base-image registries and the distributions'
// package mirrors. A leading dot matches subdomains. Operators add their own
// with BUILD_EGRESS_HOSTS.
var RegistryHosts = []string{
	// Base images.
	"registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com", "production.cloudfront.docker.com",
	"docker-images-prod.6aa30f8b08e16409b46e0173d6de2f56.r2.cloudflarestorage.com", // Docker Hub's own storage
	"ghcr.io", "pkg-containers.githubusercontent.com",
	"quay.io", ".quay.io", "mcr.microsoft.com", ".data.mcr.microsoft.com", "public.ecr.aws",
	// JavaScript.
	"registry.npmjs.org", "registry.yarnpkg.com", "repo.yarnpkg.com",
	// Python.
	"pypi.org", "files.pythonhosted.org",
	// Go.
	"proxy.golang.org", "sum.golang.org",
	// Ruby, PHP, .NET and Rust.
	"rubygems.org", "index.rubygems.org", "repo.packagist.org", "api.nuget.org",
	"index.crates.io", "static.crates.io",
	// Java and Kotlin.
	"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org", "services.gradle.org",
	// Operating system packages in base images.
	"deb.debian.org", "security.debian.org", "archive.ubuntu.com", "security.ubuntu.com",
	"ports.ubuntu.com", "dl-cdn.alpinelinux.org",
}

// egressHosts is the host list for a build egress mode: none (any public
// host) when open.
func egressHosts(mode string, extra []string) []string {
	if mode != config.BuildEgressRegistries {
		return nil
	}
	return append(append([]string(nil), RegistryHosts...), extra...)
}

// endMetering reads and logs the build's traffic through the egress proxy.
// It reports false when the build was limited to listed hosts and the proxy
// cannot confirm that the limit held (it restarted during the build).
func (b *Builder) endMetering(ctx context.Context, sb *sandbox, job Job) bool {
	if !sb.metered {
		return len(b.cfg.EgressHosts) == 0
	}
	sb.metered = false
	usage, err := sb.egress.EndMetering(ctx, sb.proxyAddr)
	if err != nil {
		b.logger.WarnContext(ctx, "build traffic unknown", "pr_id", job.PullRequestID, "error", err)
		return len(b.cfg.EgressHosts) == 0
	}
	if !usage.Known {
		b.logger.WarnContext(ctx, "build traffic unknown: the egress proxy restarted during the build", "pr_id", job.PullRequestID)
		return len(b.cfg.EgressHosts) == 0
	}
	metrics.BuildTraffic(usage.Up, usage.Down)
	b.logger.InfoContext(ctx, "build traffic", "pr_id", job.PullRequestID, "revision", job.Revision,
		"bytes_up", usage.Up, "bytes_down", usage.Down, "refused", usage.Denied)
	job.Logs.System(fmt.Sprintf("Network: downloaded %s and uploaded %s.", humanBytes(usage.Down), humanBytes(usage.Up)))
	if usage.Denied > 0 {
		job.Logs.System(fmt.Sprintf("This server lets builds reach package registries only. It refused %d request(s) to other hosts: %s. "+
			"If the build needs them, ask the server's operator to allow them.", usage.Denied, strings.Join(usage.DeniedHosts, ", ")))
	}
	return true
}

// startMetering counts the build's traffic and, in registries mode, limits
// it to the listed hosts. Counting is best effort; a limit that cannot be
// set fails the build.
func (b *Builder) startMetering(ctx context.Context, sb *sandbox, job Job) error {
	if err := sb.egress.StartMetering(ctx, sb.proxyAddr, b.cfg.EgressHosts); err != nil {
		if len(b.cfg.EgressHosts) > 0 {
			return fmt.Errorf("limit build traffic to package registries: %w", err)
		}
		b.logger.WarnContext(ctx, "build traffic will not be counted", "pr_id", job.PullRequestID, "error", err)
		return nil
	}
	sb.metered = true
	return nil
}
