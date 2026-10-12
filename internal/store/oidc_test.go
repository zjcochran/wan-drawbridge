// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"testing"
	"time"
)

func TestSSOUsersAndLinks(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	local, _ := s.CreateUser(ctx, "alice", "test-password-123", RoleAdmin, now)

	// A same-named SSO account never takes over the local one.
	sso, err := s.CreateSSOUser(ctx, "alice", RoleViewer, "https://id.example", "sub-1", "z@example.com", now)
	if err != nil || sso.ID == local.ID || sso.Username != "alice-2" || sso.HasLocal {
		t.Fatalf("sso user = %+v, %v", sso, err)
	}
	if u, ok, _ := s.UserByIdentity(ctx, "https://id.example", "sub-1"); !ok || u.ID != sso.ID {
		t.Fatalf("lookup = %+v %v", u, ok)
	}
	if err := s.LinkIdentity(ctx, local.ID, "https://id.example", "sub-1", "", now); err == nil {
		t.Fatal("linked an identity owned by someone else")
	}
	if err := s.LinkIdentity(ctx, local.ID, "https://id.example", "sub-2", "", now); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.Identities(ctx, local.ID); len(ids) != 1 {
		t.Fatalf("identities = %v", ids)
	}
	if err := s.UnlinkIdentities(ctx, sso.ID); err == nil {
		t.Fatal("unlinked the only sign-in method of an SSO-only user")
	}
	if err := s.UnlinkIdentities(ctx, local.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, local.ID, RoleViewer); err == nil {
		t.Fatal("demoted the last admin")
	}
}
