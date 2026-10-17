package sampledata

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ajaykathwatee/previa/internal/secrets"
)

func testKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	keys, err := secrets.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(key), "1")
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func store(t *testing.T, s *Storage, repoID int64, data []byte) Stored {
	t.Helper()
	w, err := s.Create(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	stored, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func readAll(s *Storage, repoID int64, stored Stored) ([]byte, error) {
	r, err := s.Open(repoID, stored.Key, stored.Checksum)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func TestBaselineFilesAreEncryptedBoundAndChecked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "baselines")
	s, err := NewStorage(dir, testKeyring(t), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	dump := bytes.Repeat([]byte("INSERT INTO orders VALUES ('Preview customer A');\n"), 2000)
	stored := store(t, s, 7, dump)
	if stored.Size <= 0 || stored.Size >= int64(len(dump)) || stored.KeyVersion != 1 || len(stored.Checksum) != 64 {
		t.Fatalf("stored %+v (dump %d bytes)", stored, len(dump))
	}
	raw, err := os.ReadFile(filepath.Join(dir, stored.Key))
	if err != nil || bytes.Contains(raw, []byte("Preview customer")) {
		t.Fatal("the file must be compressed and encrypted")
	}
	if info, err := os.Stat(filepath.Join(dir, stored.Key)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", err)
	}
	if got, err := readAll(s, 7, stored); err != nil || !bytes.Equal(got, dump) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := readAll(s, 8, stored); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("another repository's baseline: %v", err)
	}
	if _, err := readAll(s, 7, Stored{Key: stored.Key, Checksum: stored.Checksum[:63] + "0"}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a checksum mismatch: %v", err)
	}
	if _, err := readAll(s, 7, Stored{Key: "00000000000000000000000000000000", Checksum: stored.Checksum}); !errors.Is(err, ErrMissing) {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err := s.Open(7, "../../etc/passwd", stored.Checksum); !errors.Is(err, ErrMissing) {
		t.Fatalf("a path outside the directory: %v", err)
	}
	raw[len(raw)/2] ^= 1
	if err := os.WriteFile(filepath.Join(dir, stored.Key), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(s, 7, stored); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a flipped bit: %v", err)
	}
	files, err := s.Files()
	if err != nil || len(files) != 1 || files[0].Key != stored.Key || files[0].Temporary {
		t.Fatalf("files %+v %v", files, err)
	}
	if err := s.Remove(stored.Key); err != nil {
		t.Fatal(err)
	}
	if files, _ := s.Files(); len(files) != 0 {
		t.Fatalf("after removal %+v", files)
	}
}

func TestBaselineFilesRespectTheSizeLimit(t *testing.T) {
	s, err := NewStorage(filepath.Join(t.TempDir(), "baselines"), testKeyring(t), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(7)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 256<<10) // incompressible
	_, _ = rand.Read(random)
	_, writeErr := w.Write(random)
	if _, commitErr := w.Commit(); !errors.Is(writeErr, ErrTooLarge) && !errors.Is(commitErr, ErrTooLarge) {
		t.Fatalf("a dump over the limit: %v / %v", writeErr, commitErr)
	}
	if files, _ := s.Files(); len(files) != 0 {
		t.Fatalf("an oversized dump left %+v", files)
	}
	aborted, err := s.Create(7)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = aborted.Write([]byte("partial"))
	aborted.Abort()
	if files, _ := s.Files(); len(files) != 0 {
		t.Fatalf("an aborted dump left %+v", files)
	}
}
