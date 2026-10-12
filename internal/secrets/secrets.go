// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secrets encrypts values stored in the database (the OIDC client
// secret) with AES-256-GCM.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Box struct{ aead cipher.AEAD }

// Load returns a Box keyed by envKey when set, otherwise by the key file at
// path, which is created with a random key on first use. Any string works as
// envKey; it is hashed to 32 bytes.
func Load(envKey, path string) (*Box, error) {
	key := envKey
	if key == "" {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the key file path comes from DRAWBRIDGE_DB_DIR, not from a request
		switch {
		case err == nil:
			key = strings.TrimSpace(string(b))
		case errors.Is(err, os.ErrNotExist):
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return nil, err
			}
			key = base64.StdEncoding.EncodeToString(raw)
			if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
				return nil, fmt.Errorf("write secret key: %w", err)
			}
		default:
			return nil, fmt.Errorf("read secret key: %w", err)
		}
	}
	if key == "" {
		return nil, errors.New("secret key is empty")
	}
	return New(key)
}

func New(key string) (*Box, error) {
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext; "" stays "".
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return "v1:" + base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a value from Seal; "" stays "".
func (b *Box) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	if err != nil || !strings.HasPrefix(sealed, "v1:") || len(raw) < b.aead.NonceSize() {
		return "", errors.New("stored secret is malformed")
	}
	n := b.aead.NonceSize()
	pt, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt stored secret: was DRAWBRIDGE_SECRET_KEY or the key file changed?")
	}
	return string(pt), nil
}
