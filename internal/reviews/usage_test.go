package reviews

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestExhaustedComputeKeepsReviewReadsFeedbackAndAccountAccess(t *testing.T) {
	f := newFixture(t)
	f.publish(t, f.pr, shaA)
	queries := []string{
		`UPDATE workspace_entitlements SET compute_restricted=true WHERE workspace_id=$1`,
		`INSERT INTO workspace_usage_periods(workspace_id,starts_at,ends_at,consumed_us)
		SELECT workspace_id,date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
		(date_trunc('month',now() AT TIME ZONE 'UTC')+interval '1 month') AT TIME ZONE 'UTC',included_seconds*1000000
		FROM workspace_entitlements WHERE workspace_id=$1`,
	}
	for _, query := range queries {
		if _, err := f.pool.Exec(t.Context(), query, f.workspace); err != nil {
			t.Fatal(err)
		}
	}
	b := newBrowser(f.router(t, "http://localhost", nil))
	b.login(t, "reviewer")
	csrf := b.csrf(t) // Account/workspace access remains available.
	review := f.reviewPath(t)
	res := b.do(http.MethodGet, review, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("review read blocked by compute limit: %d", res.Code)
	}
	action, requestKey := composerForm(t, res.Body.String())
	res = b.do(http.MethodPost, action, url.Values{"csrf_token": {csrf}, "body": {"Checkout still needs an inline error"}, "request_key": {requestKey}, "deployment": {"1"}})
	if res.Code != http.StatusSeeOther {
		t.Fatalf("feedback blocked by compute limit: %d %s", res.Code, res.Body.String())
	}
	res = b.do(http.MethodGet, review, nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Checkout still needs an inline error") {
		t.Fatal("feedback cannot be read at the compute limit")
	}
}
