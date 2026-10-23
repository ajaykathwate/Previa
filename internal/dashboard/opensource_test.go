package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/ajaykathwatee/previa/internal/repoconfig"
	opensourcepages "github.com/ajaykathwatee/previa/web/pages/opensource"
	"github.com/go-chi/chi/v5"
)

func TestOpenSourceTestedIsPublicAndLinkedFromHome(t *testing.T) {
	pass := func(next http.Handler) http.Handler { return next }
	for name, users := range map[string]UserSession{"signed out": anonymousSession{}, "signed in": signedIn{}} {
		r := chi.NewRouter()
		Mount(r, users, pass)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/open-source-tests", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s /open-source-tests: %d", name, w.Code)
		}
		for _, want := range []string{"<title>Open Source Tested | Previa</title>", `id="repo-mermaid-live-editor"`, `id="coverage"`, ">Open Source Tested</a>", `href="/open-source-tests"`} {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("%s /open-source-tests lacks %q", name, want)
			}
		}
	}

	r := chi.NewRouter()
	Mount(r, anonymousSession{}, pass)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{`id="open-source-tests"`, `href="/open-source-tests#repo-umami"`, `href="/open-source-tests"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("home page lacks %q", want)
		}
	}
}

// Every configuration Open Source Tested publishes is accepted by Previa's own
// parser with the default service catalog, so the page never shows a
// configuration Previa would reject.
func TestOpenSourceTestConfigsPassPreviasRules(t *testing.T) {
	catalog, err := config.ParseServiceCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	platform := repoconfig.Platform{Limits: repoconfig.DefaultLimits(), Services: catalog}
	for _, repo := range opensourcepages.Repos() {
		if _, err := repoconfig.ParseFor([]byte(repo.Config), platform); err != nil {
			t.Errorf("%s: Previa rejects its .previewrc.yaml: %v\n%s", repo.ID, err, repo.Config)
		}
	}
}
