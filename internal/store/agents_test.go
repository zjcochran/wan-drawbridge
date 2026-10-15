// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHostNames(t *testing.T) {
	for _, ok := range []string{"nas", "a", "NAS-2.lan_x", strings.Repeat("a", 32)} {
		if ValidHostName(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-nas", "nas:media", "a b", strings.Repeat("a", 33), "ünïcode"} {
		if ValidHostName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAddAgentHost(t *testing.T) {
	ctx, s := context.Background(), open(t)
	id, token, err := s.AddAgentHost(ctx, " nas ", "alice", t0)
	if err != nil || !strings.HasPrefix(token, EnrollPrefix) {
		t.Fatalf("add = %d %q %v", id, token, err)
	}
	if _, _, err := s.AddAgentHost(ctx, "nas", "alice", t0); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate name: %v", err)
	}
	if _, _, err := s.AddAgentHost(ctx, "bad name", "alice", t0); err == nil {
		t.Error("a bad name was accepted")
	}
	a, ok, err := s.Agent(ctx, id)
	if err != nil || !ok || a.Name != "nas" || a.Status != HostPending || !a.EnrollPending || !a.EnrollExpires.Equal(t0.Add(EnrollTokenTTL)) {
		t.Fatalf("pending agent = %+v, %v", a, err)
	}
	if _, ok, _ := s.Agent(ctx, LocalHost); ok {
		t.Error("the local host is listed as an agent")
	}
}

func TestEnroll(t *testing.T) {
	ctx, s := context.Background(), open(t)
	id, token, _ := s.AddAgentHost(ctx, "nas", "alice", t0)
	e := Enrollment{Token: token, PublicKey: "ed25519:k1", Platform: "docker", Version: "v1", Address: "192.0.2.5", ManagerKeyID: "k1"}
	if _, err := s.Enroll(ctx, Enrollment{Token: "dbe_nope"}, t0); !errors.Is(err, ErrEnrollToken) {
		t.Errorf("unknown token: %v", err)
	}
	if _, err := s.Enroll(ctx, e, t0.Add(EnrollTokenTTL+time.Second)); !errors.Is(err, ErrEnrollExpired) {
		t.Errorf("expired token: %v", err)
	}
	a, err := s.Enroll(ctx, e, t0.Add(time.Minute))
	if err != nil || a.ID != id || a.PublicKey != "ed25519:k1" || a.Status != HostOnline || a.EnrollPending || a.Version != "v1" ||
		a.LastAddress != "192.0.2.5" || a.ManagerKeyID != "k1" || !a.KeyCreated.Equal(t0.Add(time.Minute)) {
		t.Fatalf("enrolled = %+v, %v", a, err)
	}
	if _, err := s.Enroll(ctx, e, t0.Add(time.Minute)); !errors.Is(err, ErrEnrollToken) {
		t.Errorf("a token used twice: %v", err)
	}

	// A new token cuts the old key off.
	token2, err := s.NewEnrollToken(ctx, id, t0)
	if err != nil || token2 == token {
		t.Fatal(err)
	}
	if a, _, _ := s.Agent(ctx, id); a.PublicKey != "" || a.Status != HostPending || !a.EnrollPending {
		t.Errorf("after a new token = %+v", a)
	}
	if _, err := s.NewEnrollToken(ctx, LocalHost, t0); err == nil {
		t.Error("a token for the local host")
	}
}

func TestAgentKeysAndState(t *testing.T) {
	ctx, s := context.Background(), open(t)
	id, token, _ := s.AddAgentHost(ctx, "nas", "alice", t0)
	if _, err := s.Enroll(ctx, Enrollment{Token: token, PublicKey: "ed25519:k1", ManagerKeyID: "k1"}, t0); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.RotateAgentKey(ctx, id, "ed25519:k2", t0.Add(time.Hour)))
	if a, _, _ := s.Agent(ctx, id); a.PublicKey != "ed25519:k2" || a.PreviousKey != "ed25519:k1" || !a.KeyCreated.Equal(t0.Add(time.Hour)) {
		t.Errorf("rotated = %+v", a)
	}
	must(s.RetirePreviousKey(ctx, id))
	must(s.AgentSeen(ctx, id, "198.51.100.9", t0.Add(2*time.Hour)))
	must(s.SaveInventory(ctx, id, `[{"id":"x"}]`, "docker", "v2"))
	must(s.SetManagerKeyID(ctx, id, "k2"))
	as, err := s.AgentHosts(ctx)
	if err != nil || len(as) != 1 {
		t.Fatalf("agents = %+v, %v", as, err)
	}
	if a := as[0]; a.PreviousKey != "" || a.LastAddress != "198.51.100.9" || a.Inventory != `[{"id":"x"}]` || a.Version != "v2" ||
		a.ManagerKeyID != "k2" || !a.LastSeen.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("agent = %+v", a)
	}
}

func TestAgentStatus(t *testing.T) {
	ctx, s := context.Background(), open(t)
	id, token, _ := s.AddAgentHost(ctx, "nas", "alice", t0)
	if _, err := s.Enroll(ctx, Enrollment{Token: token, PublicKey: "ed25519:k1", ManagerKeyID: "k1"}, t0); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(s.SetHostEnabled(ctx, id, false))
	must(s.AgentSeen(ctx, id, "198.51.100.9", t0))
	if a, _, _ := s.Agent(ctx, id); a.Status != HostDisabled {
		t.Errorf("a disabled host came online: %s", a.Status)
	}
	must(s.SetHostEnabled(ctx, id, true))
	if a, _, _ := s.Agent(ctx, id); a.Status != HostOffline {
		t.Errorf("enabled = %s", a.Status)
	}
	must(s.RenameHost(ctx, id, "attic"))
	if err := s.RenameHost(ctx, id, "local"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("rename to a taken name: %v", err)
	}
	if err := s.RenameHost(ctx, id, "a b"); err == nil {
		t.Error("renamed to a bad name")
	}

	must(s.RevokeHost(ctx, id))
	if a, _, _ := s.Agent(ctx, id); a.Status != HostRevoked || a.PublicKey != "" {
		t.Errorf("revoked = %+v", a)
	}
	if err := s.RotateAgentKey(ctx, id, "ed25519:k3", t0); err == nil {
		t.Error("rotated a revoked key")
	}
	if err := s.RevokeHost(ctx, LocalHost); err == nil {
		t.Error("revoked the local host")
	}
	must(s.SetHostEnabled(ctx, id, false))
	if a, _, _ := s.Agent(ctx, id); a.Status != HostRevoked {
		t.Errorf("disabling a revoked host changed it to %s", a.Status)
	}
}

func TestRemoveHost(t *testing.T) {
	ctx, s := context.Background(), open(t)
	id, _, _ := s.AddAgentHost(ctx, "nas", "alice", t0)
	if _, err := s.SyncInventory(ctx, id, []InventoryItem{{Key: "media/db", Project: "media", Service: "db", Name: "media-db-1"}}, t0); err != nil {
		t.Fatal(err)
	}
	run, _ := s.CreateRun(ctx, "auto", 0, t0)
	s.AddRunAction(ctx, run.ID, RunAction{HostID: id, ContainerID: "c", Key: "media/db", Name: "media-db-1", WasRunning: true})
	s.MarkPending(ctx, run.ID, id, "c", "waiting")
	if n, err := s.ForgetPending(ctx, 99); err != nil || n != 0 {
		t.Errorf("forget on another host = %d, %v", n, err)
	}
	if err := s.RemoveHost(ctx, LocalHost); err == nil {
		t.Error("removed the local host")
	}
	if err := s.RemoveHost(ctx, id); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Containers(ctx)
	stacks, _ := s.Stacks(ctx)
	pend, _ := s.PendingStarts(ctx)
	if len(cs) != 0 || len(stacks) != 0 || len(pend) != 0 {
		t.Errorf("left after removing: %+v %+v %+v", cs, stacks, pend)
	}
	if _, ok, _ := s.Agent(ctx, id); ok {
		t.Error("the host is still there")
	}
}
