package repoconfig

import (
	"encoding/hex"
	"reflect"
	"testing"
	"time"
)

// Repository configuration is attacker-controlled. Arbitrary YAML must either
// fail safely or produce deterministic settings within the operator's limits.
func FuzzBuildConfiguration(f *testing.F) {
	for _, input := range []string{
		"{}", "port: 65535", "port: -1", "context: ../app", "build_timeout: 30m",
		"services: {postgres: 17}\nsetup: [app, migrate]\nseed: [app, seed]",
		"processes: [{name: worker, command: [app, work], resource_tier: large}]",
		"env: [A=one, A=two]", "port: &port 3000\nenv: [*port]",
		"{}\n---\n{}", "port: [", "\x00", "resource_tier: large",
	} {
		f.Add([]byte(input), uint16(2000), uint16(2048), uint16(720))
	}
	f.Fuzz(func(t *testing.T, data []byte, cpu, memory uint16, seconds uint16) {
		if len(data) > MaxConfigBytes+1 {
			t.Skip()
		}
		platform := backendPlatform()
		platform.Limits = Resources{int64(cpu%2000) + 1, (int64(memory%2048) + 1) << 20}
		platform.BuildTimeout = time.Duration(seconds%1800+1) * time.Second
		result, err := ParseFor(data, platform)
		if err != nil {
			return
		}
		cfg := result.Config
		if cfg.Port < 1 || cfg.Port > 65535 || cfg.Resources.CPUMillis <= 0 || cfg.Resources.CPUMillis > platform.Limits.CPUMillis || cfg.Resources.MemoryBytes <= 0 || cfg.Resources.MemoryBytes > platform.Limits.MemoryBytes {
			t.Fatal("accepted configuration escaped port or resource limits")
		}
		if cfg.BuildTimeout <= 0 || cfg.BuildTimeout > platform.BuildTimeout || len(cfg.Processes) > MaxProcesses {
			t.Fatal("accepted configuration escaped execution limits")
		}
		for _, process := range cfg.Processes {
			if process.Resources.CPUMillis <= 0 || process.Resources.CPUMillis > platform.Limits.CPUMillis || process.Resources.MemoryBytes <= 0 || process.Resources.MemoryBytes > platform.Limits.MemoryBytes {
				t.Fatal("process escaped operator resource limits")
			}
		}
		hash, err := hex.DecodeString(result.Hash)
		if err != nil || len(hash) != 32 {
			t.Fatal("accepted configuration has no SHA-256 identity")
		}
		again, err := ParseFor(data, platform)
		if err != nil || !reflect.DeepEqual(result, again) {
			t.Fatal("parsing identical configuration is nondeterministic")
		}
	})
}
