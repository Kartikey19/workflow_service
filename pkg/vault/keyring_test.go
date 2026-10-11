package vault

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpenRoundTripAndBinding(t *testing.T) {
	ring, err := NewKeyring(1, map[int][]byte{1: key(t)})
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("org-1\x00API_KEY")
	ciphertext, nonce, version, err := ring.Seal([]byte("s3cret-value"), aad)
	if err != nil || version != 1 {
		t.Fatalf("seal = %v, version %d", err, version)
	}
	if bytes.Contains(ciphertext, []byte("s3cret-value")) {
		t.Fatal("the ciphertext contains the plaintext")
	}
	got, err := ring.Open(version, nonce, ciphertext, aad)
	if err != nil || string(got) != "s3cret-value" {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Bound to its owner and name: the same bytes do not open elsewhere.
	if _, err := ring.Open(version, nonce, ciphertext, []byte("org-2\x00API_KEY")); !errors.Is(err, ErrOpen) {
		t.Fatalf("open with other aad = %v, want ErrOpen", err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0xff
	if _, err := ring.Open(version, nonce, tampered, aad); !errors.Is(err, ErrOpen) {
		t.Fatalf("open tampered = %v, want ErrOpen", err)
	}
	// A fresh nonce each time: sealing twice never repeats the ciphertext.
	other, otherNonce, _, _ := ring.Seal([]byte("s3cret-value"), aad)
	if bytes.Equal(other, ciphertext) || bytes.Equal(otherNonce, nonce) {
		t.Fatal("two seals produced the same nonce or ciphertext")
	}
}

func TestKeyVersionsAndRotationSupport(t *testing.T) {
	old, current := key(t), key(t)
	v1, _ := NewKeyring(1, map[int][]byte{1: old})
	sealed, nonce, version, _ := v1.Seal([]byte("value-123456"), nil)

	v2, err := NewKeyring(2, map[int][]byte{1: old, 2: current})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v2.Open(version, nonce, sealed, nil); err != nil || string(got) != "value-123456" {
		t.Fatalf("the new keyring cannot open old data: %q, %v", got, err)
	}
	if _, _, v, _ := v2.Seal([]byte("x"), nil); v != 2 || v2.Current() != 2 {
		t.Fatalf("new data is sealed under version %d, want 2", v)
	}
	if got := v2.Versions(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("versions = %v", got)
	}
	// Once the old key is dropped its ciphertexts are unreadable, distinctly.
	only2, _ := NewKeyring(2, map[int][]byte{2: current})
	if _, err := only2.Open(1, nonce, sealed, nil); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("open with a dropped key = %v, want ErrUnknownKeyVersion", err)
	}
}

func TestKeyringValidation(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(key(t))
	for name, fn := range map[string]func() error{
		"short key":          func() error { _, err := DecodeKey(base64.StdEncoding.EncodeToString([]byte("short"))); return err },
		"not base64":         func() error { _, err := DecodeKey("%%%"); return err },
		"missing current":    func() error { _, err := NewKeyring(2, map[int][]byte{1: key(t)}); return err },
		"version zero":       func() error { _, err := NewKeyring(0, map[int][]byte{0: key(t)}); return err },
		"bad old key entry":  func() error { _, err := ParseKeyring(good, 2, "oops"); return err },
		"old clashes":        func() error { _, err := ParseKeyring(good, 2, "2:"+good); return err },
		"old key wrong size": func() error { _, err := ParseKeyring(good, 2, "1:c2hvcnQ="); return err },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	ring, err := ParseKeyring(good, 3, " 1:"+base64.RawURLEncoding.EncodeToString(key(t))+" , 2:"+base64.StdEncoding.EncodeToString(key(t)))
	if err != nil || ring.Current() != 3 || len(ring.Versions()) != 3 {
		t.Fatalf("parse = %v, %v", ring, err)
	}
	if _, err := DecodeKey(strings.Repeat("A", 43) + "="); err != nil {
		t.Fatalf("a valid padded key was rejected: %v", err)
	}
}
