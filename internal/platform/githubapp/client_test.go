package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/go-github/v91/github"
)

func testClient(t *testing.T) (*Client, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := New(config.ServerConfig{GitHubAppID: 123, GitHubAppPrivateKeyPath: path})
	if err != nil {
		t.Fatal(err)
	}
	return client, key
}
func TestInstallationTokenJWTCacheRefreshAndIsolation(t *testing.T) {
	c, key := testClient(t)
	var mint atomic.Int64
	var expired atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			token, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), func(token *jwt.Token) (any, error) {
				if token.Method.Alg() != "RS256" {
					return nil, fmt.Errorf("unexpected algorithm")
				}
				return &key.PublicKey, nil
			})
			if err != nil || !token.Valid {
				t.Error("invalid signed App JWT")
				w.WriteHeader(401)
				return
			}
			claims := token.Claims.(jwt.MapClaims)
			if claims["iss"] != "123" {
				t.Error("wrong App issuer")
			}
			n := mint.Add(1)
			expiry := time.Now().Add(time.Hour)
			if expired.Load() {
				expiry = time.Now().Add(30 * time.Second)
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("installation-token-%d", n), "expires_at": expiry.Format(time.RFC3339)}); err != nil {
				t.Error(err)
			}
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "token installation-token-") {
			t.Error("missing installation authentication")
			w.WriteHeader(401)
			return
		}
		if _, err := fmt.Fprint(w, `{"id":456,"full_name":"owner/repo"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	call := func(id int64) {
		client, err := c.InstallationClient(id)
		if err != nil {
			t.Error(err)
			return
		}
		c.mu.Lock()
		c.installations[id].BaseURL = server.URL
		c.mu.Unlock()
		client, err = client.Clone(github.WithURLs(new(server.URL+"/"), nil))
		if err != nil {
			t.Error(err)
			return
		}
		repo, _, err := client.Repositories.GetByID(t.Context(), 456)
		if err != nil || repo.GetID() != 456 {
			t.Errorf("authenticated metadata: %v", err)
		}
	}
	// Configure transport before concurrent requests; its public fields are immutable in production.
	call(99)
	call(99)
	if mint.Load() != 1 {
		t.Fatal("token was not reused")
	}
	c.Invalidate(99)
	expired.Store(true)
	call(99)
	call(99)
	if mint.Load() != 3 {
		t.Fatal("near-expiry token did not refresh")
	}
	expired.Store(false)
	c.Invalidate(99)
	call(99)
	call(100)
	before := mint.Load()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			client, err := c.InstallationClient(99)
			if err != nil {
				t.Error(err)
				return
			}
			client, err = client.Clone(github.WithURLs(new(server.URL+"/"), nil))
			if err != nil {
				t.Error(err)
				return
			}
			if _, _, err := client.Repositories.GetByID(t.Context(), 456); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if mint.Load() != before {
		t.Fatal("concurrent cached calls minted unnecessary tokens")
	}
	if c.installations[99] == c.installations[100] {
		t.Fatal("installation caches are shared")
	}

	c.Invalidate(99)
	c.Invalidate(100)
	clients := make([]*github.Client, 0, 2)
	for _, id := range []int64{99, 100} {
		client, err := c.InstallationClient(id)
		if err != nil {
			t.Fatal(err)
		}
		c.installations[id].BaseURL = server.URL
		client, err = client.Clone(github.WithURLs(new(server.URL+"/"), nil))
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
	}
	before = mint.Load()
	for i := range 12 {
		wg.Go(func() {
			if _, _, err := clients[i%2].Repositories.GetByID(t.Context(), 456); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if mint.Load() != before+2 {
		t.Fatal("concurrent first use did not mint exactly one token per installation")
	}
}
func TestPrivateKeyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe.pem")
	if err := os.WriteFile(path, []byte("not-a-key"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.ServerConfig{GitHubAppID: 1, GitHubAppPrivateKeyPath: path}); err == nil {
		t.Fatal("unsafe private key permissions accepted")
	}
}

// Opt-in proof uses a real installation token without printing it or credentials.
func TestRealInstallationRepositoryAccess(t *testing.T) {
	if os.Getenv("TEST_GITHUB_INSTALLATION_ID") == "" {
		t.Skip("set TEST_GITHUB_INSTALLATION_ID and TEST_GITHUB_REPO_ID with server App configuration for real GitHub proof")
	}
	cfg, err := config.LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	installationID := mustID(t, os.Getenv("TEST_GITHUB_INSTALLATION_ID"))
	repoID := mustID(t, os.Getenv("TEST_GITHUB_REPO_ID"))
	client, err := c.InstallationClient(installationID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	repo, _, err := client.Repositories.GetByID(ctx, repoID)
	if err != nil {
		t.Fatal("real installation-token repository API call failed (provider details omitted)")
	}
	if repo.GetID() != repoID {
		t.Fatal("GitHub returned a different repository")
	}
	t.Log("Real installation token minted and authenticated repository metadata fetched successfully")
}
func mustID(t *testing.T, value string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		t.Fatal("test GitHub IDs must be positive integers")
	}
	return id
}
