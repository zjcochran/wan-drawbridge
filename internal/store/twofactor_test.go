// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRecoveryCodeFormat(t *testing.T) {
	codes, hashes := NewRecoveryCodes()
	if len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatalf("%d codes, %d hashes", len(codes), len(hashes))
	}
	re := regexp.MustCompile(`^[a-hj-km-np-z2-9]{4}-[a-hj-km-np-z2-9]{4}-[a-hj-km-np-z2-9]{4}$`)
	seen := map[string]bool{}
	for i, c := range codes {
		if !re.MatchString(c) || seen[c] || hashes[i] != hashToken(c) {
			t.Errorf("code %q", c)
		}
		seen[c] = true
	}
	for in, want := range map[string]string{"ABCD EFGH JKMN": "abcd-efgh-jkmn", "abcdefghjkmn": "abcd-efgh-jkmn", "short": "short"} {
		if got := normalizeRecoveryCode(in); got != want {
			t.Errorf("normalize(%q) = %q", in, got)
		}
	}
}

func TestTwoFactor(t *testing.T) {
	ctx, s := context.Background(), open(t)
	u, _ := s.CreateUser(ctx, "alice", "test-password-123", RoleAdmin, t0)
	codes, hashes := NewRecoveryCodes()
	if err := s.EnableTwoFactor(ctx, u.ID, "v1:sealed", 100, hashes); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTwoFactor(ctx, 99, "v1:x", 0, nil); err == nil {
		t.Error("enabled for an unknown user")
	}
	if got, _ := s.UserByID(ctx, u.ID); !got.TwoFactor {
		t.Fatal("User.TwoFactor is false")
	}
	if secret, _ := s.TwoFactorSecret(ctx, u.ID); secret != "v1:sealed" {
		t.Errorf("secret = %q", secret)
	}
	// Steps only move forward: a code works once.
	for step, want := range map[int64]bool{100: false, 99: false, 101: true} {
		if ok, err := s.UseTOTPStep(ctx, u.ID, step); err != nil || ok != want {
			t.Errorf("step %d: %v %v", step, ok, err)
		}
	}
	if ok, _ := s.UseTOTPStep(ctx, u.ID, 101); ok {
		t.Error("step 101 twice")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, strings.ToUpper(codes[3])); !ok {
		t.Error("recovery code refused")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, codes[3]); ok {
		t.Error("recovery code used twice")
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != RecoveryCodeCount-1 {
		t.Errorf("left = %d", n)
	}
	_, fresh := NewRecoveryCodes()
	if err := s.ReplaceRecoveryCodes(ctx, u.ID, fresh[:2]); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != 2 {
		t.Errorf("left after replacing = %d", n)
	}
	if on, err := s.DisableTwoFactor(ctx, u.ID); !on || err != nil {
		t.Fatalf("disable: %v %v", on, err)
	}
	if on, _ := s.DisableTwoFactor(ctx, u.ID); on {
		t.Error("disabled twice")
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != 0 {
		t.Errorf("codes left after disabling: %d", n)
	}
}

func TestPasswordStepWithTwoFactor(t *testing.T) {
	ctx, s := context.Background(), open(t)
	u, _ := s.CreateUser(ctx, "alice", "test-password-123", RoleAdmin, t0)
	s.EnableTwoFactor(ctx, u.ID, "v1:sealed", 0, nil)
	got, err := s.Authenticate(ctx, "alice", "test-password-123", t0.Add(time.Hour))
	if err != nil || !got.TwoFactor || !got.LastLogin.IsZero() {
		t.Fatalf("the password step counted as a sign-in: %+v %v", got, err)
	}
	if ok, _ := s.VerifyPassword(ctx, u.ID, "test-password-123"); !ok {
		t.Error("right password refused")
	}
	if ok, _ := s.VerifyPassword(ctx, u.ID, "wrong"); ok {
		t.Error("wrong password accepted")
	}
	if _, err := s.VerifyPassword(ctx, 99, "x"); err == nil {
		t.Error("unknown user")
	}
}

func TestRecoveryCodesInBackups(t *testing.T) {
	ctx := context.Background()
	src, dst := open(t), open(t)
	fill(t, src)
	_, hashes := NewRecoveryCodes()
	src.EnableTwoFactor(ctx, 1, "v1:sealed", 0, hashes)
	snap, err := src.Export(ctx, false, time.Time{})
	if err != nil || len(snap.Config["recovery_codes"]) != RecoveryCodeCount {
		t.Fatalf("exported codes: %d, %v", len(snap.Config["recovery_codes"]), err)
	}
	if err := dst.Import(ctx, viaJSON(t, snap)); err != nil {
		t.Fatal(err)
	}
	if n, _ := dst.RecoveryCodesLeft(ctx, 1); n != RecoveryCodeCount {
		t.Errorf("imported codes: %d", n)
	}
}
