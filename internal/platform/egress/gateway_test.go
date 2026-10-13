package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

func TestNamedGatewayDoesNotSelectOrDetachSharedProxy(t *testing.T) {
	const name = "previa-egress-test-isolated"
	var detached string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/networks/preview") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"Containers": map[string]any{
				"shared-id": map[string]string{"Name": ContainerName, "IPv4Address": "172.20.0.2/16"},
				"test-id":   map[string]string{"Name": name, "IPv4Address": "172.20.0.3/16"},
			}})
		case strings.HasSuffix(r.URL.Path, "/networks/preview/disconnect") && r.Method == http.MethodPost:
			var body struct{ Container string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			detached = body.Container
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	docker, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.52"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := docker.Close(); err != nil {
			t.Error(err)
		}
	})
	isolated, err := NewNamedGateway(docker, "egress:test", name)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := NewGateway(docker, "egress:test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		gateway *Gateway
		address string
	}{{isolated, "172.20.0.3"}, {shared, "172.20.0.2"}} {
		address, ok, err := tc.gateway.Address(context.Background(), "preview")
		if err != nil || !ok || address != netip.MustParseAddr(tc.address) {
			t.Fatalf("%s address: %v %t %v", tc.gateway.Name(), address, ok, err)
		}
	}
	if err := isolated.Detach(context.Background(), "preview"); err != nil {
		t.Fatal(err)
	}
	if detached != name {
		t.Fatalf("detached %q instead of the isolated proxy", detached)
	}
}

func TestNamedGatewayRejectsInvalidIdentity(t *testing.T) {
	docker, err := client.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := docker.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"", "../previa-egress", "-proxy", "proxy/name", "proxy\nname", strings.Repeat("a", 64)} {
		if _, err := NewNamedGateway(docker, "egress:test", name); err == nil {
			t.Errorf("accepted invalid gateway name %q", name)
		}
	}
}
