package config

import "testing"

func TestGoogleCredentialsAreOptionalButPaired(t *testing.T) {
	for _, c := range []struct {
		id, secret string
		valid      bool
	}{{"", "", true}, {"client", "", false}, {"", "secret", false}, {"client", "secret", true}} {
		cfg := ServerConfig{GoogleOAuthClientID: c.id, GoogleOAuthClientSecret: c.secret}
		err := cfg.checkGitHubApp()
		if (err == nil) != c.valid {
			t.Errorf("Google credential pairing valid=%v: %v", c.valid, err)
		}
	}
}
