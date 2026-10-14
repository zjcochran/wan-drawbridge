// SPDX-License-Identifier: AGPL-3.0-or-later

package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// The SHA-1 test vectors of RFC 6238, appendix B, cut to 6 digits.
func TestRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471",
		1234567890: "005924", 2000000000: "279037", 20000000000: "353130",
	} {
		got, err := Code(secret, Step(time.Unix(unix, 0)))
		if err != nil || got != want {
			t.Errorf("T=%d: %q %v, want %q", unix, got, err, want)
		}
	}
}

func TestVerify(t *testing.T) {
	secret := NewSecret()
	if len(secret) != 32 {
		t.Fatalf("secret %q", secret)
	}
	now := time.Unix(1_800_000_000, 0)
	code, _ := Code(secret, Step(now))
	if step, ok := Verify(secret, code[:3]+" "+code[3:], now); !ok || step != Step(now) {
		t.Errorf("current code: %d %v", step, ok)
	}
	prev, _ := Code(secret, Step(now)-1)
	if step, ok := Verify(secret, prev, now); !ok || step != Step(now)-1 {
		t.Errorf("previous step: %d %v", step, ok)
	}
	old, _ := Code(secret, Step(now)-2)
	for _, bad := range []string{old, "12345", "1234567", "abcdef"} {
		if _, ok := Verify(secret, bad, now); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := Verify("not base32!", code, now); ok {
		t.Error("a broken secret verified")
	}
	if _, err := Code("not base32!", 1); err == nil {
		t.Error("Code accepted a broken secret")
	}
}

func TestURI(t *testing.T) {
	u := URI("WAN Drawbridge", "alice", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{"otpauth://totp/WAN%20Drawbridge:alice?", "secret=JBSWY3DPEHPK3PXP", "issuer=WAN+Drawbridge", "digits=6", "period=30"} {
		if !strings.Contains(u, want) {
			t.Errorf("%s lacks %s", u, want)
		}
	}
}
