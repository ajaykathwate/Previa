package sampledata

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// An owner sidecar exists before dump bytes are written. It lets explicit
// erasure remove interrupted/unrecorded files as well as committed baselines.
func (s *Storage) recordOwner(key string, repo int64) error {
	if repo <= 0 {
		return errors.New("baseline owner must be a repository")
	}
	f, err := os.OpenFile(s.path(key)+".owner", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create baseline owner record: %w", err)
	}
	_, writeErr := fmt.Fprint(f, strconv.FormatInt(repo, 10))
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err == nil {
		err = syncDir(s.dir)
	}
	if err != nil {
		return fmt.Errorf("persist baseline owner record: %w", err)
	}
	return nil
}

func (s *Storage) removeOwner(key string) error {
	if err := os.Remove(s.path(key) + ".owner"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove baseline owner record: %w", err)
	}
	return syncDir(s.dir)
}

func (s *Storage) RemoveRepository(repo int64) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("list baseline ownership: %w", err)
	}
	for _, entry := range entries {
		key, owner := strings.CutSuffix(entry.Name(), ".owner")
		if !owner || !entry.Type().IsRegular() || !validKey(key) {
			continue
		}
		data, err := os.ReadFile(s.path(key) + ".owner")
		if err != nil {
			return fmt.Errorf("read baseline ownership: %w", err)
		}
		if string(data) != strconv.FormatInt(repo, 10) {
			continue
		}
		if err = s.RemoveTemporary(key); err != nil {
			return err
		}
		if err = s.Remove(key); err != nil {
			return err
		}
	}
	return nil
}
