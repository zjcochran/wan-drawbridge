// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"testing"
	"time"
)

func TestGroupKey(t *testing.T) {
	cases := []struct {
		name string
		asn  int
		want string
	}{
		{"EXAMPLENET-64500", 64500, "EXAMPLENET"},
		{"EXAMPLENET-64501", 64501, "EXAMPLENET"},
		{"METROCABLE-64503-WA-OR-C3", 64503, "METROCABLE"},
		{"SKYLINK-AS64510", 64510, "SKYLINK"},
		{"Skylink Wireless, Inc.", 64510, "SKYLINK"}, // ipinfo fallback spelling
		{"Examplenet Communications, LLC", 64500, "EXAMPLENET"},
		{"SPECTRUM-AS Spectrum Internet Services", 139570, "SPECTRUM"},
		{"BIGTEL-INTERNET4", 64506, "BIGTEL-INTERNET4"},
		{"BIGTEL-MOBILITY-LLC-AS64511", 64511, "BIGTEL-MOBILITY-LLC"},
		{"SPACEX-STARLINK", 14593, "SPACEX-STARLINK"},
		{"", 64512, "AS64512"},
		{"AS64512", 64512, "AS64512"},
	}
	for _, c := range cases {
		if got := GroupKey(c.name, c.asn); got != c.want {
			t.Errorf("GroupKey(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestASNsGroupIntoOneISP(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	a, _, err := s.SeeASN(ctx, 64500, "EXAMPLENET-64500", now)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := s.SeeASN(ctx, 64501, "EXAMPLENET-64501", now)
	c, _, _ := s.SeeASN(ctx, 64510, "SKYLINK-AS64510", now)
	if a != b || a == c {
		t.Fatalf("examplenet=%d,%d tmobile=%d", a, b, c)
	}

	s.Observe(ctx, 64500, 100, now)
	s.Observe(ctx, 64501, 50, now)
	s.Observe(ctx, 64510, 30, now)
	shares, _ := s.Shares(ctx, now)
	got := map[int]int64{}
	for _, sh := range shares {
		got[sh.ID] = sh.Seconds
	}
	if got[a] != 150 || got[c] != 30 {
		t.Fatalf("shares = %v", got)
	}
}

func TestMetered(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	id, _, _ := s.SeeASN(ctx, 64510, "SKYLINK-AS64510", now)
	if m, err := s.Metered(ctx, id); err != nil || !m {
		t.Fatalf("a new ISP: metered = %v, %v", m, err)
	}
	if err := s.SetMetered(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if isps, _ := s.ISPs(ctx, now); len(isps) != 1 || isps[0].Metered {
		t.Errorf("ISPs = %+v", isps)
	}
	if m, _ := s.Metered(ctx, 99); !m {
		t.Error("an unknown ISP should count as metered")
	}
	if err := s.SetMetered(ctx, 99, true); err == nil {
		t.Error("SetMetered on an unknown ISP succeeded")
	}
}

func TestMoveASNAndRoles(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	bigtel, _, _ := s.SeeASN(ctx, 64506, "BIGTEL-INTERNET4", now)
	odd, _, _ := s.SeeASN(ctx, 64600, "BIGTEL-SOMETHING-ELSE", now)

	// Merge the odd ASN into the BigTel group; its now-empty group is removed.
	if got, err := s.MoveASN(ctx, 64600, bigtel, now); err != nil || got != bigtel {
		t.Fatalf("merge: %d %v", got, err)
	}
	if id, _ := s.ISPOfASN(ctx, 64600); id != bigtel {
		t.Fatalf("64600 in %d, want %d", id, bigtel)
	}
	isps, _ := s.ISPs(ctx, now)
	for _, i := range isps {
		if i.ID == odd {
			t.Fatal("empty group was not removed")
		}
	}

	// Split it back out into its own group.
	alone, err := s.MoveASN(ctx, 64600, 0, now)
	if err != nil || alone == bigtel {
		t.Fatalf("split: %d %v", alone, err)
	}

	if err := s.SetRole(ctx, bigtel, RolePrimary, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, alone, RolePrimary, false); err != nil {
		t.Fatal(err)
	}
	id, pinned, _ := s.Primary(ctx)
	if id != alone || pinned {
		t.Fatalf("primary = %d pinned=%v", id, pinned)
	}
	if err := s.SetRole(ctx, bigtel, RoleIgnored, false); err != nil {
		t.Fatal(err)
	}
	if _, role, _ := s.SeeASN(ctx, 64506, "", now); role != RoleIgnored {
		t.Fatalf("role = %s", role)
	}
}
