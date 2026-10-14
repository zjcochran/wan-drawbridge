// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAPITokenNames(t *testing.T) {
	ctx, s := context.Background(), open(t)
	if _, err := s.CreateAPIToken(ctx, "  ", "alice", t0); err == nil {
		t.Error("an empty name was accepted")
	}
	if _, err := s.CreateAPIToken(ctx, strings.Repeat("x", 65), "alice", t0); err == nil {
		t.Error("a 65-character name was accepted")
	}
}

func TestWrongAPITokens(t *testing.T) {
	ctx, s := context.Background(), open(t)
	token, _ := s.CreateAPIToken(ctx, "test", "alice", t0)
	for _, bad := range []string{"", "nope", APITokenPrefix + "wrong", token + "x"} {
		if _, ok, err := s.UseAPIToken(ctx, bad, t0); ok || err != nil {
			t.Errorf("UseAPIToken(%q) = %v, %v", bad, ok, err)
		}
	}
}

func TestAPITokens(t *testing.T) {
	ctx, s := context.Background(), open(t)
	token, err := s.CreateAPIToken(ctx, " Home Assistant ", "alice", t0)
	if err != nil || !strings.HasPrefix(token, APITokenPrefix) || len(token) < 40 {
		t.Fatalf("token = %q, %v", token, err)
	}
	var stored string
	s.db.QueryRow(`SELECT hash FROM api_tokens`).Scan(&stored)
	if strings.Contains(stored, token[len(APITokenPrefix):]) {
		t.Fatal("the token is stored in the clear")
	}

	name, ok, err := s.UseAPIToken(ctx, token, t0.Add(time.Hour))
	if err != nil || !ok || name != "Home Assistant" {
		t.Fatalf("UseAPIToken = %q %v %v", name, ok, err)
	}
	list, err := s.APITokens(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "Home Assistant" || list[0].CreatedBy != "alice" ||
		!list[0].LastUsed.Equal(t0.Add(time.Hour)) || !list[0].CreatedAt.Equal(t0) {
		t.Fatalf("tokens = %+v, %v", list, err)
	}
	if name, err := s.DeleteAPIToken(ctx, list[0].ID); err != nil || name != "Home Assistant" {
		t.Fatalf("delete = %q, %v", name, err)
	}
	if _, ok, _ := s.UseAPIToken(ctx, token, t0); ok {
		t.Error("a revoked token still works")
	}
	if _, err := s.DeleteAPIToken(ctx, list[0].ID); err == nil {
		t.Error("deleting twice succeeded")
	}
}
