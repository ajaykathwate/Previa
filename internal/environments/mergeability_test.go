package environments_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	env "github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/installations"
	"github.com/ajaykathwatee/previa/internal/webhooks"
)

func TestRealGitConflictBlocksManualAndScheduledBuildsUntilResolution(t *testing.T) {
	for _, mode := range []installations.BuildMode{installations.ManualBuilds, installations.AutomaticBuilds} {
		t.Run(string(mode), func(t *testing.T) {
			f := newScheduleFixture(t)
			dir := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			git("init", "-b", "main")
			git("config", "user.name", "Previa test")
			git("config", "user.email", "test@example.invalid")
			remote := filepath.Join(t.TempDir(), "remote.git")
			git("init", "--bare", remote)
			git("remote", "add", "origin", remote)
			write("base\n")
			git("add", "conflict.txt")
			git("commit", "-m", "base")
			git("checkout", "-b", "topic")
			write("topic change\n")
			git("commit", "-am", "topic")
			conflictSHA := git("rev-parse", "HEAD")
			git("checkout", "main")
			write("main change\n")
			git("commit", "-am", "main")
			git("push", "origin", "main", "topic")
			git("checkout", "topic")
			cmd := exec.CommandContext(t.Context(), "git", "merge", "main")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "CONFLICT") || git("diff", "--name-only", "--diff-filter=U") != "conflict.txt" {
				t.Fatal("deliberate merge conflict not created")
			}
			if _, err := f.policy.Set(t.Context(), f.owner, f.repoID, mode); err != nil {
				t.Fatal(err)
			}
			// Start with an authorized queued request, then change only mergeability.
			// This proves admission repeats the check even when generation still matches.
			r := f.observe(t, "opened", conflictSHA)
			if mode == installations.ManualBuilds {
				outcome, err := f.broker.RequestBuild(t.Context(), f.command(r, "before-conflict-build-0001", 0, env.BuildLatest))
				if err != nil {
					t.Fatal(err)
				}
				r = requestFromIntent(outcome.Intent)
			}
			f.clock.advance(env.QuietPeriod)
			if err := f.broker.Dispatch(t.Context(), wake(r)); err != nil {
				t.Fatal(err)
			}
			f.provision(t, r)
			if _, err := f.pool.Exec(t.Context(), "UPDATE pull_requests SET mergeable=false,mergeable_state='dirty',build_status='conflicts' WHERE id=$1", r.PullRequestID); err != nil {
				t.Fatal(err)
			}
			runtime := newRuntime()
			service, err := env.NewService(f.repo, runtime, f.logger, time.Minute, f.clock.now)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Build(t.Context(), r); !errors.Is(err, env.ErrPRConflicts) {
				t.Fatal("runtime admission did not refuse conflicts", err)
			}
			if err := f.broker.Dispatch(t.Context(), wake(r)); !errors.Is(err, env.ErrPRConflicts) {
				t.Fatal("scheduled dispatch did not surface refusal", err)
			}
			if _, err := f.intents.Request(t.Context(), f.command(r, "conflict-attempt-build-0001", r.Revision, env.BuildLatest)); !errors.Is(err, env.ErrPRConflicts) {
				t.Fatal("manual request did not surface refusal", err)
			}
			if !strings.Contains(env.ErrPRConflicts.Error(), "resolve merge conflicts") {
				t.Fatal("refusal lacks contributor guidance")
			}
			select {
			case <-runtime.started:
				t.Fatal("conflicting build reached runtime")
			default:
			}
			// Resolve the actual conflict and push the merge commit to the local remote.
			write("main and topic resolved\n")
			git("add", "conflict.txt")
			git("commit", "-m", "resolve conflict")
			git("push", "origin", "topic")
			resolvedSHA := git("rev-parse", "HEAD")
			if resolvedSHA == conflictSHA || git("diff", "--name-only", "--diff-filter=U") != "" {
				t.Fatal("conflict not resolved")
			}
			git("merge", "--no-commit", "main")
			// GitHub payloads are simulated; the git conflict and resolving push are real.
			yes := true
			next, err := f.events.ProcessPR(t.Context(), webhooks.PREvent{RepoID: f.repoID, Number: 1, Action: "synchronize", HeadSHA: resolvedSHA, Mergeable: &yes, MergeableState: "clean"})
			if err != nil {
				t.Fatal(err)
			}
			if mode == installations.ManualBuilds {
				outcome, err := f.broker.RequestBuild(t.Context(), f.command(next, "resolved-build-attempt-0001", r.Revision, env.BuildLatest))
				if err != nil {
					t.Fatal(err)
				}
				next = requestFromIntent(outcome.Intent)
			}
			f.clock.advance(env.QuietPeriod)
			if err := f.broker.Dispatch(t.Context(), wake(next)); err != nil {
				t.Fatal(err)
			}
			if err := service.Build(t.Context(), next); err != nil {
				t.Fatal("resolved build failed", err)
			}
			select {
			case <-runtime.started:
			default:
				t.Fatal("resolved PR never reached runtime")
			}
			t.Log("Real local Git conflict: Conflicts; manual/dispatch/runtime refused with resolve-conflicts reason; resolving commit pushed, synchronize restores Open and runtime is called. GitHub payloads and runtime are test adapters.")
		})
	}
}
