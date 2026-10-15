// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestHosts(t *testing.T) {
	ctx, s := context.Background(), open(t)
	hs, err := s.Hosts(ctx)
	if err != nil || len(hs) != 1 || hs[0].ID != LocalHost || hs[0].Name != "local" || hs[0].Kind != "local" || hs[0].Status != HostOnline {
		t.Fatalf("hosts on a new store = %+v, %v", hs, err)
	}
	nas, err := s.AddHost(ctx, "nas", "agent", "docker", "alice", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddHost(ctx, "nas", "agent", "docker", "alice", t0); err == nil {
		t.Error("a second host with the same name was added")
	}
	if names, _ := s.HostNames(ctx); names[nas] != "nas" || names[LocalHost] != "local" {
		t.Errorf("names = %v", names)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetHostReached(ctx, nas, "", t0))
	hs, _ = s.Hosts(ctx)
	if hs[1].Status != HostOnline || !hs[1].LastSeen.Equal(t0) {
		t.Errorf("reached = %+v", hs[1])
	}
	must(s.SetHostReached(ctx, nas, "connection refused", t0.Add(time.Minute)))
	hs, _ = s.Hosts(ctx)
	if hs[1].Status != HostOffline || hs[1].LastError != "connection refused" || !hs[1].LastSeen.Equal(t0) {
		t.Errorf("unreachable = %+v", hs[1])
	}
	// A disabled host stays disabled.
	if _, err := s.db.ExecContext(ctx, `UPDATE hosts SET status = 'disabled' WHERE id = ?`, nas); err != nil {
		t.Fatal(err)
	}
	must(s.SetHostReached(ctx, nas, "", t0))
	if hs, _ = s.Hosts(ctx); hs[1].Status != HostDisabled {
		t.Errorf("disabled host became %s", hs[1].Status)
	}
}

// twoHosts is a store with the same "media" stack on the local host and on
// "nas", where it stops on failover.
func twoHosts(t *testing.T) (*Store, int64) {
	t.Helper()
	ctx, s := context.Background(), open(t)
	nas, _ := s.AddHost(ctx, "nas", "agent", "docker", "alice", t0)
	items := []InventoryItem{
		{Key: "media/player", Project: "media", Service: "player", Name: "media-player-1"},
		{Key: "media/db", Project: "media", Service: "db", Name: "media-db-1"},
	}
	for _, h := range []int64{LocalHost, nas} {
		if _, err := s.SyncInventory(ctx, h, items, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetStackPolicy(ctx, nas, "media", true, "alice", t0); err != nil {
		t.Fatal(err)
	}
	return s, nas
}

// effectiveByHost maps "<host>:<key>" to each container's effective policy.
func effectiveByHost(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Host+":"+r.Key] = r.Effective
	}
	return out
}

// The same stack on two hosts is two stacks: settings and conversions never
// cross hosts.
func TestPoliciesPerHost(t *testing.T) {
	ctx := context.Background()
	s, nas := twoHosts(t)
	effective := func() map[string]string { return effectiveByHost(t, s) }
	if e := effective(); e["nas:media/player"] != Disable || e["local:media/player"] != Keep || len(e) != 4 {
		t.Fatalf("effective = %v", e)
	}
	stacks, _ := s.Stacks(ctx)
	if len(stacks) != 2 || stacks[0].Host != "local" || stacks[0].Disabled || stacks[1].Host != "nas" || !stacks[1].Disabled || stacks[1].Disabling != 2 {
		t.Fatalf("stacks = %+v", stacks)
	}
	// Converting the nas stack leaves the local one alone.
	if conv, err := s.SetContainerPolicy(ctx, nas, "media/player", Keep, "alice", t0); err != nil || !conv {
		t.Fatalf("convert = %v, %v", conv, err)
	}
	if e := effective(); e["nas:media/db"] != Disable || e["nas:media/player"] != Keep || e["local:media/db"] != Keep {
		t.Errorf("after converting = %v", e)
	}
	if _, err := s.SetContainerPolicy(ctx, 99, "media/player", Keep, "alice", t0); err == nil {
		t.Error("a container on an unknown host was set")
	}
}

// Syncing one host does not age out another's containers or stacks.
func TestSyncKeepsOtherHosts(t *testing.T) {
	ctx := context.Background()
	s, nas := twoHosts(t)
	if _, err := s.SyncInventory(ctx, LocalHost, nil, t0.Add(2*GoneAfter)); err != nil {
		t.Fatal(err)
	}
	if e := effectiveByHost(t, s); len(e) != 2 || e["nas:media/db"] == "" {
		t.Errorf("after the local host emptied = %v", e)
	}
	if stacks, _ := s.Stacks(ctx); len(stacks) != 1 || stacks[0].HostID != nas {
		t.Errorf("stacks after the local host emptied = %+v", stacks)
	}
}

func TestPendingStarts(t *testing.T) {
	ctx, s := context.Background(), open(t)
	nas, _ := s.AddHost(ctx, "nas", "agent", "docker", "alice", t0)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	run, _ := s.CreateRun(ctx, "auto", 0, t0)
	must(s.AddRunAction(ctx, run.ID, RunAction{HostID: LocalHost, ContainerID: "c1", Key: "app/web", Name: "web", WasRunning: true}))
	// The same container ID on two hosts is two actions.
	must(s.AddRunAction(ctx, run.ID, RunAction{HostID: nas, ContainerID: "c1", Key: "app/web", Name: "web", WasRunning: true}))
	must(s.MarkStopped(ctx, run.ID, LocalHost, "c1", nil, "", t0))
	must(s.MarkStopped(ctx, run.ID, nas, "c1", []byte(`{"replicas":2}`), "", t0))
	must(s.MarkStarted(ctx, run.ID, LocalHost, "c1", "", t0))
	must(s.MarkPending(ctx, run.ID, nas, "c1", "waiting for the host"))
	must(s.CloseRun(ctx, run.ID, "completed", t0))

	pend, err := s.PendingStarts(ctx)
	if err != nil || len(pend) != 1 || pend[0].RunID != run.ID || pend[0].HostID != nas || pend[0].Host != "nas" ||
		string(pend[0].Restore) != `{"replicas":2}` || !pend[0].Pending || pend[0].Error != "waiting for the host" {
		t.Fatalf("pending = %+v, %v", pend, err)
	}

	// A new failover takes the owed start over: it starts at its failback.
	next, _ := s.CreateRun(ctx, "auto", 0, t0)
	must(s.TakePendingStarts(ctx, next.ID))
	if pend, _ := s.PendingStarts(ctx); len(pend) != 0 {
		t.Errorf("still pending after the takeover: %+v", pend)
	}
	as, _ := s.RunActions(ctx, next.ID)
	if len(as) != 1 || as[0].HostID != nas || !as[0].WasRunning || !as[0].Stopped || string(as[0].Restore) != `{"replicas":2}` {
		t.Fatalf("taken over = %+v", as)
	}

	// Starting a pending action clears it, whether or not it worked.
	must(s.MarkPending(ctx, next.ID, nas, "c1", "waiting"))
	must(s.MarkStarted(ctx, next.ID, nas, "c1", "no such container", t0))
	if pend, _ := s.PendingStarts(ctx); len(pend) != 0 {
		t.Errorf("a failed start stayed pending: %+v", pend)
	}
}

// Upgrading puts existing containers, stacks, runs and usage on the local
// host, and the migration can be undone.
func TestHostsMigration(t *testing.T) {
	ctx := context.Background()
	path := oldDatabase(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stacks, _ := s.Stacks(ctx)
	cs, _ := s.Containers(ctx)
	run, _ := s.OpenRun(ctx)
	as, _ := s.RunActions(ctx, run.ID)
	u, _ := s.OutageUsage(ctx, 1)
	if len(stacks) != 1 || stacks[0].Host != "local" || !stacks[0].Disabled || len(cs) != 1 || cs[0].HostID != LocalHost ||
		len(as) != 1 || as[0].HostID != LocalHost || !as[0].Stopped || len(u) != 1 || u[0].HostID != LocalHost || u[0].RX != 5 {
		t.Fatalf("after the upgrade: stacks %+v containers %+v actions %+v usage %+v", stacks, cs, as, u)
	}

	if err := goose.DownToContext(ctx, s.db, "migrations", 13); err != nil {
		t.Fatalf("down: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM containers WHERE key = 'media/player'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("containers after down = %d, %v", n, err)
	}
}

// oldDatabase is a database at migration 13, before hosts, with a stack, a
// container, an open run and an outage's usage.
func oldDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 13); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO stacks (project, disable_on_failover, updated_at) VALUES ('media', 1, 1)`,
		`INSERT INTO containers (key, project, service, name, first_seen, last_seen) VALUES ('media/player', 'media', 'player', 'media-player-1', 1, 1)`,
		`INSERT INTO runs (id, started_at, trigger) VALUES (1, 1, 'auto')`,
		`INSERT INTO run_actions (run_id, container_id, container_key, name, was_running, stopped_at) VALUES (1, 'c1', 'media/player', 'media-player-1', 1, 1)`,
		`INSERT INTO outages (id, started) VALUES (1, 1)`,
		`INSERT INTO outage_usage (outage_id, container, container_id, rx, tx, last_rx, last_tx, ts) VALUES (1, 'media/player', 'c1', 5, 6, 0, 0, 1)`,
	} {
		// Fixed statements from the list above, no input: test data for an old schema.
		// nosemgrep
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	return path
}

func TestWorkloadKind(t *testing.T) {
	ctx, s := context.Background(), open(t)
	items := []InventoryItem{
		{Key: "media/db", Project: "media", Service: "db", Name: "media_db", Kind: "service", Warning: "scaled back up by an autoscaler"},
		{Key: "dns", Name: "dns"},
	}
	if _, err := s.SyncInventory(ctx, LocalHost, items, t0); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Containers(ctx)
	kinds := map[string]string{}
	for _, c := range cs {
		kinds[c.Key] = c.Kind
		if c.Key == "media/db" && c.Warning != "scaled back up by an autoscaler" {
			t.Errorf("warning = %q", c.Warning)
		}
	}
	if kinds["media/db"] != "service" || kinds["dns"] != "container" {
		t.Errorf("kinds = %v", kinds)
	}
}
