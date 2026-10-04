package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

func TestPrepareExtractsVerifiedTreeAndConfig(t *testing.T) {
	data := tarball(t, testSHA,
		validConfig, validDockerfile,
		dir("src"), file("src/main.go", "package main\n"),
		item{name: "bin/run.sh", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o4755},
		file("README.md", "hello\n"),
		symlink("docs/README.md", "../README.md"),
	)
	preparer, workspaces := newTestPreparer(t, staticArchive(data), Limits{})
	prepared, err := preparer.Prepare(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Config.Config.Port != 8080 || prepared.Config.Config.ResourceTier != "small" || prepared.Config.Hash == "" {
		t.Fatalf("config not parsed from the tree: %+v", prepared.Config.Config)
	}
	if prepared.Dockerfile != "Dockerfile" || prepared.ContextDir != prepared.Workspace.SourceDir() {
		t.Fatalf("unexpected build inputs: %+v", prepared)
	}
	if prepared.Stats.Files != 5 || prepared.Stats.Symlinks != 1 || prepared.Stats.ArchiveBytes != int64(len(data)) {
		t.Fatalf("unexpected stats: %+v", prepared.Stats)
	}
	main, err := os.ReadFile(filepath.Join(prepared.ContextDir, "src", "main.go"))
	if err != nil || string(main) != "package main\n" {
		t.Fatalf("file content not extracted: %q %v", main, err)
	}
	info, err := os.Stat(filepath.Join(prepared.ContextDir, "bin", "run.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 || info.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("executable bit must survive and setuid must not: %v %v", info.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(prepared.ContextDir, "docs", "README.md")); err != nil || target != "../README.md" {
		t.Fatalf("in-tree symlink not recreated: %q %v", target, err)
	}
	listed, err := workspaces.List()
	if err != nil || len(listed) != 1 || !listed[0].Marked || listed[0].CommitSHA != testSHA || listed[0].PullRequestID != 7 || listed[0].Generation != 3 || listed[0].Revision != 2 {
		t.Fatalf("workspace not discoverable with its identity: %+v %v", listed, err)
	}
	if err := preparer.Release(prepared); err != nil {
		t.Fatal(err)
	}
	assertNoWorkspaces(t, workspaces)
}

func TestPrepareRejectsHostileArchives(t *testing.T) {
	big := strings.Repeat("0", 4096)
	cases := []struct {
		name   string
		data   func(t *testing.T) []byte
		limits Limits
		code   string
	}{
		{"missing commit header", func(t *testing.T) []byte { return tarball(t, "", validConfig, validDockerfile) }, Limits{}, CodeIdentityMismatch},
		{"different commit", func(t *testing.T) []byte {
			return tarball(t, strings.Repeat("f", 40), validConfig, validDockerfile)
		}, Limits{}, CodeIdentityMismatch},
		{"traversal", func(t *testing.T) []byte { return tarball(t, testSHA, file("../escape", "x")) }, Limits{}, CodeUnsafeArchive},
		{"nested traversal", func(t *testing.T) []byte { return tarball(t, testSHA, file("a/../../escape", "x")) }, Limits{}, CodeUnsafeArchive},
		{"non-canonical path", func(t *testing.T) []byte { return tarball(t, testSHA, file("a/./b", "x")) }, Limits{}, CodeUnsafeArchive},
		{"absolute path", func(t *testing.T) []byte {
			return tarball(t, testSHA, item{name: "/etc/passwd", typ: tar.TypeReg, body: "x", raw: true})
		}, Limits{}, CodeUnsafeArchive},
		{"second top-level directory", func(t *testing.T) []byte {
			return tarball(t, testSHA, item{name: "other-root/file", typ: tar.TypeReg, body: "x", raw: true})
		}, Limits{}, CodeUnsafeArchive},
		{"absolute symlink", func(t *testing.T) []byte { return tarball(t, testSHA, symlink("etc", "/etc")) }, Limits{}, CodeUnsafeArchive},
		{"escaping symlink", func(t *testing.T) []byte { return tarball(t, testSHA, symlink("a/up", "../../outside")) }, Limits{}, CodeUnsafeArchive},
		{"write through symlink", func(t *testing.T) []byte {
			return tarball(t, testSHA, dir("real"), symlink("link", "real"), file("link/planted", "x"))
		}, Limits{}, CodeUnsafeArchive},
		{"symlink over existing directory", func(t *testing.T) []byte {
			return tarball(t, testSHA, file("link/planted", "x"), symlink("link", "real"))
		}, Limits{}, CodeUnsafeArchive},
		{"hard link", func(t *testing.T) []byte {
			return tarball(t, testSHA, file("a", "x"), item{name: "b", typ: tar.TypeLink, link: testPrefix + "a"})
		}, Limits{}, CodeUnsafeArchive},
		{"character device", func(t *testing.T) []byte { return tarball(t, testSHA, item{name: "null", typ: tar.TypeChar}) }, Limits{}, CodeUnsafeArchive},
		{"fifo", func(t *testing.T) []byte { return tarball(t, testSHA, item{name: "pipe", typ: tar.TypeFifo}) }, Limits{}, CodeUnsafeArchive},
		{"duplicate file", func(t *testing.T) []byte { return tarball(t, testSHA, file("a", "1"), file("a", "2")) }, Limits{}, CodeUnsafeArchive},
		{"file then directory", func(t *testing.T) []byte { return tarball(t, testSHA, file("a", "1"), dir("a")) }, Limits{}, CodeUnsafeArchive},
		{"too many entries", func(t *testing.T) []byte {
			return tarball(t, testSHA, file("a", "1"), file("b", "2"), file("c", "3"), file("d", "4"))
		}, Limits{MaxEntries: 3}, CodeTooManyFiles},
		{"extracted size cap", func(t *testing.T) []byte { return tarball(t, testSHA, file("big", big)) }, Limits{MaxExtractedBytes: 1024}, CodeTooLarge},
		{"compression bomb", func(t *testing.T) []byte {
			return tarball(t, testSHA, file("zeros", strings.Repeat("\x00", 8<<20)))
		}, Limits{MaxExtractedBytes: 1 << 20}, CodeTooLarge},
		{"compressed size cap", func(t *testing.T) []byte { return tarball(t, testSHA, file("big", big)) }, Limits{MaxArchiveBytes: 64}, CodeTooLarge},
		{"path too long", func(t *testing.T) []byte { return tarball(t, testSHA, file(strings.Repeat("a/", 40)+"x", "1")) }, Limits{MaxPathBytes: 64}, CodeUnsafeArchive},
		{"name too long", func(t *testing.T) []byte { return tarball(t, testSHA, file(strings.Repeat("n", 256), "1")) }, Limits{}, CodeUnsafeArchive},
		{"empty archive", func(t *testing.T) []byte { return tarball(t, testSHA) }, Limits{}, CodeInvalidArchive},
		{"not gzip", func(*testing.T) []byte { return []byte("this is not an archive") }, Limits{}, CodeInvalidArchive},
		{"truncated", func(t *testing.T) []byte { return truncated(t, tarball(t, testSHA, file("big", big), validConfig)) }, Limits{}, CodeInvalidArchive},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.data(t)
			preparer, workspaces := newTestPreparer(t, archiveFunc(func(context.Context, Request) (io.ReadCloser, int64, error) {
				return io.NopCloser(bytes.NewReader(data)), -1, nil
			}), tt.limits)
			_, err := preparer.Prepare(t.Context(), testRequest())
			if CodeOf(err) != tt.code {
				t.Fatalf("code = %q (%v); want %q", CodeOf(err), err, tt.code)
			}
			assertNoWorkspaces(t, workspaces)
		})
	}
}

// truncated cuts the uncompressed tar stream mid-file and recompresses it, the
// way a dropped download looks once decompressed.
func truncated(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw[:2048+512]); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestPrepareConfigAndDockerfileFromTheSameTree(t *testing.T) {
	cases := []struct {
		name  string
		items []item
		code  string
	}{
		{"missing config", []item{validDockerfile}, CodeMissingConfig},
		{"config symlink", []item{file("real.yaml", "port: 8080\n"), symlink(".previewrc.yaml", "real.yaml"), validDockerfile}, CodeInvalidConfig},
		{"config directory", []item{dir(".previewrc.yaml"), validDockerfile}, CodeInvalidConfig},
		{"unknown key", []item{file(".previewrc.yaml", "port: 8080\nbuild_mode: automatic\n"), validDockerfile}, CodeInvalidConfig},
		{"oversized config", []item{file(".previewrc.yaml", "port: 8080\n#"+strings.Repeat("x", repoconfig.MaxConfigBytes)), validDockerfile}, CodeInvalidConfig},
		{"missing Dockerfile", []item{validConfig}, CodeMissingDockerfile},
		{"Dockerfile is a directory", []item{file(".previewrc.yaml", "dockerfile: build\n"), dir("build")}, CodeMissingDockerfile},
		{"Dockerfile through an in-tree link", []item{file(".previewrc.yaml", "dockerfile: Dockerfile\n"), file("docker/Dockerfile.real", "FROM scratch\n"), symlink("Dockerfile", "docker/Dockerfile.real")}, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			preparer, workspaces := newTestPreparer(t, staticArchive(tarball(t, testSHA, tt.items...)), Limits{})
			prepared, err := preparer.Prepare(t.Context(), testRequest())
			if CodeOf(err) != tt.code {
				t.Fatalf("code = %q (%v); want %q", CodeOf(err), err, tt.code)
			}
			if tt.code == "" {
				if err := preparer.Release(prepared); err != nil {
					t.Fatal(err)
				}
			}
			assertNoWorkspaces(t, workspaces)
		})
	}
	// Validation issues stay inspectable without exposing YAML content.
	preparer, _ := newTestPreparer(t, staticArchive(tarball(t, testSHA, file(".previewrc.yaml", "env:\n  - SECRET_TOKEN=leaked-value\n  - SECRET_TOKEN=again\n"), validDockerfile)), Limits{})
	_, err := preparer.Prepare(t.Context(), testRequest())
	var invalid *repoconfig.ValidationError
	if !errors.As(err, &invalid) || strings.Contains(err.Error(), "leaked-value") {
		t.Fatalf("validation error must be inspectable and value-free: %v", err)
	}
}

func TestPrepareCancellationAndRequestValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	data := tarball(t, testSHA, validConfig, validDockerfile, file("big", strings.Repeat("y", 1<<20)))
	preparer, workspaces := newTestPreparer(t, archiveFunc(func(context.Context, Request) (io.ReadCloser, int64, error) {
		return io.NopCloser(&cancelAfter{r: bytes.NewReader(data), after: 512, cancel: cancel}), -1, nil
	}), Limits{})
	if _, err := preparer.Prepare(ctx, testRequest()); CodeOf(err) != CodeCancelled {
		t.Fatalf("cancelled preparation: %v", err)
	}
	assertNoWorkspaces(t, workspaces)

	bad := testRequest()
	bad.CommitSHA = "main"
	if _, err := preparer.Prepare(t.Context(), bad); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("a branch name must never be fetched: %v", err)
	}
	bad = testRequest()
	bad.CommitSHA = strings.ToUpper(testSHA)
	if _, err := preparer.Prepare(t.Context(), bad); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("SHA must be canonical lowercase: %v", err)
	}
	opened := false
	declaredHuge, _ := newTestPreparer(t, archiveFunc(func(context.Context, Request) (io.ReadCloser, int64, error) {
		opened = true
		return io.NopCloser(strings.NewReader("")), 1 << 40, nil
	}), Limits{})
	if _, err := declaredHuge.Prepare(t.Context(), testRequest()); CodeOf(err) != CodeTooLarge || !opened {
		t.Fatalf("declared oversize archive: %v", err)
	}
}

type cancelAfter struct {
	r      io.Reader
	after  int
	read   int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.read >= c.after {
		c.cancel()
	}
	if len(p) > 256 {
		p = p[:256]
	}
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestWorkspacesListAndRemoveCrashLeftovers(t *testing.T) {
	root := t.TempDir()
	workspaces, err := NewWorkspaces(root)
	if err != nil {
		t.Fatal(err)
	}
	// A crash right after mkdir leaves a directory without a marker.
	orphan := filepath.Join(root, workspacePrefix+"9-g1-r1-deadbeef")
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "partial"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "unrelated"), 0o700); err != nil {
		t.Fatal(err)
	}
	listed, err := workspaces.List()
	if err != nil || len(listed) != 1 || listed[0].Marked || listed[0].Path != orphan {
		t.Fatalf("crash leftover not discoverable: %+v %v", listed, err)
	}
	if err := workspaces.Remove(listed[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("leftover not removed")
	}
	if err := workspaces.Remove(WorkspaceInfo{Path: filepath.Join(root, "unrelated")}); err == nil {
		t.Fatal("Remove must refuse paths that are not workspaces")
	}
	if err := workspaces.Remove(WorkspaceInfo{Path: "/etc"}); err == nil {
		t.Fatal("Remove must refuse paths outside the root")
	}
	if _, err := NewWorkspaces("relative/path"); err == nil {
		t.Fatal("workspace root must be absolute")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkspaces(link); err == nil {
		t.Fatal("workspace root must not be a symlink")
	}
}
