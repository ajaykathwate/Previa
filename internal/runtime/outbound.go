package runtime

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"github.com/ajaykathwatee/previa/internal/platform/egress"
	cerrdefs "github.com/containerd/errdefs"
)

// outbound decides the candidate's approved outbound connections and records
// them for its preview network on the egress proxy. Untrusted sources (forks,
// unknown heads) get none, the same rule as secrets; withheld then says
// whether the repository approves any.
func (r *Runtime) outbound(ctx context.Context, t Target, egressAddr netip.Addr) (destinations []egress.Destination, withheld bool, err error) {
	if r.destinations == nil || !r.egress.Forwarding() {
		return nil, false, nil
	}
	approved, err := r.destinations.Destinations(ctx, t.RepoID)
	if err != nil {
		return nil, false, failure(CodeUnavailable, "The repository's approved outbound connections could not be read.", true, err)
	}
	if !t.secretSource().Trusted() {
		destinations, withheld = nil, len(approved[t.RepoID]) > 0
	} else {
		destinations = approved[t.RepoID]
	}
	if err := r.egress.Allow(ctx, egressAddr, destinations); err != nil {
		return nil, false, failure(CodeUnavailable, "Approved outbound connections could not be prepared on the egress proxy.", true, err)
	}
	return destinations, withheld, nil
}

// RefreshOutbound brings every preview network's approvals on the egress
// proxy up to date: the current approvals for trusted previews, none for
// anything else. It withdraws removed approvals from running previews within
// a minute and restores them after the egress container restarts.
func (r *Runtime) RefreshOutbound(ctx context.Context) error {
	if r.egress == nil || r.destinations == nil || r.outboundTargets == nil || !r.egress.Forwarding() {
		return nil
	}
	previews, err := r.outboundTargets.OutboundPreviews(ctx)
	if err != nil {
		return err
	}
	var repos []int64
	for _, p := range previews {
		if p.Trusted && !slices.Contains(repos, p.RepoID) {
			repos = append(repos, p.RepoID)
		}
	}
	approved := map[int64][]egress.Destination{}
	if len(repos) > 0 {
		if approved, err = r.destinations.Destinations(ctx, repos...); err != nil {
			return fmt.Errorf("read approved destinations: %w", err)
		}
	}
	desired := map[netip.Addr][]egress.Destination{}
	for _, p := range previews {
		if !p.Trusted || len(approved[p.RepoID]) == 0 {
			continue
		}
		addr, ok, err := r.egress.Address(ctx, r.networkName(p.PullRequestID))
		if cerrdefs.IsNotFound(err) || err == nil && !ok {
			continue // No network yet, or the proxy has not joined it.
		}
		if err != nil {
			return err
		}
		desired[addr] = approved[p.RepoID]
	}
	return r.egress.SyncAllow(ctx, desired)
}
