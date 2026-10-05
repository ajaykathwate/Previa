package routing

import (
	"os"
	"strings"
	"testing"
)

// The router's deployment must keep the properties the route controller and
// the security model rely on.
func TestRouterDeploymentInvariants(t *testing.T) {
	data, err := os.ReadFile("../../deploy/traefik/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(data)
	for _, forbidden := range []string{"docker.sock", "providers.docker", "api.dashboard=true", "--api.insecure=true\n      - --entrypoints.traefik.address=:"} {
		if strings.Contains(compose, forbidden) {
			t.Fatalf("router deployment contains %q", forbidden)
		}
	}
	for _, required := range []string{
		"--providers.file.directory=/etc/traefik/dynamic",
		"--entrypoints.traefik.address=127.0.0.1:8080", // API on loopback only
		"--api.dashboard=false",
		"--entrypoints.websecure.http.tls.certresolver=preview",
		"acme-staging-v02.api.letsencrypt.org", // staging until an operator switches
		"_FILE=/run/secrets/dns_credential",    // the DNS token is never an environment value
		":/etc/traefik/dynamic:ro",
		"acme:/acme",
	} {
		if !strings.Contains(compose, required) {
			t.Fatalf("router deployment lacks %q", required)
		}
	}
}
