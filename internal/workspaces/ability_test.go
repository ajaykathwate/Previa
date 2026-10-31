package workspaces

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Test data settings, turning it off and deleting baselines need managers;
// Refresh baseline and refresh logs need builders; the tab itself is a read.
func TestTestDataPathsNeedTheirAbility(t *testing.T) {
	for _, c := range []struct {
		method, path, form string
		ability            string
		source             bool
	}{
		{"GET", "/repositories/1/test-data", "", "read", false},
		{"POST", "/repositories/1/test-data/settings", "enabled=on", "manage", true},
		{"POST", "/repositories/1/test-data/off", "", "manage", false},
		{"POST", "/repositories/1/test-data/delete", "", "manage", false},
		{"POST", "/repositories/1/test-data/refresh", "", "operate", true},
		{"GET", "/repositories/1/test-data/refreshes/3/logs", "", "operate", true},
		{"GET", "/repositories/1/test-data/refreshes/3/logs/download", "", "operate", true},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.form))
		if c.form != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.PostForm, _ = url.ParseQuery(c.form)
		}
		ability, source := repositoryAbility(r)
		if ability != c.ability || source != c.source {
			t.Errorf("%s %s: %s %v, want %s %v", c.method, c.path, ability, source, c.ability, c.source)
		}
	}
}
