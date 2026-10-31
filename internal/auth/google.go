package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ajaykathwatee/previa/internal/platform/config"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type googleProvider interface {
	AuthCodeURL(state, nonce, verifier string) string
	Authenticate(context.Context, string, string, string) (Identity, error)
}

// GoogleOAuth verifies Google's signed ID token; no provider token is persisted.
type GoogleOAuth struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

func NewGoogleOAuth(cfg config.ServerConfig) *GoogleOAuth {
	client := &http.Client{Timeout: 10 * time.Second}
	ctx := oidc.ClientContext(context.Background(), client)
	return &GoogleOAuth{oauth: oauth2.Config{ClientID: cfg.GoogleOAuthClientID, ClientSecret: cfg.GoogleOAuthClientSecret,
		RedirectURL: strings.TrimSuffix(cfg.AppBaseURL, "/") + "/auth/google/callback", Scopes: []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token"}},
		verifier: oidc.NewVerifier("https://accounts.google.com", oidc.NewRemoteKeySet(ctx, "https://www.googleapis.com/oauth2/v3/certs"), &oidc.Config{ClientID: cfg.GoogleOAuthClientID, SupportedSigningAlgs: []string{"RS256"}}), client: client}
}
func (g *GoogleOAuth) AuthCodeURL(state, nonce, verifier string) string {
	return g.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "select_account"))
}
func (g *GoogleOAuth) Authenticate(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, g.client)
	token, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("exchange Google code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("missing Google ID token")
	}
	id, err := g.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("verify Google identity: %w", err)
	}
	if nonce == "" || id.Nonce != nonce || id.Subject == "" || len(id.Subject) > 255 {
		return Identity{}, errors.New("mismatched Google identity proof")
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Name     string `json:"name"`
	}
	if err = id.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("read Google claims: %w", err)
	}
	if !claims.Verified || claims.Email == "" {
		return Identity{}, errors.New("verified Google email required")
	}
	return Identity{GoogleSubject: id.Subject, Email: strings.ToLower(strings.TrimSpace(claims.Email)), Name: claims.Name}, nil
}
