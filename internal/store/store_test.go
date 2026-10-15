// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStateRoundTrip(t *testing.T) {
	ctx, s := context.Background(), open(t)
	now := time.Unix(1_800_000_000, 0)
	st, err := s.LoadState(ctx, now)
	if err != nil || st.Machine.State != wan.Learning || !st.LearningStartedAt.Equal(now) {
		t.Fatalf("fresh state = %+v, %v", st, err)
	}
	st.Machine = wan.Machine{State: wan.FailbackPending, PendingSince: now.Add(time.Minute),
		LastEvidence: now.Add(2 * time.Minute), Flaps: 3, LastFailover: now, LastFailback: now.Add(-time.Hour)}
	st.CurrentASN = 64510
	if err := s.SaveState(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadState(ctx, now.Add(time.Hour))
	same := got.Machine.State == st.Machine.State && got.Machine.Flaps == 3 &&
		got.Machine.PendingSince.Equal(st.Machine.PendingSince) && got.Machine.LastEvidence.Equal(st.Machine.LastEvidence) &&
		got.Machine.LastFailover.Equal(st.Machine.LastFailover) && got.Machine.LastFailback.Equal(st.Machine.LastFailback)
	if err != nil || !same || got.CurrentASN != 64510 || !got.LearningStartedAt.Equal(now) {
		t.Fatalf("reloaded state = %+v, %v", got, err)
	}
}

// Transactions that read before they write must wait for another writer, not
// fail with "database is locked".
func TestConcurrentWriters(t *testing.T) {
	ctx, s := context.Background(), open(t)
	now := time.Unix(1_800_000_000, 0)
	items := []InventoryItem{{Key: "dns", Name: "dns"}, {Key: "media/db", Project: "media", Service: "db", Name: "media-db-1"}}
	errs := make(chan error, 60)
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			_, err := s.SyncInventory(ctx, LocalHost, items, now.Add(time.Duration(i)*time.Second))
			errs <- err
		})
		wg.Go(func() {
			_, _, err := s.SeeASN(ctx, 64500+i%10, "EXAMPLENET-64500", now)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
