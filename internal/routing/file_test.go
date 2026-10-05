package routing

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFileOnlyMovesForward(t *testing.T) {
	f, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := f.Version(); err != nil || v != 0 {
		t.Fatalf("empty version %d %v", v, err)
	}
	// Version 0 is written only when no file exists.
	if wrote, err := f.Write(0, []byte("{}\n")); err != nil || !wrote {
		t.Fatalf("bootstrap %v %v", wrote, err)
	}
	if wrote, _ := f.Write(0, []byte("{}\n")); wrote {
		t.Fatal("rewrote version 0")
	}
	for _, step := range []struct {
		version int64
		wrote   bool
	}{{3, true}, {2, false}, {3, false}, {5, true}, {4, false}} {
		wrote, err := f.Write(step.version, []byte("{\"v\":"+strings.Repeat("1", int(step.version))+"}\n"))
		if err != nil || wrote != step.wrote {
			t.Fatalf("write %d: wrote=%v err=%v", step.version, wrote, err)
		}
	}
	if v, _ := f.Version(); v != 5 {
		t.Fatalf("version %d", v)
	}
	data, err := os.ReadFile(f.Path())
	if err != nil || !strings.HasPrefix(string(data), versionPrefix+"5\n") || !strings.Contains(string(data), "11111") {
		t.Fatalf("content %q", data)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.Path()), "."+FileName+".tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	if _, err := f.Write(-1, nil); err == nil {
		t.Fatal("accepted a negative version")
	}
}

func TestFileConcurrentWritersKeepTheNewest(t *testing.T) {
	f, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for v := int64(1); v <= 40; v++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.Write(v, []byte("{}\n")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if v, _ := f.Version(); v != 40 {
		t.Fatalf("version %d after concurrent writes", v)
	}
}

func TestFileReplacesAnUnrecognisedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("garbage"), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := f.Version(); err != nil || v != 0 {
		t.Fatalf("version %d %v", v, err)
	}
	if wrote, err := f.Write(1, []byte("{}\n")); err != nil || !wrote {
		t.Fatal("did not replace an unrecognised file")
	}
}

func TestNewFileRequiresAnExistingAbsoluteDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", dir + "/../x", filepath.Join(dir, "missing"), file} {
		if _, err := NewFile(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
