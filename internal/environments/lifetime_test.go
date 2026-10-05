package environments_test

import (
	"testing"
	"time"
)

func (f *fixture) expiresAt(t *testing.T, pr int64) (time.Time, bool) {
	t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(t.Context(), "SELECT expires_at FROM environments WHERE pull_request_id=$1", pr).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at == nil {
		return time.Time{}, false
	}
	return *at, true
}

func TestPublicationSetsExpiryAndTeardownClearsIt(t *testing.T) {
	f := newFixture(t)
	f.repo.WithLifetime(48 * time.Hour)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	if _, err := f.pool.Exec(t.Context(), "UPDATE environments SET expires_at=NULL WHERE pull_request_id=$1", a.PullRequestID); err != nil {
		t.Fatal(err)
	}
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, set := f.expiresAt(t, a.PullRequestID); set {
		t.Fatal("expiry set before the preview was published")
	}
	drainWith(t, f, &fenceExecutor{repo: f.repo, runtime: r})
	first, set := f.expiresAt(t, a.PullRequestID)
	if !set || time.Until(first) < 47*time.Hour || time.Until(first) > 49*time.Hour {
		t.Fatalf("publication set expiry %v (%v)", first, set)
	}
	// A failed rebuild leaves the published preview's expiry alone.
	b := f.event(t, "synchronize", "sha-B")
	r.fail[b.Generation] = true
	_ = s.Build(t.Context(), b)
	drainWith(t, f, &fenceExecutor{repo: f.repo, runtime: r})
	if after, _ := f.expiresAt(t, a.PullRequestID); !after.Equal(first) {
		t.Fatalf("a failed rebuild moved the expiry from %v to %v", first, after)
	}
	// A successful rebuild publishes again and restarts the lifetime.
	c := f.event(t, "synchronize", "sha-C")
	if err := s.Build(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	drainWith(t, f, &fenceExecutor{repo: f.repo, runtime: r})
	if again, _ := f.expiresAt(t, a.PullRequestID); !again.After(first) {
		t.Fatalf("a publication did not restart the lifetime: %v then %v", first, again)
	}
	closed := f.event(t, "closed", "sha-C")
	if err := s.Teardown(t.Context(), closed); err != nil {
		t.Fatal(err)
	}
	if _, set := f.expiresAt(t, a.PullRequestID); set {
		t.Fatal("a torn-down preview kept an expiry")
	}
}

func TestWithoutLifetimePreviewsNeverExpire(t *testing.T) {
	f := newFixture(t)
	a := f.event(t, "opened", "sha-A")
	f.provision(t, a)
	r := newRuntime()
	s := service(t, f, r, &fakePublisher{})
	if err := s.Build(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	drainWith(t, f, &fenceExecutor{repo: f.repo, runtime: r})
	if at, set := f.expiresAt(t, a.PullRequestID); set {
		t.Fatalf("expiry %v without a lifetime", at)
	}
}
