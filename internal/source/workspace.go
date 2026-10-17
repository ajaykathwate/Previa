package source

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	workspacePrefix = "previa-src-"
	markerName      = "workspace.json"
	sourceDirName   = "source"
	maxMarkerBytes  = 4096
)

// Workspaces owns one root directory of per-preparation workspaces. Every
// workspace is a fresh 0700 directory holding a marker (written before any
// extraction) and the extracted source tree, so leftovers from a crash are
// discoverable and attributable without guessing.
type Workspaces struct {
	root string
}

// WorkspaceInfo describes one workspace on disk.
type WorkspaceInfo struct {
	Path          string `json:"-"`
	PullRequestID int64  `json:"pull_request_id"`
	Generation    int64  `json:"generation"`
	Revision      int64  `json:"revision"`
	// BaselineRefreshID is set instead of the PR identity for a baseline.
	BaselineRefreshID int64     `json:"baseline_refresh_id,omitempty"`
	CommitSHA         string    `json:"commit_sha"`
	CreatedAt         time.Time `json:"created_at"`
	PID               int       `json:"pid"`
	// Marked is false when the marker is missing or unreadable, for example after
	// a crash immediately after the directory was created.
	Marked bool `json:"-"`
}

// SourceDir is the extracted tree inside the workspace.
func (w WorkspaceInfo) SourceDir() string { return filepath.Join(w.Path, sourceDirName) }

// NewWorkspaces creates (if needed) an absolute, non-symlink root with 0700 mode.
func NewWorkspaces(root string) (*Workspaces, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("source workspace root must be an absolute, clean path")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create source workspace root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect source workspace root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("source workspace root must be a real directory")
	}
	return &Workspaces{root: root}, nil
}

// Root returns the workspace root directory.
func (w *Workspaces) Root() string { return w.root }

func (w *Workspaces) create(request Request, now time.Time) (WorkspaceInfo, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return WorkspaceInfo{}, fmt.Errorf("generate workspace name: %w", err)
	}
	name := fmt.Sprintf("%s%d-g%d-r%d-%s", workspacePrefix, request.PullRequestID, request.Generation, request.Revision, hex.EncodeToString(suffix))
	if request.BaselineRefreshID > 0 {
		name = fmt.Sprintf("%sbaseline%d-%s", workspacePrefix, request.BaselineRefreshID, hex.EncodeToString(suffix))
	}
	info := WorkspaceInfo{Path: filepath.Join(w.root, name), PullRequestID: request.PullRequestID, Generation: request.Generation, Revision: request.Revision,
		BaselineRefreshID: request.BaselineRefreshID, CommitSHA: request.CommitSHA, CreatedAt: now.UTC(), PID: os.Getpid(), Marked: true}
	if err := os.Mkdir(info.Path, 0o700); err != nil {
		return WorkspaceInfo{}, fmt.Errorf("create workspace: %w", err)
	}
	marker, err := json.Marshal(info)
	if err != nil {
		return info, fmt.Errorf("encode workspace marker: %w", err)
	}
	if err := writeExclusive(filepath.Join(info.Path, markerName), marker); err != nil {
		return info, fmt.Errorf("write workspace marker: %w", err)
	}
	if err := os.Mkdir(info.SourceDir(), 0o755); err != nil {
		return info, fmt.Errorf("create workspace source directory: %w", err)
	}
	return info, nil
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// List returns every workspace under the root, oldest first, including ones
// whose marker is missing. Housekeeping uses it to find crash leftovers.
func (w *Workspaces) List() ([]WorkspaceInfo, error) {
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return nil, fmt.Errorf("list source workspaces: %w", err)
	}
	var result []WorkspaceInfo
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), workspacePrefix) || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		path := filepath.Join(w.root, entry.Name())
		info := WorkspaceInfo{Path: path}
		if marker, ok := readMarker(path); ok {
			info = marker
			info.Path, info.Marked = path, true
		} else if stat, err := entry.Info(); err == nil {
			info.CreatedAt = stat.ModTime().UTC()
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func readMarker(dir string) (WorkspaceInfo, bool) {
	f, err := os.OpenFile(filepath.Join(dir, markerName), os.O_RDONLY|syscallNoFollow, 0)
	if err != nil {
		return WorkspaceInfo{}, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes+1))
	if err != nil || len(data) > maxMarkerBytes {
		return WorkspaceInfo{}, false
	}
	var info WorkspaceInfo
	if json.Unmarshal(data, &info) != nil || info.PullRequestID <= 0 && info.BaselineRefreshID <= 0 {
		return WorkspaceInfo{}, false
	}
	return info, true
}

// Remove deletes one workspace. It refuses paths outside the root and symlinks;
// os.RemoveAll removes links inside the tree without following them.
func (w *Workspaces) Remove(info WorkspaceInfo) error {
	if filepath.Dir(info.Path) != w.root || !strings.HasPrefix(filepath.Base(info.Path), workspacePrefix) {
		return errors.New("refusing to remove a path outside the source workspace root")
	}
	stat, err := os.Lstat(info.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect workspace: %w", err)
	}
	if !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove a workspace that is not a real directory")
	}
	if err := os.RemoveAll(info.Path); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}
	return nil
}
