// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentauth is the protocol between the Drawbridge manager and its
// agents: Ed25519 keys, signed requests (agent to manager), signed commands
// (manager to agent), and the messages both send.
//
// Agents call the manager; the manager never calls an agent. Every agent
// request is signed with the agent's key, and every command with the
// manager's key, which the agent pins at enrollment, so nothing between them
// (a reverse proxy included) can forge either.
package agentauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Prefixes of encoded keys.
const (
	publicPrefix  = "ed25519:"
	privatePrefix = "ed25519-private:"
)

// MaxSkew is how far a request's timestamp may be from the manager's clock.
const MaxSkew = 5 * time.Minute

// NonceTTL is how long the manager remembers a request's nonce.
const NonceTTL = 2 * MaxSkew

var b64 = base64.RawURLEncoding

// Key is an Ed25519 private key.
type Key struct{ priv ed25519.PrivateKey }

// NewKey makes a new random key.
func NewKey() (Key, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return Key{priv}, err
}

// ParseKey reads a key written by Key.String.
func ParseKey(s string) (Key, error) {
	seed, err := b64.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), privatePrefix))
	if err != nil || !strings.HasPrefix(strings.TrimSpace(s), privatePrefix) || len(seed) != ed25519.SeedSize {
		return Key{}, errors.New("not an ed25519-private: key")
	}
	return Key{ed25519.NewKeyFromSeed(seed)}, nil
}

// String encodes the key as "ed25519-private:<base64url seed>". It is secret.
func (k Key) String() string { return privatePrefix + b64.EncodeToString(k.priv.Seed()) }

// Public returns the key's public half.
func (k Key) Public() PublicKey { return PublicKey(k.priv.Public().(ed25519.PublicKey)) }

// Sign signs msg.
func (k Key) Sign(msg []byte) string { return b64.EncodeToString(ed25519.Sign(k.priv, msg)) }

// PublicKey is an Ed25519 public key.
type PublicKey ed25519.PublicKey

// ParsePublicKey reads a key written by PublicKey.String.
func ParsePublicKey(s string) (PublicKey, error) {
	s = strings.TrimSpace(s)
	raw, err := b64.DecodeString(strings.TrimPrefix(s, publicPrefix))
	if err != nil || !strings.HasPrefix(s, publicPrefix) || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("not an ed25519: public key")
	}
	return PublicKey(raw), nil
}

// String encodes the key as "ed25519:<base64url>".
func (p PublicKey) String() string { return publicPrefix + b64.EncodeToString(p) }

// Fingerprint is a short form of the key for people to compare: the first
// 16 hex digits of its SHA-256.
func (p PublicKey) Fingerprint() string {
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:8])
}

// Verify checks sig over msg.
func (p PublicKey) Verify(msg []byte, sig string) bool {
	raw, err := b64.DecodeString(sig)
	return err == nil && len(p) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(p), msg, raw)
}

// Equal reports whether two keys are the same.
func (p PublicKey) Equal(q PublicKey) bool { return string(p) == string(q) }

// ---- requests, agent to manager ----

// Scheme is the Authorization scheme of agent requests.
const Scheme = "Drawbridge-Agent"

// Auth is the parsed Authorization header of an agent request.
type Auth struct {
	Host  int64
	TS    int64
	Nonce string
	Sig   string
}

// requestText is what an agent signs for a request.
func requestText(method, pathQuery string, host, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return fmt.Appendf(nil, "%s\n%s\n%d\n%d\n%s\n%s", method, pathQuery, host, ts, nonce, hex.EncodeToString(sum[:]))
}

// SignRequest returns the Authorization header for a request.
func SignRequest(k Key, host int64, method, pathQuery string, body []byte, now time.Time) (string, error) {
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		return "", err
	}
	nonce := b64.EncodeToString(n)
	ts := now.Unix()
	sig := k.Sign(requestText(method, pathQuery, host, ts, nonce, body))
	return fmt.Sprintf("%s host=%d, ts=%d, nonce=%s, sig=%s", Scheme, host, ts, nonce, sig), nil
}

// ParseAuth reads an Authorization header.
func ParseAuth(header string) (Auth, error) {
	rest, ok := strings.CutPrefix(header, Scheme+" ")
	if !ok {
		return Auth{}, errors.New("not a " + Scheme + " header")
	}
	var a Auth
	var err error
	seen := map[string]bool{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || seen[k] {
			return Auth{}, errors.New("malformed " + Scheme + " header")
		}
		seen[k] = true
		switch k {
		case "host":
			a.Host, err = strconv.ParseInt(v, 10, 64)
		case "ts":
			a.TS, err = strconv.ParseInt(v, 10, 64)
		case "nonce":
			a.Nonce = v
		case "sig":
			a.Sig = v
		}
		if err != nil {
			return Auth{}, errors.New("malformed " + Scheme + " header")
		}
	}
	if len(seen) != 4 || a.Nonce == "" || a.Sig == "" {
		return Auth{}, errors.New("incomplete " + Scheme + " header")
	}
	return a, nil
}

// Verify checks a request's signature and timestamp against one of keys. It
// returns the key that signed it. Nonces are checked separately (Nonces).
func (a Auth) Verify(keys []PublicKey, method, pathQuery string, body []byte, now time.Time) (PublicKey, error) {
	if d := now.Sub(time.Unix(a.TS, 0)); d > MaxSkew || d < -MaxSkew {
		return nil, fmt.Errorf("the request's time is %s from the manager's; check both clocks", d.Round(time.Second))
	}
	msg := requestText(method, pathQuery, a.Host, a.TS, a.Nonce, body)
	for _, k := range keys {
		if k != nil && k.Verify(msg, a.Sig) {
			return k, nil
		}
	}
	return nil, errors.New("bad signature")
}

// Nonces remembers recent request nonces so a request cannot be replayed.
type Nonces struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// Use records a nonce, and reports false if it was seen within NonceTTL.
func (n *Nonces) Use(nonce string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[string]time.Time{}
	}
	for k, t := range n.seen {
		if now.Sub(t) > NonceTTL {
			delete(n.seen, k)
		}
	}
	if _, ok := n.seen[nonce]; ok {
		return false
	}
	n.seen[nonce] = now
	return true
}

// RotationProof is what a new key signs to show the agent holds it.
func RotationProof(host int64, newKey PublicKey, ts int64) []byte {
	return fmt.Appendf(nil, "rotate\n%d\n%s\n%d", host, newKey, ts)
}
