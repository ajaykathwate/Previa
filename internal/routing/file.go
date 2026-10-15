package routing

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// FileName is the route table file inside the router's dynamic directory.
	FileName      = "previa-routes.yml"
	lockName      = ".previa-routes.lock"
	versionPrefix = "# previa-route-table-version: "
)

// File is the router's dynamic configuration file. Atomic writes never move
// the table version backwards. Only explicit recovery refreshes may update
// settings at the current version. The directory belongs to Previa alone:
// Traefik loads every YAML file in it.
type File struct {
	dir string
}

// NewFile opens the dynamic configuration directory, which must already exist.
func NewFile(dir string) (*File, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, errors.New("router configuration directory must be an absolute, clean path")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("open router configuration directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("router configuration path is not a directory")
	}
	return &File{dir: dir}, nil
}

// Path is the route table file's path.
func (f *File) Path() string { return filepath.Join(f.dir, FileName) }

// Version reads the version of the route table on disk; 0 means none yet.
func (f *File) Version() (int64, error) {
	file, err := os.Open(f.Path())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open route table file: %w", err)
	}
	defer func() { _ = file.Close() }()
	line, err := bufio.NewReader(io.LimitReader(file, 128)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read route table version: %w", err)
	}
	value, ok := strings.CutPrefix(strings.TrimSpace(line), strings.TrimSpace(versionPrefix))
	version, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if !ok || parseErr != nil || version < 0 {
		// An unrecognised file is replaced by any real table.
		return 0, nil
	}
	return version, nil
}

// Write stores content as the route table at version when that is newer than
// the file's version (version 0, a table no route has changed yet, is written
// only when there is no file). It reports whether it wrote. Writers on one
// host serialize on a lock file; the rename makes a reader see the old or the
// new file, never part of one.
func (f *File) Write(version int64, content []byte) (bool, error) {
	return f.write(version, content, false)
}

// Refresh also replaces changed configuration at the current version. Only
// startup/recovery syncs use it: older route tables remain fenced out.
func (f *File) Refresh(version int64, content []byte) (bool, error) {
	return f.write(version, content, true)
}

func (f *File) write(version int64, content []byte, refresh bool) (bool, error) {
	if version < 0 {
		return false, errors.New("route table version must not be negative")
	}
	unlock, err := lockFile(filepath.Join(f.dir, lockName))
	if err != nil {
		return false, fmt.Errorf("lock route table file: %w", err)
	}
	defer unlock()
	_, statErr := os.Stat(f.Path())
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, fmt.Errorf("inspect route table file: %w", statErr)
	}
	current, err := f.Version()
	if err != nil {
		return false, err
	}
	if version < current || (exists && version == current && !refresh) {
		return false, nil
	}
	if exists && version == current {
		data, err := os.ReadFile(f.Path())
		if err != nil {
			return false, fmt.Errorf("read route table configuration: %w", err)
		}
		if bytes.Equal(data, append([]byte(fmt.Sprintf("%s%d\n", versionPrefix, version)), content...)) {
			return false, nil
		}
	}
	// A dotted name with another extension is ignored by Traefik until renamed.
	temporary := filepath.Join(f.dir, "."+FileName+".tmp")
	out, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return false, fmt.Errorf("create route table file: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s%d\n", versionPrefix, version)
	if err == nil {
		_, err = out.Write(content)
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temporary)
		return false, fmt.Errorf("write route table file: %w", err)
	}
	if err := os.Rename(temporary, f.Path()); err != nil {
		_ = os.Remove(temporary)
		return false, fmt.Errorf("replace route table file: %w", err)
	}
	if dir, err := os.Open(f.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return true, nil
}
