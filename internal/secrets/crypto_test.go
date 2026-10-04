package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	k, err := ParseKeyring("1:"+testKey(t), "1")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyringSealOpenRoundTripAndBinding(t *testing.T) {
	k := testKeyring(t)
	value := []byte("sk_test_1234567890\nsecond line")
	sealed, version, err := k.Seal(42, "STRIPE_KEY", value)
	if err != nil || version != 1 {
		t.Fatalf("seal: %v version=%d", err, version)
	}
	if bytes.Contains(sealed, value) || len(sealed) != nonceBytes+len(value)+16 {
		t.Fatalf("ciphertext leaks plaintext or has wrong size %d", len(sealed))
	}
	plain, err := k.Open(42, "STRIPE_KEY", 1, sealed)
	if err != nil || !bytes.Equal(plain, value) {
		t.Fatalf("open: %q %v", plain, err)
	}
	again, _, _ := k.Seal(42, "STRIPE_KEY", value)
	if bytes.Equal(again[:nonceBytes], sealed[:nonceBytes]) || bytes.Equal(again, sealed) {
		t.Fatal("nonces must be random per value")
	}
	// The repository, name and version are authenticated: moving a value fails.
	for _, tt := range []struct {
		name    string
		repo    int64
		key     string
		version int32
		want    error
	}{
		{"other repository", 43, "STRIPE_KEY", 1, ErrUndecryptable},
		{"other name", 42, "OTHER_KEY", 1, ErrUndecryptable},
		{"unknown version", 42, "STRIPE_KEY", 2, ErrUnknownKeyVersion},
	} {
		if _, err := k.Open(tt.repo, tt.key, tt.version, sealed); !errors.Is(err, tt.want) {
			t.Fatalf("%s: %v; want %v", tt.name, err, tt.want)
		}
	}
	// Flipping any byte (nonce, ciphertext or tag) is detected.
	for i := range sealed {
		tampered := append([]byte(nil), sealed...)
		tampered[i] ^= 0x01
		if _, err := k.Open(42, "STRIPE_KEY", 1, tampered); !errors.Is(err, ErrUndecryptable) {
			t.Fatalf("tampering at byte %d not detected: %v", i, err)
		}
	}
	if _, err := k.Open(42, "STRIPE_KEY", 1, sealed[:nonceBytes+3]); !errors.Is(err, ErrUndecryptable) {
		t.Fatal("truncated ciphertext must not open")
	}
}

func TestKeyringRotationKeepsOldValuesReadable(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)
	v1, err := ParseKeyring("1:"+oldKey, "1")
	if err != nil {
		t.Fatal(err)
	}
	sealed, _, err := v1.Seal(7, "DATABASE_URL", []byte("postgres://preview"))
	if err != nil {
		t.Fatal(err)
	}
	both, err := ParseKeyring(" 1:"+oldKey+" , 2:"+newKey+" ", "2")
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := both.Open(7, "DATABASE_URL", 1, sealed); err != nil || string(plain) != "postgres://preview" {
		t.Fatalf("old value unreadable after adding a key: %v", err)
	}
	if _, version, _ := both.Seal(7, "DATABASE_URL", []byte("x")); version != 2 {
		t.Fatalf("new values must use the active version, got %d", version)
	}
	onlyNew, err := ParseKeyring("2:"+newKey, "2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onlyNew.Open(7, "DATABASE_URL", 1, sealed); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatal("removing a key before re-encryption must surface as an unknown version")
	}
}

func TestParseKeyringRejectsBadConfigurationWithoutEchoingKeys(t *testing.T) {
	good := testKey(t)
	short := base64.StdEncoding.EncodeToString([]byte("only-sixteen-byt"))
	for _, tt := range []struct{ spec, active string }{
		{"", "1"},
		{"1:" + good, ""},
		{"1:" + good, "0"},
		{"1:" + good, "2"},
		{"1:" + good, "x"},
		{good, "1"},
		{"0:" + good, "0"},
		{"-1:" + good, "1"},
		{"1:not base64!!", "1"},
		{"1:" + short, "1"},
		{"1:" + good + ",1:" + good, "1"},
		{"99999999999:" + good, "1"},
	} {
		_, err := ParseKeyring(tt.spec, tt.active)
		if err == nil {
			t.Fatalf("accepted spec=%q active=%q", tt.spec, tt.active)
		}
		if strings.Contains(err.Error(), good) || strings.Contains(err.Error(), short) {
			t.Fatalf("error echoes key material: %v", err)
		}
	}
}
