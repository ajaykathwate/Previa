package source

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestUnavailableSourceWorkspaceFailsSafelyAndCanRetry(t *testing.T) {
	for _, entry := range []item{
		file("private-source.txt", "private-file-content"),
		dir("private-directory"),
		symlink("private-link", "target"),
	} {
		t.Run(entry.name, func(t *testing.T) {
			directory := t.TempDir()
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			// A real closed workspace handle exercises file, directory and link
			// write failures without filling the host disk or changing permissions.
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			archive := tarball(t, testSHA, entry)
			_, err = extract(t.Context(), bytes.NewReader(archive), root, testSHA, DefaultLimits())
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != CodeWorkspaceFailure || !failure.Retryable || !errors.Is(err, os.ErrClosed) {
				t.Fatalf("unavailable workspace should be a retryable storage failure: %v", err)
			}
			for _, private := range []string{entry.name, entry.body, directory} {
				if private != "" && strings.Contains(err.Error(), private) {
					t.Fatal("source failure exposed workspace paths or archive content")
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed extraction wrote partial source: %d entries, %v", len(entries), err)
			}
			retryRoot, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := retryRoot.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := extract(t.Context(), bytes.NewReader(archive), retryRoot, testSHA, DefaultLimits()); err != nil {
				t.Fatalf("fresh workspace could not retry the same verified archive: %v", err)
			}
		})
	}
}
