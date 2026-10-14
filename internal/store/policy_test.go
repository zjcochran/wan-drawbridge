// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

var media = []InventoryItem{
	{Key: "media/player", Project: "media", Service: "player", Name: "media-player-1"},
	{Key: "media/indexer", Project: "media", Service: "indexer", Name: "media-indexer-1"},
	{Key: "media/fetcher", Project: "media", Service: "fetcher", Name: "media-fetcher-1"},
	{Key: "media/downloader", Project: "media", Service: "downloader", Name: "media-downloader-1"},
	{Key: "automation", Name: "automation"},
}

func effective(t *testing.T, s *Store) map[string]string {
	t.Helper()
	cs, err := s.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range cs {
		out[c.Key] = c.Effective
	}
	return out
}

// wantEffective fails the test for each container whose effective policy is
// not the one in want.
func wantEffective(t *testing.T, s *Store, step string, want map[string]string) {
	t.Helper()
	e := effective(t, s)
	for k, w := range want {
		if e[k] != w {
			t.Errorf("%s: %s = %s, want %s", step, k, e[k], w)
		}
	}
}

func stack(t *testing.T, s *Store, project string) StackRow {
	t.Helper()
	rows, err := s.Stacks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Project == project {
			return r
		}
	}
	t.Fatalf("no stack %q", project)
	return StackRow{}
}

// The worked example from the spec's "Stacks & containers rules" section.
func TestWorkedExample(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	if _, err := s.SyncInventory(ctx, media, now); err != nil {
		t.Fatal(err)
	}

	// 1. Stack set to disable: every container is disabled through the stack.
	if err := s.SetStackPolicy(ctx, "media", true, "test", now); err != nil {
		t.Fatal(err)
	}
	// The standalone container keeps its default, keep.
	wantEffective(t, s, "step 1", map[string]string{
		"media/player": Disable, "media/indexer": Disable, "media/fetcher": Disable, "media/downloader": Disable,
		"automation": Keep,
	})

	// 2. player set to keep running: the stack rule becomes per-container rules.
	converted, err := s.SetContainerPolicy(ctx, "media/player", Keep, "test", now)
	if err != nil || !converted {
		t.Fatalf("step 2: converted=%v err=%v", converted, err)
	}
	wantEffective(t, s, "step 2", map[string]string{
		"media/player": Keep, "media/indexer": Disable, "media/fetcher": Disable, "media/downloader": Disable,
	})

	// 3. The Stacks screen shows Partial, 3 of 4.
	if r := stack(t, s, "media"); r.Status() != "partial" || r.Disabling != 3 || r.Containers != 4 || r.Disabled {
		t.Errorf("step 3: %+v status=%s", r, r.Status())
	}

	// 4. Stack set to disable again: every container follows the stack.
	if err := s.SetStackPolicy(ctx, "media", true, "test", now); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Containers(ctx)
	for _, c := range cs {
		if c.Project == "media" && (c.Policy != Inherit || c.Effective != Disable) {
			t.Errorf("step 4: %s policy=%s effective=%s", c.Key, c.Policy, c.Effective)
		}
	}
}

func TestStackOffResetsOverrides(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	s.SyncInventory(ctx, media, now)
	s.SetContainerPolicy(ctx, "media/indexer", Disable, "test", now)
	if err := s.SetStackPolicy(ctx, "media", false, "test", now); err != nil {
		t.Fatal(err)
	}
	if e := effective(t, s); e["media/indexer"] != Keep {
		t.Errorf("turning the stack off must reset overrides, indexer = %s", e["media/indexer"])
	}
}

func TestOverrideWithoutStackRuleDoesNotConvert(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	s.SyncInventory(ctx, media, now)
	converted, err := s.SetContainerPolicy(ctx, "media/fetcher", Disable, "test", now)
	if err != nil || converted {
		t.Fatalf("converted=%v err=%v", converted, err)
	}
	e := effective(t, s)
	if e["media/fetcher"] != Disable || e["media/player"] != Keep {
		t.Errorf("%v", e)
	}
}

func TestGoneContainersAreDeleted(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	items := append(slices.Clone(media),
		InventoryItem{Key: "runner-1-build", Name: "runner-1-build"},
		InventoryItem{Key: "tools/cli", Project: "tools", Service: "cli", Name: "tools-cli-1"})
	s.SyncInventory(ctx, items, now)
	s.SetContainerPolicy(ctx, "media/player", Disable, "test", now)
	keys := func() []string {
		cs, err := s.Containers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, c.Key)
		}
		slices.Sort(out)
		return out
	}

	// A re-create removes media/player for a moment: it keeps its row and setting.
	s.SyncInventory(ctx, media[1:], now.Add(GoneAfter))
	s.SyncInventory(ctx, media, now.Add(GoneAfter+time.Second))
	if e := effective(t, s); e["media/player"] != Disable {
		t.Errorf("a re-created container lost its setting: %v", e)
	}
	if k := keys(); len(k) != len(media) {
		t.Errorf("containers missing longer than GoneAfter were kept: %v", k)
	}
	stacks, err := s.Stacks(ctx)
	if err != nil || len(stacks) != 1 || stacks[0].Project != "media" {
		t.Errorf("stacks = %+v, %v; want only media", stacks, err)
	}

	// Once nothing is left, everything goes.
	if _, err := s.SyncInventory(ctx, nil, now.Add(3*GoneAfter)); err != nil {
		t.Fatal(err)
	}
	if k := keys(); len(k) != 0 {
		t.Errorf("left after everything was removed: %v", k)
	}
}

func TestNewServiceInSplitStackNeedsReview(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	s.SyncInventory(ctx, media, now)
	s.SetStackPolicy(ctx, "media", true, "test", now)
	s.SetContainerPolicy(ctx, "media/player", Keep, "test", now)

	added, err := s.SyncInventory(ctx, append(media,
		InventoryItem{Key: "media/extra", Project: "media", Service: "extra", Name: "media-extra-1"}), now)
	if err != nil || len(added) != 1 {
		t.Fatalf("added=%v err=%v", added, err)
	}
	cs, _ := s.Containers(ctx)
	for _, c := range cs {
		if c.Key == "media/extra" && (!c.NeedsReview || c.Effective != Keep) {
			t.Errorf("new service = %+v, want keep + needs review", c)
		}
	}
}

func TestUnknownContainerAndBadPolicy(t *testing.T) {
	ctx, s, now := context.Background(), open(t), time.Unix(1_800_000_000, 0)
	if _, err := s.SetContainerPolicy(ctx, "nope", Keep, "test", now); err == nil {
		t.Error("expected error for unknown container")
	}
	if _, err := s.SetContainerPolicy(ctx, "nope", "pause", "test", now); err == nil {
		t.Error("expected error for bad policy")
	}
	if err := s.SetStackPolicy(ctx, "nope", true, "test", now); err == nil {
		t.Error("expected error for unknown stack")
	}
}

func TestLabels(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	items := []InventoryItem{
		{Key: "media/player", Project: "media", Service: "player", Name: "media-player-1", Label: Keep},
		{Key: "media/indexer", Project: "media", Service: "indexer", Name: "media-indexer-1"},
		{Key: "media/fetcher", Project: "media", Service: "fetcher", Name: "media-fetcher-1", Label: Disable},
		{Key: "automation", Name: "automation", Label: Disable},
	}
	if _, err := s.SyncInventory(ctx, items, t0); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"media/player": Keep, "media/indexer": Keep, "media/fetcher": Disable, "automation": Disable}
	if got := effective(t, s); !mapsEqual(got, want) {
		t.Fatalf("labels alone: %v", got)
	}

	// A label beats the stack switch.
	s.SetStackPolicy(ctx, "media", true, "alice", t0)
	want = map[string]string{"media/player": Keep, "media/indexer": Disable, "media/fetcher": Disable, "automation": Disable}
	if got := effective(t, s); !mapsEqual(got, want) {
		t.Fatalf("stack on: %v", got)
	}

	// A UI setting beats the label. Converting the stack leaves labelled
	// containers following their label.
	if conv, err := s.SetContainerPolicy(ctx, "media/indexer", Keep, "alice", t0); err != nil || !conv {
		t.Fatalf("convert: %v %v", conv, err)
	}
	s.SetContainerPolicy(ctx, "automation", Keep, "alice", t0)
	cs, _ := s.Containers(ctx)
	for _, c := range cs {
		if (c.Key == "media/player" || c.Key == "media/fetcher") && c.Policy != Inherit {
			t.Errorf("%s got its own setting %q in the conversion", c.Key, c.Policy)
		}
	}
	want = map[string]string{"media/player": Keep, "media/indexer": Keep, "media/fetcher": Disable, "automation": Keep}
	if got := effective(t, s); !mapsEqual(got, want) {
		t.Fatalf("after overrides: %v", got)
	}

	// A new labelled service in a split stack needs no review; an unlabelled
	// one does until a label appears.
	more := append(slices.Clone(items),
		InventoryItem{Key: "media/subs", Project: "media", Service: "subs", Name: "media-subs-1", Label: Disable},
		InventoryItem{Key: "media/web", Project: "media", Service: "web", Name: "media-web-1"})
	s.SyncInventory(ctx, more, t0)
	review := map[string]bool{}
	cs, _ = s.Containers(ctx)
	for _, c := range cs {
		review[c.Key] = c.NeedsReview
	}
	if review["media/subs"] || !review["media/web"] {
		t.Fatalf("review flags: %v", review)
	}
	more[len(more)-1].Label = Keep
	s.SyncInventory(ctx, more, t0)
	cs, _ = s.Containers(ctx)
	for _, c := range cs {
		if c.Key == "media/web" && (c.NeedsReview || c.Effective != Keep || c.Label != Keep) {
			t.Fatalf("label added later: %+v", c)
		}
	}
	// Removing a label goes back to the stack (no longer switched on).
	more[0].Label = ""
	s.SyncInventory(ctx, more, t0)
	if got := effective(t, s)["media/player"]; got != Keep {
		t.Errorf("label removed: %s", got)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
