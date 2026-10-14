// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "test-password-123") || CheckPassword(h, "wrong horse battery") {
		t.Fatal("password check is wrong")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestLoginAndSessions(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	admin, err := s.CreateUser(ctx, "Alice", "test-password-123", RoleAdmin, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "alice", "test-password-123", RoleViewer, now); err == nil {
		t.Fatal("usernames must be unique ignoring case")
	}
	if _, err := s.Authenticate(ctx, "alice", "nope nope nope", now); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := s.Authenticate(ctx, "nobody", "test-password-123", now); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("unknown user: %v", err)
	}
	u, err := s.Authenticate(ctx, "ALICE", "test-password-123", now)
	if err != nil || u.ID != admin.ID || !u.IsAdmin() {
		t.Fatalf("login: %+v %v", u, err)
	}

	token, err := s.CreateSession(ctx, u.ID, "192.0.2.1", "test", now)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok, err := s.SessionByToken(ctx, token, now.Add(time.Hour))
	if err != nil || !ok || sess.User.ID != u.ID || sess.CSRF == "" {
		t.Fatalf("session: %+v %v %v", sess, ok, err)
	}
	if _, ok, _ := s.SessionByToken(ctx, token, now.Add(SessionTTL+time.Hour)); ok {
		t.Fatal("expired session accepted")
	}

	// Changing the password ends existing sessions.
	if err := s.SetPassword(ctx, u.ID, "another long password"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionByToken(ctx, token, now); ok {
		t.Fatal("session survived a password change")
	}
}

func TestLastAdminCannotBeDisabled(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	a, _ := s.CreateUser(ctx, "admin", "test-password-123", RoleAdmin, now)
	if err := s.SetUserDisabled(ctx, a.ID, true); err == nil {
		t.Fatal("disabled the last admin")
	}
	b, _ := s.CreateUser(ctx, "admin2", "test-password-123", RoleAdmin, now)
	if err := s.SetUserDisabled(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "admin", "test-password-123", now); !errors.Is(err, ErrBadLogin) {
		t.Fatal("disabled user logged in")
	}
	_ = b
}

func TestDeleteUser(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	a, _ := s.CreateUser(ctx, "admin", "test-password-123", RoleAdmin, now)
	if err := s.DeleteUser(ctx, a.ID); err == nil {
		t.Fatal("deleted the last admin")
	}
	v, _ := s.CreateSSOUser(ctx, "alice", RoleViewer, "https://id.example.com", "alice-sub", "", now)
	token, _ := s.CreateSession(ctx, v.ID, "", "", now)
	if err := s.DeleteUser(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionByToken(ctx, token, now); ok {
		t.Error("session survived the delete")
	}
	if _, ok, _ := s.UserByIdentity(ctx, "https://id.example.com", "alice-sub"); ok {
		t.Error("identity survived the delete")
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
	if err := s.DeleteUser(ctx, v.ID); err == nil {
		t.Error("deleted a user twice")
	}
	// A disabled admin is not the last admin.
	b, _ := s.CreateUser(ctx, "admin2", "test-password-123", RoleAdmin, now)
	if err := s.SetUserDisabled(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, b.ID); err != nil {
		t.Errorf("delete a disabled admin: %v", err)
	}
}

func TestAppearance(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u, err := s.CreateUser(ctx, "alice", "test-password-123", RoleAdmin, t0)
	if err != nil {
		t.Fatal(err)
	}
	if u.Theme != "default" || u.Mode != "auto" {
		t.Fatalf("new user: %q %q", u.Theme, u.Mode)
	}
	if err := s.SetAppearance(ctx, u.ID, "nord", "dark"); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.UserByID(ctx, u.ID); u.Theme != "nord" || u.Mode != "dark" {
		t.Fatalf("saved: %q %q", u.Theme, u.Mode)
	}
	if err := s.SetAppearance(ctx, 999, "nord", "dark"); err == nil {
		t.Fatal("unknown user accepted")
	}
}
