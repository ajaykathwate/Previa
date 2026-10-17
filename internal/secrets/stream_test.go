package secrets

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func encryptAll(t *testing.T, k *Keyring, repoID int64, purpose string, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, version, err := k.EncryptStream(&out, repoID, purpose)
	if err != nil || version != k.ActiveVersion() {
		t.Fatalf("encrypt: %v (version %d)", err, version)
	}
	// Uneven writes cross chunk boundaries.
	for rest := plain; len(rest) > 0; {
		n := min(len(rest), 1000+len(rest)%7919)
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func decryptAll(k *Keyring, repoID int64, purpose string, sealed []byte) ([]byte, error) {
	r, _, err := k.DecryptStream(bytes.NewReader(sealed), repoID, purpose)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestEncryptedStreamsRoundTripAnySize(t *testing.T) {
	k, err := ParseKeyring("1:"+testKey(t), "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, streamChunk - 1, streamChunk, streamChunk + 1, 3*streamChunk + 17} {
		plain := make([]byte, size)
		_, _ = rand.Read(plain)
		sealed := encryptAll(t, k, 42, "previa:baseline:a", plain)
		got, err := decryptAll(k, 42, "previa:baseline:a", sealed)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: %v", size, err)
		}
		if size > 0 && bytes.Contains(sealed, plain[:min(size, 32)]) {
			t.Fatalf("size %d: plaintext visible", size)
		}
	}
}

func TestEncryptedStreamsRefuseTamperingAndMisuse(t *testing.T) {
	k, err := ParseKeyring("1:"+testKey(t), "1")
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, 2*streamChunk+100)
	_, _ = rand.Read(plain)
	sealed := encryptAll(t, k, 42, "previa:baseline:a", plain)
	chunk := streamChunk + 16
	first := streamHeader
	cases := map[string][]byte{
		"another repository": nil,
		"another purpose":    nil,
		"dropped last chunk": sealed[:first+2*chunk],
		"cut inside a chunk": sealed[:first+chunk+100],
		"header only":        sealed[:first],
		"flipped bit":        append(append([]byte{}, sealed[:first+10]...), append([]byte{sealed[first+10] ^ 1}, sealed[first+11:]...)...),
		"swapped chunks":     append(append(append([]byte{}, sealed[:first]...), sealed[first+chunk:first+2*chunk]...), append(append([]byte{}, sealed[first:first+chunk]...), sealed[first+2*chunk:]...)...),
		"not a stream":       []byte("plain text, not encrypted at all"),
	}
	for name, data := range cases {
		repo, purpose := int64(42), "previa:baseline:a"
		switch name {
		case "another repository":
			data, repo = sealed, 43
		case "another purpose":
			data, purpose = sealed, "previa:baseline:b"
		}
		if got, err := decryptAll(k, repo, purpose, data); !errors.Is(err, ErrStreamCorrupt) {
			t.Errorf("%s: %v (read %d bytes)", name, err, len(got))
		}
	}
}

func TestEncryptedStreamsFollowKeyRotation(t *testing.T) {
	v1 := "1:" + testKey(t)
	old, err := ParseKeyring(v1, "1")
	if err != nil {
		t.Fatal(err)
	}
	sealed := encryptAll(t, old, 7, "p", []byte("starting data"))
	rotated, err := ParseKeyring(v1+",2:"+testKey(t), "2")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decryptAll(rotated, 7, "p", sealed); err != nil || string(got) != "starting data" {
		t.Fatalf("an older key version opens: %v", err)
	}
	fresh := encryptAll(t, rotated, 7, "p", []byte("new"))
	if _, err := decryptAll(old, 7, "p", fresh); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("a keyring without the stream's key: %v", err)
	}
}
