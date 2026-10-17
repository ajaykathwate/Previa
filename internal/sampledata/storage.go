package sampledata

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Keyring encrypts baseline files with the platform's secret keys, bound to
// their repository and purpose; *secrets.Keyring implements it.
type Keyring interface {
	EncryptStream(w io.Writer, repoID int64, purpose string) (io.WriteCloser, int32, error)
	DecryptStream(r io.Reader, repoID int64, purpose string) (io.Reader, int32, error)
}

var (
	// ErrTooLarge: a dump exceeded the baseline size limit
	// (PREVIEW_BASELINE_MAX_SIZE, after compression).
	ErrTooLarge = errors.New("baseline exceeds the size limit")
	// ErrMissing: the baseline's file is gone.
	ErrMissing = errors.New("baseline file is missing")
	// ErrCorrupt: the file does not open as this repository's baseline, or
	// its checksum differs from the one recorded.
	ErrCorrupt = errors.New("baseline file is corrupt")
)

const tempSuffix = ".tmp"

// Storage keeps baseline dumps as compressed, encrypted files in one worker
// directory. Files are derived data: a lost one is rebuilt from its branch.
type Storage struct {
	dir   string
	keys  Keyring
	limit int64
}

// NewStorage creates (if needed) a 0700 directory for baselines; limit is the
// most a stored file may hold.
func NewStorage(dir string, keys Keyring, limit int64) (*Storage, error) {
	if keys == nil || limit <= 0 || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, errors.New("baseline storage requires the secret keys, a size limit and an absolute, clean directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create baseline directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("the baseline directory must be a real directory")
	}
	return &Storage{dir: dir, keys: keys, limit: limit}, nil
}

func purpose(key string) string { return "previa:baseline:" + key }

func (s *Storage) path(key string) string { return filepath.Join(s.dir, key) }

// Stored is one committed baseline file.
type Stored struct {
	Key, Checksum string
	Size          int64
	KeyVersion    int32
}

// Writer stores one dump: write it, then Commit or Abort.
type Writer struct {
	s        *Storage
	key      string
	file     *os.File
	counted  *limitedWriter
	sealed   io.WriteCloser
	gzip     *gzip.Writer
	version  int32
	finished bool
}

// Create starts storing one dump of repoID.
func (s *Storage) Create(repoID int64) (*Writer, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("name a baseline file: %w", err)
	}
	key := hex.EncodeToString(raw)
	file, err := os.OpenFile(s.path(key)+tempSuffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create baseline file: %w", err)
	}
	counted := &limitedWriter{w: file, limit: s.limit, hash: sha256.New()}
	sealed, version, err := s.keys.EncryptStream(counted, repoID, purpose(key))
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	compressed, err := gzip.NewWriterLevel(sealed, gzip.BestSpeed)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return &Writer{s: s, key: key, file: file, counted: counted, sealed: sealed, gzip: compressed, version: version}, nil
}

// Write compresses and encrypts dump bytes into the file; it fails with
// ErrTooLarge once the file exceeds the limit.
func (w *Writer) Write(p []byte) (int, error) { return w.gzip.Write(p) }

// Commit finishes the file and makes it visible under its key.
func (w *Writer) Commit() (Stored, error) {
	if w.finished {
		return Stored{}, errors.New("baseline file already finished")
	}
	w.finished = true
	err := errors.Join(w.gzip.Close(), w.sealed.Close())
	if err == nil {
		err = w.file.Sync()
	}
	err = errors.Join(err, w.file.Close())
	if err == nil {
		err = os.Rename(w.file.Name(), w.s.path(w.key))
	}
	if err == nil {
		err = syncDir(w.s.dir)
	}
	if err != nil {
		_ = os.Remove(w.file.Name())
		if errors.Is(err, ErrTooLarge) {
			return Stored{}, ErrTooLarge
		}
		return Stored{}, fmt.Errorf("store baseline file: %w", err)
	}
	return Stored{Key: w.key, Checksum: hex.EncodeToString(w.counted.hash.Sum(nil)), Size: w.counted.n, KeyVersion: w.version}, nil
}

// Abort discards the file.
func (w *Writer) Abort() {
	if w.finished {
		return
	}
	w.finished = true
	_ = w.file.Close()
	_ = os.Remove(w.file.Name())
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// limitedWriter counts and hashes what reaches the file and refuses more
// than limit bytes.
type limitedWriter struct {
	w     io.Writer
	limit int64
	n     int64
	hash  hash.Hash
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.limit {
		return 0, ErrTooLarge
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	l.hash.Write(p[:n])
	return n, err
}

// Open opens a stored dump of repoID, decrypted and decompressed. Reading it
// to the end verifies the recorded checksum: it then fails with ErrCorrupt
// rather than returning io.EOF if the file differs.
func (s *Storage) Open(repoID int64, key, checksum string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, ErrMissing
	}
	file, err := os.Open(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, fmt.Errorf("open baseline file: %w", err)
	}
	hashed := &hashingReader{r: file, hash: sha256.New()}
	plain, _, err := s.keys.DecryptStream(hashed, repoID, purpose(key))
	if err != nil {
		_ = file.Close()
		return nil, ErrCorrupt
	}
	decompressed, err := gzip.NewReader(plain)
	if err != nil {
		_ = file.Close()
		return nil, ErrCorrupt
	}
	return &dumpReader{file: file, hashed: hashed, plain: plain, gzip: decompressed, checksum: checksum}, nil
}

type hashingReader struct {
	r    io.Reader
	hash hash.Hash
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.hash.Write(p[:n])
	return n, err
}

type dumpReader struct {
	file     *os.File
	hashed   *hashingReader
	plain    io.Reader
	gzip     *gzip.Reader
	checksum string
	err      error
}

func (d *dumpReader) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	n, err := d.gzip.Read(p)
	switch {
	case errors.Is(err, io.EOF):
		// Hash every remaining byte, then compare with the record.
		if _, drainErr := io.Copy(io.Discard, d.plain); drainErr != nil {
			d.err = ErrCorrupt
		} else if _, drainErr := io.Copy(io.Discard, d.hashed); drainErr != nil {
			d.err = ErrCorrupt
		} else if hex.EncodeToString(d.hashed.hash.Sum(nil)) != d.checksum {
			d.err = ErrCorrupt
		} else {
			d.err = io.EOF
		}
		return n, d.err
	case err != nil:
		d.err = ErrCorrupt
		return n, d.err
	}
	return n, nil
}

func (d *dumpReader) Close() error { return d.file.Close() }

// Remove deletes a stored dump; a missing one counts as removed.
func (s *Storage) Remove(key string) error {
	if !validKey(key) {
		return nil
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove baseline file: %w", err)
	}
	return nil
}

// StoredFile is a file in the baseline directory.
type StoredFile struct {
	Key       string
	Temporary bool // an unfinished file
	Modified  time.Time
}

// Files lists the directory, for cleanup of files no baseline records.
func (s *Storage) Files() ([]StoredFile, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list baseline files: %w", err)
	}
	var files []StoredFile
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		name := entry.Name()
		key, temporary := strings.CutSuffix(name, tempSuffix)
		if !validKey(key) {
			continue
		}
		files = append(files, StoredFile{Key: key, Temporary: temporary, Modified: info.ModTime()})
	}
	return files, nil
}

// RemoveTemporary deletes an unfinished file.
func (s *Storage) RemoveTemporary(key string) error {
	if !validKey(key) {
		return nil
	}
	if err := os.Remove(s.path(key) + tempSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unfinished baseline file: %w", err)
	}
	return nil
}

// validKey keeps every name inside the directory.
func validKey(key string) bool {
	if len(key) != 32 {
		return false
	}
	for _, r := range key {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
