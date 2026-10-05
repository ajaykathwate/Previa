package routing

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/environments"
)

var testSettings = Settings{BaseDomain: "preview.example.com", EntryPoint: "websecure", UnavailableURL: "https://previa.example.com/previews/unavailable"}

func TestRenderRoutesActiveVersionsAndRedirectsTheRest(t *testing.T) {
	table := environments.RouteTable{Version: 7, Routes: []environments.Route{
		{EnvironmentID: 3, Hostname: "pr-1-1-aaaaaaaa", Generation: 4, Revision: 2, Backend: "previa-pr1-g4-r2-5", Port: 8080},
		{EnvironmentID: 9, Hostname: "pr-2-1-bbbbbbbb", Generation: 1, Revision: 1, Backend: "previa-pr2-g1-r1-6", Port: 3000},
	}}
	content, err := Render(testSettings, table)
	if err != nil {
		t.Fatal(err)
	}
	var config dynamicConfig
	if err := json.Unmarshal(content, &config); err != nil {
		t.Fatal(err)
	}
	r := config.HTTP.Routers["preview-3"]
	if r.Rule != "Host(`pr-1-1-aaaaaaaa.preview.example.com`)" || r.Service != "preview-3-g4-r2" || len(r.EntryPoints) != 1 || r.EntryPoints[0] != "websecure" {
		t.Fatalf("router %+v", r)
	}
	if got := config.HTTP.Services["preview-3-g4-r2"].LoadBalancer; len(got.Servers) != 1 || got.Servers[0].URL != "http://previa-pr1-g4-r2-5:8080" || !got.PassHostHeader {
		t.Fatalf("service %+v", got)
	}
	if _, ok := config.HTTP.Routers["preview-9"]; !ok {
		t.Fatal("second route missing")
	}
	fallback := config.HTTP.Routers[unavailableName]
	if fallback.Priority != 1 || fallback.Service != "noop@internal" || fallback.Middlewares[0] != unavailableName {
		t.Fatalf("fallback %+v", fallback)
	}
	redirect := config.HTTP.Middlewares[unavailableName].RedirectRegex
	if redirect == nil || redirect.Replacement != testSettings.UnavailableURL {
		t.Fatalf("redirect %+v", redirect)
	}
	// Routers must not carry their own TLS block: it would replace the entry
	// point's wildcard certificate resolver.
	if strings.Contains(string(content), `"tls"`) {
		t.Fatal("rendered a router TLS block")
	}
	// The fallback rule matches subdomains of the base domain only.
	rule := regexp.MustCompile("`(.*)`").FindStringSubmatch(fallback.Rule)[1]
	host := regexp.MustCompile(rule)
	for name, want := range map[string]bool{
		"x.preview.example.com": true, "a.b.preview.example.com": true, "preview.example.com": false,
		"xpreview.example.com": false, "x.preview.example.com.evil.test": false, "x.previewXexample.com": false,
	} {
		if host.MatchString(name) != want {
			t.Fatalf("fallback match %q = %v", name, !want)
		}
	}
}

func TestRenderIsDeterministicAndEmptyTableStillRedirects(t *testing.T) {
	first, err := Render(testSettings, environments.RouteTable{})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Render(testSettings, environments.RouteTable{})
	if string(first) != string(second) || !strings.Contains(string(first), unavailableName) || strings.Contains(string(first), `"services"`) {
		t.Fatalf("empty table rendered %s", first)
	}
}

func TestRenderRejectsInvalidInput(t *testing.T) {
	bad := []environments.Route{
		{EnvironmentID: 1, Hostname: "Bad`Host", Backend: "b", Port: 80},
		{EnvironmentID: 1, Hostname: "ok", Backend: "b:1@evil", Port: 80},
		{EnvironmentID: 1, Hostname: "ok", Backend: "b", Port: 0},
		{EnvironmentID: 0, Hostname: "ok", Backend: "b", Port: 80},
	}
	for _, route := range bad {
		if _, err := Render(testSettings, environments.RouteTable{Version: 1, Routes: []environments.Route{route}}); err == nil {
			t.Fatalf("rendered invalid route %+v", route)
		}
	}
	for _, s := range []Settings{
		{BaseDomain: "Preview.example.com", EntryPoint: "websecure", UnavailableURL: testSettings.UnavailableURL},
		{BaseDomain: "localhost", EntryPoint: "websecure", UnavailableURL: testSettings.UnavailableURL},
		{BaseDomain: "preview.example.com", EntryPoint: "web secure", UnavailableURL: testSettings.UnavailableURL},
		{BaseDomain: "preview.example.com", EntryPoint: "websecure", UnavailableURL: "/previews/unavailable"},
		{BaseDomain: "preview.example.com", EntryPoint: "websecure", UnavailableURL: "https://x.test/$1"},
		{BaseDomain: "preview.example.com", EntryPoint: "websecure", UnavailableURL: "https://user:pw@x.test/"},
	} {
		if _, err := Render(s, environments.RouteTable{}); err == nil {
			t.Fatalf("accepted settings %+v", s)
		}
	}
}
