package source

import (
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise path validation without writing an archive to disk, including
// traversal, multiple archive roots, long names and escaping symlink targets.
func FuzzArchivePathContainment(f *testing.F) {
	for _, seed := range [][2]string{
		{"repo/Dockerfile", "Dockerfile"}, {"repo/app/link", "../Dockerfile"},
		{"repo/../escape", "../../escape"}, {"/etc/passwd", "/etc/passwd"},
		{"repo/a//b", ""}, {"repo/a/./b", "."}, {"repo/a\x00b", "\x00"},
		{"repo/" + strings.Repeat("x", 256), "../target"},
		{"repo/app/link", "../../../escape"}, {"repo", "target"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, raw, target string) {
		if len(raw) > 8192 || len(target) > 8192 {
			t.Skip()
		}
		x := extractor{limits: (Limits{}).withDefaults()}
		name, root, err := x.relative(raw)
		if err != nil {
			return
		}
		if !root && (!filepath.IsLocal(name) || path.Clean(name) != name || len(name) > x.limits.MaxPathBytes || strings.ContainsRune(name, 0)) {
			t.Fatal("accepted archive path escapes or exceeds its source boundary")
		}
		if _, _, err := x.relative(x.prefix + "-other/file"); err == nil {
			t.Fatal("accepted an entry under a second archive root")
		}
		if root {
			return
		}
		if err := x.validateLink(name, target); err == nil {
			resolved := path.Join(path.Dir(name), target)
			if target == "" || strings.ContainsRune(target, 0) || filepath.IsAbs(target) || !filepath.IsLocal(resolved) || len(target) > x.limits.MaxPathBytes {
				t.Fatal("accepted symlink escapes or exceeds its source boundary")
			}
		}
	})
}
