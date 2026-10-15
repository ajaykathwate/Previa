package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareContextContainmentAndIncompleteSources(t *testing.T) {
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 123\n"
	for _, tc := range []struct {
		name, config, code string
		extra              []item
	}{
		{"subfolder", "context: apps/web", "", nil},
		{"missing context", "context: absent", CodeInvalidConfig, nil},
		{"file context", "context: apps/web/Dockerfile", CodeInvalidConfig, nil},
		{"context traversal", "context: ../web", CodeInvalidConfig, nil},
		{"dockerfile escape", "context: apps/web\ndockerfile: linked", CodeMissingDockerfile, []item{symlink("apps/web/linked", "../../Dockerfile")}},
		{"submodule", "context: apps/web", CodeSubmodules, []item{file("apps/web/.gitmodules", "[submodule]\n")}},
		{"root manifest", "context: apps/web", CodeSubmodules, []item{file(".gitmodules", "[submodule \"lib\"]\n  path = apps/web/lib\n")}},
		{"unrelated submodule", "context: apps/web", "", []item{file(".gitmodules", "[submodule \"lib\"]\n  path = apps/api/lib\n")}},
		{"context alias submodule", "context: web", CodeSubmodules, []item{symlink("web", "apps/web"), file(".gitmodules", "[submodule \"lib\"]\n  path = apps/web/lib\n")}},
		{"lfs pointer", "context: apps/web", CodeLFS, []item{file("apps/web/data.bin", pointer)}},
		{"lfs CRLF", "context: apps/web", CodeLFS, []item{file("apps/web/data.bin", strings.ReplaceAll(pointer, "\n", "\r\n"))}},
		{"ordinary text", "context: apps/web", "", []item{file("apps/web/readme", "Git LFS:\n"+pointer)}},
		{"unrelated lfs", "context: apps/web", "", []item{file("data.bin", pointer)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := []item{file(".previewrc.yaml", tc.config), validDockerfile, file("apps/web/Dockerfile", "FROM scratch"), file("apps/web/index.html", "hello")}
			items = append(items, tc.extra...)
			p, workspaces := newTestPreparer(t, staticArchive(tarball(t, testSHA, items...)), Limits{})
			prepared, err := p.Prepare(t.Context(), testRequest())
			if CodeOf(err) != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			if err == nil {
				if _, err := os.Stat(filepath.Join(prepared.ContextDir, "index.html")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(prepared.ContextDir, ".previewrc.yaml")); !os.IsNotExist(err) {
					t.Fatal("root config must not be sent as part of the subfolder context")
				}
				if err := p.Release(prepared); err != nil {
					t.Fatal(err)
				}
			}
			assertNoWorkspaces(t, workspaces)
		})
	}
}
