package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestGoogleOAuthVerifiesSignedIdentityAndPKCE(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "test-key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "nonce", "audience", "issuer", "expired", "unverified", "missing-sub", "missing-token", "bad-signature", "exchange-error"} {
		t.Run(mode, func(t *testing.T) {
			claims := map[string]any{"iss": "https://accounts.google.com", "aud": "client", "sub": "google-subject", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": "expected", "email": "reviewer@example.test", "email_verified": true, "name": "Client Reviewer"}
			switch mode {
			case "nonce":
				claims["nonce"] = "wrong"
			case "audience":
				claims["aud"] = "other"
			case "issuer":
				claims["iss"] = "https://evil.test"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "unverified":
				claims["email_verified"] = false
			case "missing-sub":
				claims["sub"] = ""
			}
			payload, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "bad-signature" {
				raw = raw[:len(raw)-10] + "0123456789"
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/keys":
					if e := json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}}); e != nil {
						t.Error(e)
					}
				case "/token":
					if mode == "exchange-error" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if e := r.ParseForm(); e != nil {
						t.Error(e)
					}
					if r.Form.Get("code_verifier") != "verifier" {
						t.Error("PKCE verifier missing")
					}
					value := map[string]any{"access_token": "discard-this", "token_type": "Bearer", "id_token": raw}
					if mode == "missing-token" {
						delete(value, "id_token")
					}
					if e := json.NewEncoder(w).Encode(value); e != nil {
						t.Error(e)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			g := NewGoogleOAuth(config.ServerConfig{GoogleOAuthClientID: "client", GoogleOAuthClientSecret: "secret", AppBaseURL: "http://localhost"})
			g.client = server.Client()
			g.oauth.Endpoint.TokenURL = server.URL + "/token"
			ctx := oidc.ClientContext(context.Background(), server.Client())
			g.verifier = oidc.NewVerifier("https://accounts.google.com", oidc.NewRemoteKeySet(ctx, server.URL+"/keys"), &oidc.Config{ClientID: "client"})
			identity, err := g.Authenticate(t.Context(), "code", "expected", "verifier")
			if mode != "valid" {
				if err == nil {
					t.Fatal("invalid Google proof accepted")
				}
				return
			}
			if err != nil || identity.GoogleSubject != "google-subject" || identity.Email != "reviewer@example.test" || identity.ID != 0 {
				t.Fatalf("identity: %+v %v", identity, err)
			}
			authURL, err := url.Parse(g.AuthCodeURL("state", "nonce", "verifier"))
			if err != nil {
				t.Fatal(err)
			}
			q := authURL.Query()
			if q.Get("nonce") != "nonce" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier("verifier") {
				t.Fatal("missing nonce/PKCE")
			}
		})
	}
}
