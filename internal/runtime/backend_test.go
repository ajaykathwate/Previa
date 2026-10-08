package runtime

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/dockerx"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	"github.com/moby/moby/api/types/container"
)

func backendSettings() repoconfig.Config {
	return repoconfig.Config{
		Port: 8080, Resources: repoconfig.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30},
		Services: []repoconfig.BackendService{
			{Kind: "postgres", Version: "17", Image: "postgres@sha256:" + strings.Repeat("a", 64)},
			{Kind: "redis", Version: "7", Image: "redis@sha256:" + strings.Repeat("b", 64)},
		},
		Setup:     []string{"/app/api", "migrate"},
		Processes: []repoconfig.Process{{Name: "worker", Command: []string{"/app/api", "worker"}, Resources: repoconfig.Resources{CPUMillis: 500, MemoryBytes: 512 << 20}}},
	}
}

func TestPlanBackendGeneratesPerCandidateCredentials(t *testing.T) {
	a, err := planBackend("previa-x-pr1-g1-r1-1", backendSettings())
	if err != nil {
		t.Fatal(err)
	}
	b, err := planBackend("previa-x-pr1-g1-r1-2", backendSettings())
	if err != nil {
		t.Fatal(err)
	}
	pg, redis := a.services[0], a.services[1]
	if pg.alias != "postgres-"+a.nonce || redis.alias != "redis-"+a.nonce || a.nonce == b.nonce || pg.password == b.services[0].password || len(pg.password) != 32 {
		t.Fatalf("hostnames and passwords are not unique per candidate: %+v %+v", a.services, b.services)
	}
	want := map[string]string{
		"DATABASE_URL": "postgres://preview:" + pg.password + "@" + pg.alias + ":5432/preview?sslmode=disable",
		"PGHOST":       pg.alias, "PGPORT": "5432", "PGUSER": "preview", "PGPASSWORD": pg.password, "PGDATABASE": "preview", "PGSSLMODE": "disable",
		"REDIS_URL": "redis://:" + redis.password + "@" + redis.alias + ":6379/0",
	}
	for name, value := range want {
		if a.env[name] != value {
			t.Errorf("%s = %q, want %q", name, a.env[name], value)
		}
	}
	if len(a.env) != len(want) || !slices.Contains(a.secrets, pg.password) || !slices.Contains(a.secrets, redis.password) {
		t.Fatalf("env %v, redacted %d values", a.env, len(a.secrets))
	}
	// App, two services, the setup container (the app's size) and the process.
	cpu, memory := a.resources(backendSettings())
	if cpu != 1000+500+250+1000+500 || memory != (1<<30)+(512<<20)+(256<<20)+(1<<30)+(512<<20) {
		t.Fatalf("candidate resources %d m, %d bytes", cpu, memory)
	}
}

func TestPreviaConnectionSettingsWin(t *testing.T) {
	env, notice := injectBackend([]string{"DATABASE_URL=from-yaml", "MODE=preview", "REDIS_URL=from-secret"}, map[string]string{"DATABASE_URL": "pg", "REDIS_URL": "redis", "PGHOST": "h"})
	if !slices.Equal(env, []string{"DATABASE_URL=pg", "MODE=preview", "PGHOST=h", "REDIS_URL=redis"}) {
		t.Fatalf("env %v", env)
	}
	if !strings.Contains(notice, "DATABASE_URL, REDIS_URL") || strings.Contains(notice, "from-yaml") {
		t.Fatalf("notice %q", notice)
	}
	if env, notice := injectBackend([]string{"A=1"}, nil); notice != "" || !slices.Equal(env, []string{"A=1"}) {
		t.Fatal("nothing to inject changed the environment")
	}
}

func TestServiceContainersAreHardened(t *testing.T) {
	plan, err := planBackend("key", backendSettings())
	if err != nil {
		t.Fatal(err)
	}
	labels := componentLabels(map[string]string{dockerx.LabelPort: "8080", dockerx.LabelImage: "previa/pr-1", dockerx.LabelKind: dockerx.KindPreview}, dockerx.KindService, "postgres", 500, 512<<20)
	config, host, networking := serviceSpec(plan.services[0], []string{"/var/lib/postgresql/data"}, "svc-net", labels, Policy{OCIRuntime: "runsc"})
	switch {
	case config.User != "postgres" || config.Image != plan.services[0].Image:
		t.Fatalf("user %q image %q", config.User, config.Image)
	case !host.ReadonlyRootfs || !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Contains(host.SecurityOpt, "no-new-privileges"):
		t.Fatalf("privileges %+v", host)
	case host.RestartPolicy.Name != container.RestartPolicyDisabled || host.Runtime != "runsc" || host.NetworkMode != "svc-net":
		t.Fatalf("restart %v runtime %q network %q", host.RestartPolicy, host.Runtime, host.NetworkMode)
	case host.Memory != 512<<20 || host.MemorySwap != 512<<20 || host.NanoCPUs != 500e6 || *host.PidsLimit != servicePIDs:
		t.Fatalf("limits %+v", host.Resources)
	case !strings.Contains(host.Tmpfs["/var/lib/postgresql/data"], "size=201326592") || host.Tmpfs["/var/run/postgresql"] == "" || len(host.Binds) != 0 || len(host.Mounts) != 0:
		t.Fatalf("storage %v %v %v", host.Tmpfs, host.Binds, host.Mounts)
	case !slices.Equal(networking.EndpointsConfig["svc-net"].Aliases, []string{plan.services[0].alias}) || len(networking.EndpointsConfig) != 1:
		t.Fatalf("networks %+v", networking.EndpointsConfig)
	case !slices.Contains(config.Env, "PGDATA=/var/lib/postgresql/data/pgdata") || !slices.Contains(config.Env, "POSTGRES_PASSWORD="+plan.services[0].password):
		t.Fatalf("env %v", config.Env)
	case labels[dockerx.LabelKind] != dockerx.KindService || labels[dockerx.LabelPort] != "" || labels[dockerx.LabelImage] != "" || labels[dockerx.LabelComponent] != "postgres":
		t.Fatalf("labels %v", labels)
	}
	if len(host.PortBindings) != 0 || host.Privileged || host.NetworkMode == "host" {
		t.Fatal("a service publishes ports or runs privileged")
	}
	// Redis keeps nothing on disk and requires its password.
	config, host, _ = serviceSpec(plan.services[1], nil, "svc-net", labels, Policy{})
	if !slices.Contains(config.Cmd, "--requirepass") || !slices.Contains(config.Cmd, plan.services[1].password) || !slices.Contains(config.Cmd, "--appendonly") || host.Tmpfs["/data"] == "" || config.User != "redis" {
		t.Fatalf("redis %v %v", config.Cmd, host.Tmpfs)
	}
}

func TestSetupAndProcessesRunTheirArgvWithTheAppsHardening(t *testing.T) {
	app, appHost, _ := containerSpec(candidate{Name: "app", ImageID: "sha256:img", Port: 8080, ProxyURL: "http://10.0.0.2:3128", CPUMillis: 1000, Memory: 1 << 30,
		NetworkID: "pr-net", ExtraHosts: []string{"db.example.com:10.0.0.2"}, Labels: map[string]string{dockerx.LabelKind: dockerx.KindPreview}},
		Policy{TmpfsBytes: 64 << 20, PIDs: 128, StartupTimeout: time.Minute})
	config, host, _ := commandSpec(app, appHost, nil, []string{"/app/api", "worker", "--queue", "jobs"}, "process-worker",
		container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: maxRestarts}, map[string]string{dockerx.LabelKind: dockerx.KindProcess}, 500, 512<<20)
	switch {
	case !slices.Equal(config.Entrypoint, []string{"/app/api", "worker", "--queue", "jobs"}) || config.Cmd != nil:
		t.Fatalf("argv %v %v", config.Entrypoint, config.Cmd)
	case config.User != app.User || !slices.Equal(config.Env, app.Env) || !host.ReadonlyRootfs || !slices.Equal(host.CapDrop, []string{"ALL"}):
		t.Fatal("a process lost the app's hardening or environment")
	case host.Memory != 512<<20 || host.NanoCPUs != 500e6 || host.RestartPolicy.MaximumRetryCount != maxRestarts:
		t.Fatalf("process limits %+v %v", host.Resources, host.RestartPolicy)
	case !slices.Equal(host.ExtraHosts, []string{"db.example.com:10.0.0.2"}) || appHost.Memory != 1<<30:
		t.Fatalf("hosts %v; the app's own limits changed: %d", host.ExtraHosts, appHost.Memory)
	case config.Labels[dockerx.LabelKind] != dockerx.KindProcess || app.Labels[dockerx.LabelKind] != dockerx.KindPreview:
		t.Fatal("labels leaked between the app and its process")
	}
}
