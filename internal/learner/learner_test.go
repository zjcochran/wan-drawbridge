// SPDX-License-Identifier: AGPL-3.0-or-later

package learner

import "testing"

const day = 24 * 3600

func TestPinnedWins(t *testing.T) {
	d := Decide(Input{Pinned: 1, Current: 2, Week: []Share{{2, 7 * day}}})
	if d.Primary != 1 || d.Reason != "pinned" {
		t.Fatalf("got %+v", d)
	}
}

func TestNoDecisionWhileLearning(t *testing.T) {
	d := Decide(Input{Week: []Share{{1, 100}}})
	if d.Primary != 0 || d.NeedsUser {
		t.Fatalf("got %+v", d)
	}
}

func TestPromotesClearLeader(t *testing.T) {
	d := Decide(Input{LearningDone: true, Week: []Share{{1, 70}, {2, 30}}})
	if d.Primary != 1 {
		t.Fatalf("got %+v", d)
	}
}

func TestAsksUserWhenNoClearLeader(t *testing.T) {
	d := Decide(Input{LearningDone: true, Week: []Share{{1, 55}, {2, 45}}})
	if d.Primary != 0 || !d.NeedsUser {
		t.Fatalf("got %+v", d)
	}
}

func TestSuggestsNewPrimaryWithoutSwitching(t *testing.T) {
	d := Decide(Input{Current: 1, Recent: []Share{{1, day / 4}, {2, 3 * day}}})
	if d.Primary != 1 || d.Suggest != 2 {
		t.Fatalf("got %+v", d)
	}
}

func TestKeepsPrimaryDuringShortOutage(t *testing.T) {
	// A day on backup is not enough data to relearn.
	d := Decide(Input{Current: 1, Recent: []Share{{2, day}}})
	if d.Primary != 1 || d.Suggest != 0 {
		t.Fatalf("got %+v", d)
	}
}
