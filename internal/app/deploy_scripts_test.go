package app

import (
	"os/exec"
	"strings"
	"testing"
)

// The production scripts run directly: backup.sh calls deletion-journal.sh
// and systemd starts it. This checkout is often edited where Git records no
// executable bit (core.fileMode=false on /mnt/c), so check the index itself.
func TestDeployScriptsAreExecutableInGit(t *testing.T) {
	out, err := exec.Command("git", "-C", "../..", "ls-files", "-s", "--", "deploy/production/*.sh").Output()
	if err != nil {
		t.Skipf("no Git index to check: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Skip("no tracked production scripts")
	}
	for _, line := range lines {
		meta, path, _ := strings.Cut(line, "\t")
		if mode := strings.Fields(meta)[0]; mode != "100755" {
			t.Errorf("%s is %s in Git, so it cannot run on the server; run: git update-index --chmod=+x %s", path, mode, path)
		}
	}
}
