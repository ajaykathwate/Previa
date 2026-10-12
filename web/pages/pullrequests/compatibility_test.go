package pullrequests

import (
	"strings"
	"testing"
)

func TestCompatibilityDiagnoses(t *testing.T) {
	for _, tc := range []struct{ code, line, title, guidance string }{
		{"build_failed", "ERROR: pull access denied: authentication required", "registry credentials", "REGISTRY_AUTH"},
		{"build_failed", "ERROR: no match for platform in manifest", "different platform", "Linux"},
		{"crashed", "exec /app/server: exec format error", "different platform", "amd64"},
		{"build_failed", "image operating system (windows) cannot be used on this platform", "different platform", "Windows"},
		{"crashed", "RuntimeError: Found no NVIDIA driver on your system", "different platform", "GPU"},
		{"crashed", "RuntimeError: No CUDA GPUs are available", "different platform", "GPU"},
		{"unhealthy", "connection refused", "didn't become ready", "8080"},
		{"crashed", "nginx bind port 80: permission denied", "stopped while starting", "nginx-unprivileged"},
		{"source_failed", "Source preparation failed: Git submodules are unsupported", "Git submodules", "Vendor"},
		{"source_failed", "Source preparation failed: Git LFS pointer files are unsupported", "Git LFS pointers", "real files"},
		{"source_failed", "Source preparation failed: The source contains more than 100000 entries.", "too large", "context"},
		{"image_too_large", "", "image is too large", "multi-stage"},
		{"timeout", "", "ran out of time", "build_timeout"},
	} {
		t.Run(tc.code+tc.title, func(t *testing.T) {
			d := Diagnose(tc.code, []string{tc.line})
			if !strings.Contains(d.Title, tc.title) || !strings.Contains(strings.Join(d.Steps, " "), tc.guidance) || d.Code != tc.code {
				t.Fatalf("diagnosis: %+v", d)
			}
		})
	}
}
