package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	keyBytes   = 32 // AES-256
	nonceBytes = 12 // standard GCM nonce, random per value
)

// ErrUndecryptable hides why a value failed to open: wrong key, tampering, or a
// value moved to another repository, name or key version all look the same.
var ErrUndecryptable = errors.New("secret value could not be decrypted with the configured keys")

// ErrUnknownKeyVersion means the value was sealed with a key that is no longer
// configured. Keep old keys until re-encryption reports zero rows for them.
var ErrUnknownKeyVersion = errors.New("secret value uses a key version that is not configured")

// Keyring holds versioned AES-256-GCM keys. New values always use the active
// version; any configured version can open existing values, so rotation can
// complete without downtime.
type Keyring struct {
	keys   map[int32]cipher.AEAD
	active int32
}

// ParseKeyring reads SECRETS_ENCRYPTION_KEYS ("1:<base64 32 bytes>,2:<…>") and
// SECRETS_ACTIVE_KEY_VERSION. Error messages never contain key material.
func ParseKeyring(spec, activeVersion string) (*Keyring, error) {
	active, err := parseVersion(strings.TrimSpace(activeVersion))
	if err != nil {
		return nil, errors.New("SECRETS_ACTIVE_KEY_VERSION must be a positive integer")
	}
	k := &Keyring{keys: map[int32]cipher.AEAD{}, active: active}
	for i, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		versionText, encoded, found := strings.Cut(entry, ":")
		if !found {
			return nil, fmt.Errorf("SECRETS_ENCRYPTION_KEYS entry %d must look like <version>:<base64 key>", i+1)
		}
		version, err := parseVersion(strings.TrimSpace(versionText))
		if err != nil {
			return nil, fmt.Errorf("SECRETS_ENCRYPTION_KEYS entry %d has an invalid version", i+1)
		}
		if _, exists := k.keys[version]; exists {
			return nil, fmt.Errorf("SECRETS_ENCRYPTION_KEYS lists version %d more than once", version)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil || len(key) != keyBytes {
			clear(key)
			return nil, fmt.Errorf("SECRETS_ENCRYPTION_KEYS version %d must be exactly 32 random bytes, base64-encoded (openssl rand -base64 32)", version)
		}
		block, err := aes.NewCipher(key)
		clear(key)
		if err != nil {
			return nil, fmt.Errorf("SECRETS_ENCRYPTION_KEYS version %d is not a usable AES-256 key", version)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("SECRETS_ENCRYPTION_KEYS version %d is not a usable AES-256 key", version)
		}
		k.keys[version] = aead
	}
	if _, ok := k.keys[active]; !ok {
		return nil, fmt.Errorf("SECRETS_ACTIVE_KEY_VERSION %d is not listed in SECRETS_ENCRYPTION_KEYS", active)
	}
	return k, nil
}

func parseVersion(text string) (int32, error) {
	n, err := strconv.ParseInt(text, 10, 32)
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return 0, errors.New("invalid key version")
	}
	return int32(n), nil
}

// ActiveVersion is the key version used for new values.
func (k *Keyring) ActiveVersion() int32 { return k.active }

// associatedData authenticates where a value belongs. Changing the repository,
// name or version recorded beside the ciphertext makes it fail to open.
func associatedData(repoID int64, name string, version int32) []byte {
	return []byte("previa:env_secret:v1\x00" + strconv.FormatInt(repoID, 10) + "\x00" + name + "\x00" + strconv.FormatInt(int64(version), 10))
}

// Seal encrypts value for one repository secret with the active key. The result
// is nonce || ciphertext || tag.
func (k *Keyring) Seal(repoID int64, name string, value []byte) ([]byte, int32, error) {
	aead := k.keys[k.active]
	nonce := make([]byte, nonceBytes, nonceBytes+len(value)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, fmt.Errorf("generate secret nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, value, associatedData(repoID, name, k.active)), k.active, nil
}

// Open decrypts a sealed value. The caller owns the returned plaintext and
// should clear it once used.
func (k *Keyring) Open(repoID int64, name string, version int32, sealed []byte) ([]byte, error) {
	aead, ok := k.keys[version]
	if !ok {
		return nil, ErrUnknownKeyVersion
	}
	if len(sealed) < nonceBytes+aead.Overhead() {
		return nil, ErrUndecryptable
	}
	plain, err := aead.Open(nil, sealed[:nonceBytes], sealed[nonceBytes:], associatedData(repoID, name, version))
	if err != nil {
		return nil, ErrUndecryptable
	}
	return plain, nil
}
