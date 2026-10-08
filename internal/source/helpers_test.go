package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"testing"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"
const testPrefix = "owner-repo-0123456/"

// item is one archive entry; names are relative to the archive's top directory
// unless raw is set.
type item struct {
	name string
	typ  byte
	body string
	link string
	mode int64
	raw  bool
}

func dir(name string) item        { return item{name: name, typ: tar.TypeDir, mode: 0o755} }
func file(name, body string) item { return item{name: name, typ: tar.TypeReg, body: body, mode: 0o644} }
func symlink(name, to string) item {
	return item{name: name, typ: tar.TypeSymlink, link: to, mode: 0o777}
}

var validConfig = file(".previewrc.yaml", "port: 8080\nresource_tier: small\n")
var validDockerfile = file("Dockerfile", "FROM scratch\n")

// tarball writes a git-archive-shaped gzip tarball: a global header naming sha
// (omitted when sha is empty), the top directory, then the items.
func tarball(t *testing.T, sha string, items ...item) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	if sha != "" {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: testPrefix, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		name := it.name
		if !it.raw {
			name = testPrefix + it.name
		}
		if it.typ == tar.TypeDir && name[len(name)-1] != '/' {
			name += "/"
		}
		hdr := &tar.Header{Typeflag: it.typ, Name: name, Linkname: it.link, Mode: it.mode, Size: int64(len(it.body)), Format: tar.FormatPAX}
		if it.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if it.typ == tar.TypeReg {
			if _, err := io.WriteString(tw, it.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// archiveFunc adapts a function to ArchiveSource.
type archiveFunc func(context.Context, Request) (io.ReadCloser, int64, error)

func (f archiveFunc) Open(ctx context.Context, r Request) (io.ReadCloser, int64, error) {
	return f(ctx, r)
}

func staticArchive(data []byte) archiveFunc {
	return func(context.Context, Request) (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
}

func testRequest() Request {
	return Request{InstallationID: 99, BaseRepoID: 100, CommitSHA: testSHA, PullRequestID: 7, Generation: 3, Revision: 2}
}

func newTestPreparer(t *testing.T, archives ArchiveSource, limits Limits) (*Preparer, *Workspaces) {
	t.Helper()
	workspaces, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := NewPreparer(archives, workspaces, limits, repoconfig.Platform{Limits: repoconfig.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	return preparer, workspaces
}

func assertNoWorkspaces(t *testing.T, w *Workspaces) {
	t.Helper()
	left, err := w.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("failed preparation left %d workspace(s) behind", len(left))
	}
}
