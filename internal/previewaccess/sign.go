package previewaccess

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// claims is what a signed value grants: user may open host until Expires.
// A token also carries the path to return to.
type claims struct {
	Host    string `json:"h"`
	User    int64  `json:"u"`
	Path    string `json:"p,omitempty"`
	Expires int64  `json:"e"`
}

// signer signs access tokens and cookies with a key derived from
// SESSION_SECRET. kind separates them, so a cookie is never a token.
type signer struct{ key []byte }

func newSigner(secret string) signer {
	key := sha256.Sum256([]byte("previa/preview-access/" + secret))
	return signer{key: key[:]}
}

func (s signer) mac(kind, payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(kind + "." + payload))
	return m.Sum(nil)
}

func (s signer) sign(kind string, c claims) string {
	data, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(data)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(kind, payload))
}

// verify returns the claims of a value signed as kind for host that has not
// expired.
func (s signer) verify(kind, value, host string, now time.Time) (claims, bool) {
	payload, signature, ok := strings.Cut(value, ".")
	if !ok || len(value) > 4096 {
		return claims{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(got, s.mac(kind, payload)) {
		return claims{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return claims{}, false
	}
	var c claims
	if json.Unmarshal(data, &c) != nil || c.Host != host || c.User <= 0 || now.Unix() >= c.Expires {
		return claims{}, false
	}
	return c, true
}
