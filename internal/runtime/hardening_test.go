package runtime

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/runtime/dbgen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/moby/moby/api/types/container"
)

func TestRunAsReplacesRootUnlessTheOperatorAllowsIt(t *testing.T) {
	for _, tt := range []struct {
		image     string
		allowRoot bool
		want      string
		replaced  bool
	}{
		{"", false, unprivilegedUser, true},
		{"root", false, unprivilegedUser, true},
		{"0", false, unprivilegedUser, true},
		{"0:0", false, unprivilegedUser, true},
		{"root:staff", false, unprivilegedUser, true},
		{"node", false, "node", false},
		{"1000:1000", false, "1000:1000", false},
		{"", true, "", false},
		{"root", true, "root", false},
	} {
		got, replaced := runAs(tt.image, tt.allowRoot)
		if got != tt.want || replaced != tt.replaced {
			t.Errorf("runAs(%q, %v) = %q %v, want %q %v", tt.image, tt.allowRoot, got, replaced, tt.want, tt.replaced)
		}
	}
}

func TestEnvironmentKeepsEgressAuthoritative(t *testing.T) {
	env := environment([]string{"API_URL=https://api.example", "HTTPS_PROXY=http://elsewhere:9", "PORT=8080", "NO_PROXY=*"}, "http://172.20.0.2:3128", 3000, true)
	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	for name, want := range map[string]string{
		"API_URL": "https://api.example", "PORT": "8080", "HOME": "/tmp",
		"HTTP_PROXY": "http://172.20.0.2:3128", "HTTPS_PROXY": "http://172.20.0.2:3128", "https_proxy": "http://172.20.0.2:3128",
		"NO_PROXY": "localhost,127.0.0.1", "NODE_USE_ENV_PROXY": "1",
	} {
		if values[name] != want {
			t.Errorf("%s = %q, want %q", name, values[name], want)
		}
	}
	if !slices.IsSorted(env) {
		t.Error("environment must be deterministic")
	}
	defaults := environment(nil, "http://p:3128", 3000, false)
	if !slices.Contains(defaults, "PORT=3000") || slices.ContainsFunc(defaults, func(e string) bool { return strings.HasPrefix(e, "HOME=") }) {
		t.Fatalf("defaults: %v", defaults)
	}
}

func TestContainerSpecIsHardened(t *testing.T) {
	labels := map[string]string{dockerx.LabelManaged: "true", dockerx.LabelKind: dockerx.KindPreview}
	c := candidate{Name: "n", ImageID: "sha256:abc", ImageUser: "", Port: 3000, Env: []string{"A=b"}, ProxyURL: "http://p:3128",
		CPUMillis: 1000, Memory: 1 << 30, NetworkID: "net-1", Labels: labels}
	policy := Policy{TmpfsBytes: 256 << 20, PIDs: 512, StartupTimeout: time.Minute, MaxPerRepo: 10, OCIRuntime: "runsc"}
	config, host, replaced := containerSpec(c, policy)
	if !replaced || config.User != unprivilegedUser || config.Image != "sha256:abc" || config.Labels[dockerx.LabelKind] != dockerx.KindPreview {
		t.Fatalf("config: %+v", config)
	}
	switch {
	case host.Privileged || host.PublishAllPorts || len(host.PortBindings) > 0 || len(host.Binds) > 0 || len(host.Mounts) > 0 || len(host.Devices) > 0:
		t.Fatal("privileges, ports, host paths or devices must never be granted")
	case !slices.Equal(host.CapDrop, []string{"ALL"}) || len(host.CapAdd) > 0 || !slices.Contains(host.SecurityOpt, "no-new-privileges"):
		t.Fatalf("capabilities: drop %v add %v opts %v", host.CapDrop, host.CapAdd, host.SecurityOpt)
	case !host.ReadonlyRootfs || host.Tmpfs["/tmp"] != "rw,nosuid,nodev,size=268435456,mode=1777":
		t.Fatalf("filesystem: read-only %v tmpfs %v", host.ReadonlyRootfs, host.Tmpfs)
	case host.NetworkMode != container.NetworkMode("net-1"):
		t.Fatalf("network %q", host.NetworkMode)
	case host.Sysctls["net.ipv6.conf.all.disable_ipv6"] != "1" || host.Sysctls["net.ipv6.conf.default.disable_ipv6"] != "1":
		t.Fatalf("IPv6 must be disabled: %v", host.Sysctls)
	case host.NanoCPUs != 1e9 || host.Memory != 1<<30 || host.MemorySwap != 1<<30 || host.PidsLimit == nil || *host.PidsLimit != 512:
		t.Fatalf("limits: %+v", host.Resources)
	case host.CPUShares != cpuWeight || host.OomScoreAdj != oomPriority:
		t.Fatalf("platform protection: shares %d oom %d", host.CPUShares, host.OomScoreAdj)
	case host.RestartPolicy.Name != container.RestartPolicyOnFailure || host.RestartPolicy.MaximumRetryCount != maxRestarts:
		t.Fatalf("restart policy %+v", host.RestartPolicy)
	case host.LogConfig.Config["max-size"] != "10m" || host.Runtime != "runsc":
		t.Fatalf("logs %+v runtime %q", host.LogConfig, host.Runtime)
	}
	opted := policy
	opted.AllowRoot, opted.WritableRootfs = true, true
	config, host, replaced = containerSpec(c, opted)
	if replaced || config.User != "" || host.ReadonlyRootfs {
		t.Fatal("operator opt-outs must be honoured")
	}
	if !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Contains(host.SecurityOpt, "no-new-privileges") {
		t.Fatal("opt-outs never restore capabilities or privileges")
	}
}

func TestAdmitEnforcesRepositoryCountAndHostBudget(t *testing.T) {
	b := budget{cpuMillis: 4000, memory: 8 << 30, perRepo: 2}
	prs := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	if err := admit(usage{cpuMillis: 2000, memory: 4 << 30, repoPRs: prs("5")}, b, 7, 1000, 2<<30); err != nil {
		t.Fatalf("within budget: %v", err)
	}
	// A PR's swap never competes with its own active preview for the count.
	if err := admit(usage{repoPRs: prs("5", "7")}, b, 7, 1000, 2<<30); err != nil {
		t.Fatalf("a swap at the repository limit: %v", err)
	}
	for name, u := range map[string]usage{
		"repository count": {repoPRs: prs("5", "6")},
		"memory":           {memory: 7 << 30},
		"cpu":              {cpuMillis: 3500},
	} {
		if err := admit(u, b, 7, 1000, 2<<30); CodeOf(err) != CodeCapacity {
			t.Errorf("%s: %v", name, err)
		} else if e := err.(*Error); !e.Retryable {
			t.Errorf("%s: capacity refusals must be retryable", name)
		}
	}
}

func row() dbgen.ExecutionTargetRow {
	return dbgen.ExecutionTargetRow{
		PullRequestID: 3, RepoID: 2, Number: 7, HeadSha: "sha-1", State: "open", Generation: 4,
		HeadRepoID: pgtype.Int8{Int64: 100, Valid: true}, GithubRepoID: pgtype.Int8{Int64: 100, Valid: true}, Accessible: true,
		GithubInstallationID: 99, InstallationStatus: "active",
		IntentGeneration: pgtype.Int8{Int64: 4, Valid: true}, IntentRevision: pgtype.Int8{Int64: 2, Valid: true},
		IntentHeadSha: pgtype.Text{String: "sha-1", Valid: true}, IntentState: pgtype.Text{String: "running", Valid: true},
	}
}

func TestCurrentTargetRefusesAnythingStaleOrRevoked(t *testing.T) {
	request := environments.Request{PullRequestID: 3, Generation: 4, Revision: 2, HeadSHA: "sha-1", State: "open"}
	got, err := currentTarget(row(), request)
	if err != nil || got.InstallationID != 99 || got.BaseRepoID != 100 || got.Fork || !got.secretSource().Trusted() {
		t.Fatalf("current same-repository target: %+v %v", got, err)
	}
	for name, tt := range map[string]struct {
		change func(*dbgen.ExecutionTargetRow)
		code   string
	}{
		"closed":           {func(r *dbgen.ExecutionTargetRow) { r.State = "closed" }, CodeNotCurrent},
		"newer generation": {func(r *dbgen.ExecutionTargetRow) { r.Generation = 5 }, CodeNotCurrent},
		"moved head":       {func(r *dbgen.ExecutionTargetRow) { r.HeadSha = "sha-2" }, CodeNotCurrent},
		"newer revision":   {func(r *dbgen.ExecutionTargetRow) { r.IntentRevision.Int64 = 3 }, CodeNotCurrent},
		"not running":      {func(r *dbgen.ExecutionTargetRow) { r.IntentState.String = "succeeded" }, CodeNotCurrent},
		"no intent":        {func(r *dbgen.ExecutionTargetRow) { r.IntentState = pgtype.Text{} }, CodeNotCurrent},
		"access revoked":   {func(r *dbgen.ExecutionTargetRow) { r.Accessible = false }, CodeInaccessible},
		"suspended":        {func(r *dbgen.ExecutionTargetRow) { r.InstallationStatus = "suspended" }, CodeInaccessible},
	} {
		r := row()
		tt.change(&r)
		if _, err := currentTarget(r, request); CodeOf(err) != tt.code {
			t.Errorf("%s: %v, want %s", name, err, tt.code)
		}
	}
	for name, change := range map[string]func(*dbgen.ExecutionTargetRow){
		"fork flag":       func(r *dbgen.ExecutionTargetRow) { r.IsFork = true },
		"other head repo": func(r *dbgen.ExecutionTargetRow) { r.HeadRepoID.Int64 = 555 },
		"deleted head":    func(r *dbgen.ExecutionTargetRow) { r.HeadRepoID = pgtype.Int8{} },
	} {
		r := row()
		change(&r)
		got, err := currentTarget(r, request)
		if err != nil || !got.Fork || got.secretSource().Trusted() {
			t.Errorf("%s must be untrusted: %+v %v", name, got, err)
		}
	}
}

func TestObservedHealthDetectsCrashLoops(t *testing.T) {
	running := observedHealth(container.InspectResponse{State: &container.State{Status: container.StateRunning, Running: true}})
	if !running.Running || running.CrashLoop {
		t.Fatalf("running: %+v", running)
	}
	restarting := observedHealth(container.InspectResponse{RestartCount: 1, State: &container.State{Status: container.StateRestarting, Running: true, Restarting: true}})
	if restarting.Running || restarting.CrashLoop {
		t.Fatalf("restarting is neither ready nor a crash loop yet: %+v", restarting)
	}
	looped := observedHealth(container.InspectResponse{RestartCount: maxRestarts, State: &container.State{Status: container.StateExited, ExitCode: 3, OOMKilled: true}})
	if !looped.CrashLoop || looped.ExitCode != 3 || !looped.OOMKilled {
		t.Fatalf("crash loop: %+v", looped)
	}
}
