// Package vault seals small secrets with AES-256-GCM under versioned master
// keys. The version is stored with each ciphertext, so a new key can be
// introduced while old ciphertexts stay readable, and rows are re-sealed in the
// background (rotation) without downtime. Every sealing binds additional
// authenticated data, so a ciphertext copied to another owner or name fails to
// open.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// KeySize is the master key length: AES-256.
const KeySize = 32

// ErrUnknownKeyVersion means a ciphertext was sealed under a key this keyring
// no longer (or never) held.
var ErrUnknownKeyVersion = errors.New("vault: unknown key version")

// ErrOpen means a ciphertext did not authenticate: wrong key, wrong additional
// data, or tampering. The cause is deliberately not distinguished.
var ErrOpen = errors.New("vault: cannot open ciphertext")

// Keyring holds the current master key and any older ones still needed to open
// existing ciphertexts. It is immutable and safe for concurrent use.
type Keyring struct {
	current int
	aeads   map[int]cipher.AEAD
}

// NewKeyring builds a keyring. keys maps version to a 32-byte key and must
// contain current.
func NewKeyring(current int, keys map[int][]byte) (*Keyring, error) {
	if current < 1 {
		return nil, errors.New("vault: key version must be at least 1")
	}
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("vault: the current key (version %d) is missing", current)
	}
	k := &Keyring{current: current, aeads: make(map[int]cipher.AEAD, len(keys))}
	for version, key := range keys {
		if len(key) != KeySize {
			return nil, fmt.Errorf("vault: key version %d must be %d bytes, got %d", version, KeySize, len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("vault: key version %d: %w", version, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("vault: key version %d: %w", version, err)
		}
		k.aeads[version] = aead
	}
	return k, nil
}

// DecodeKey parses a base64 (standard or URL-safe) master key.
func DecodeKey(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := encoding.DecodeString(text); err == nil {
			if len(key) != KeySize {
				return nil, fmt.Errorf("vault: master key must decode to %d bytes, got %d", KeySize, len(key))
			}
			return key, nil
		}
	}
	return nil, errors.New("vault: master key is not valid base64")
}

// ParseKeyring builds a keyring from configuration: the current key and its
// version, plus older keys as "version:base64,version:base64".
func ParseKeyring(currentKey string, currentVersion int, oldKeys string) (*Keyring, error) {
	current, err := DecodeKey(currentKey)
	if err != nil {
		return nil, err
	}
	keys := map[int][]byte{currentVersion: current}
	for _, entry := range strings.Split(oldKeys, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		versionText, keyText, ok := strings.Cut(entry, ":")
		version, err := strconv.Atoi(strings.TrimSpace(versionText))
		if !ok || err != nil || version < 1 {
			return nil, fmt.Errorf("vault: old key %q must look like <version>:<base64 key>", versionText)
		}
		if version == currentVersion {
			return nil, fmt.Errorf("vault: old key version %d clashes with the current key", version)
		}
		key, err := DecodeKey(keyText)
		if err != nil {
			return nil, fmt.Errorf("old key version %d: %w", version, err)
		}
		keys[version] = key
	}
	return NewKeyring(currentVersion, keys)
}

// Current is the version new ciphertexts are sealed under.
func (k *Keyring) Current() int { return k.current }

// Versions lists every version this keyring can open, ascending.
func (k *Keyring) Versions() []int {
	out := make([]int, 0, len(k.aeads))
	for version := range k.aeads {
		out = append(out, version)
	}
	sort.Ints(out)
	return out
}

// Seal encrypts plaintext under the current key, binding aad. The nonce is
// random per call and returned separately.
func (k *Keyring) Seal(plaintext, aad []byte) (ciphertext, nonce []byte, version int, err error) {
	aead := k.aeads[k.current]
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, fmt.Errorf("vault: generate nonce: %w", err)
	}
	return aead.Seal(nil, nonce, plaintext, aad), nonce, k.current, nil
}

// Open decrypts a ciphertext sealed under version with the same aad.
func (k *Keyring) Open(version int, nonce, ciphertext, aad []byte) ([]byte, error) {
	aead, ok := k.aeads[version]
	if !ok {
		return nil, fmt.Errorf("%w %d", ErrUnknownKeyVersion, version)
	}
	if len(nonce) != aead.NonceSize() {
		return nil, ErrOpen
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}
