// SPDX-License-Identifier: AGPL-3.0-or-later

// Package totp implements time-based one-time passwords (RFC 6238) as
// authenticator apps use them: HMAC-SHA1, 6 digits, 30 second steps.
package totp

// HMAC-SHA1 is what RFC 6238 and authenticator apps use; HMAC does not rely
// on SHA-1's collision resistance, which is what is broken.
// nosemgrep
import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 and every authenticator app use HMAC-SHA1
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Period is the length of a time step.
	Period = 30 * time.Second
	// Digits is the length of a code.
	Digits = 6
	// Skew is how many steps before or after now a code is accepted, for
	// clocks that drift.
	Skew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random 160-bit secret, base32-encoded as apps expect it.
func NewSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// Step is the time step t falls in.
func Step(t time.Time) int64 { return t.Unix() / int64(Period/time.Second) }

// Code is the code for a secret at a time step.
func Code(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("totp secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) //nolint:gosec // G115: steps are positive
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1_000_000), nil
}

// Verify checks a code at now, within Skew steps. It returns the step that
// matched, so the caller can refuse a code used before (any step not after
// the last one used).
func Verify(secret, code string, now time.Time) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return 0, false
	}
	cur := Step(now)
	for s := cur - Skew; s <= cur+Skew; s++ {
		want, err := Code(secret, s)
		if err != nil {
			return 0, false
		}
		if hmac.Equal([]byte(want), []byte(code)) {
			return s, true
		}
	}
	return 0, false
}

// URI is the otpauth:// link an authenticator app reads from a QR code.
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"},
		"digits": {fmt.Sprint(Digits)}, "period": {fmt.Sprint(int(Period / time.Second))}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}
