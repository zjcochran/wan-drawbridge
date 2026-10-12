// SPDX-License-Identifier: AGPL-3.0-or-later

package wan

import (
	"testing"
	"time"
)

const (
	primary = 1
	backup  = 2
)

var (
	t0     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	policy = DefaultPolicy(45*time.Second, 5*time.Minute, 2*time.Hour, 15*time.Second)
)

// sim feeds checks every 15 seconds, like the monitor while a change is pending.
type sim struct {
	m   Machine
	now time.Time
	log []Transition
}

func newSim(s State) *sim { return &sim{m: Machine{State: s}, now: t0} }

func (s *sim) check(o Observation) Transition {
	s.now = s.now.Add(15 * time.Second)
	o.At = s.now
	tr := s.m.Step(o, primary, policy)
	s.log = append(s.log, tr)
	return tr
}

// run feeds n identical checks and counts the actions taken.
func (s *sim) run(n int, o Observation) (stops, starts int) {
	for range n {
		switch s.check(o).Action {
		case StopContainers:
			stops++
		case StartContainers:
			starts++
		}
	}
	return
}

func (s *sim) failedOver() bool { return s.m.State == Failover || s.m.State == FailbackPending }

var (
	onPrimary = Observation{Conclusive: true, Clean: true, ISP: primary}
	onBackup  = Observation{Conclusive: true, Clean: true, ISP: backup}
	dirty     = Observation{Conclusive: true, Clean: false, ISP: primary} // a provider disagreed
	nothing   = Observation{}                                             // inconclusive
	newISP    = Observation{Conclusive: true, Clean: true, ISP: 99}
)

func minutes(m int) int { return m * 4 } // checks in m minutes at 15 s

func TestFailoverAfterDelay(t *testing.T) {
	s := newSim(Primary)
	if stops, _ := s.run(3, onBackup); stops != 0 { // seen for 0, 15, 30 s
		t.Fatalf("failed over before 45 s: %v", s.log)
	}
	if tr := s.check(onBackup); tr.Action != StopContainers || tr.To != Failover {
		t.Fatalf("expected failover at 45 s, got %+v", tr)
	}
}

func TestShortDropIsIgnored(t *testing.T) {
	s := newSim(Primary)
	s.run(2, onBackup)
	s.check(onPrimary)
	if stops, _ := s.run(3, onBackup); stops != 0 || s.m.State != FailoverPending {
		t.Fatalf("the timer must restart after the primary is seen: %+v", s.m)
	}
}

func TestInconclusiveDoesNotBlockFailover(t *testing.T) {
	s := newSim(Primary)
	s.check(onBackup)
	s.check(nothing)
	s.check(onBackup)
	if tr := s.check(onBackup); tr.Action != StopContainers {
		t.Fatalf("an unreachable check between backup sightings must not reset the timer: %+v", s.m)
	}
}

func TestStaleEvidenceStartsOver(t *testing.T) {
	s := newSim(Primary)
	s.run(2, onBackup)
	s.now = s.now.Add(time.Hour) // nothing could be checked for an hour
	if tr := s.check(onBackup); tr.Action != NoAction || s.m.State != FailoverPending || !s.m.PendingSince.Equal(s.now) {
		t.Fatalf("old evidence must not count: %+v", s.m)
	}
}

func TestUnknownISPCountsAsBackup(t *testing.T) {
	s := newSim(Primary)
	if stops, _ := s.run(4, newISP); stops != 1 {
		t.Fatalf("an ISP never seen before should fail over: %v", s.log)
	}
}

func TestFailbackNeedsPrimaryStableForHold(t *testing.T) {
	s := newSim(Failover)
	if _, starts := s.run(minutes(5), onPrimary); starts != 0 {
		t.Fatal("failed back before five minutes")
	}
	if tr := s.check(onPrimary); tr.Action != StartContainers || tr.To != Primary {
		t.Fatalf("expected failback after five stable minutes, got %+v", tr)
	}
}

func TestAnyInstabilityRestartsFailback(t *testing.T) {
	for name, blip := range map[string]Observation{
		"backup sighting":       onBackup,
		"inconclusive check":    nothing,
		"provider disagreement": dirty,
	} {
		s := newSim(Failover)
		s.run(minutes(4), onPrimary)
		s.check(blip)
		if s.m.State != Failover {
			t.Errorf("%s: should drop back to failover, state %s", name, s.m.State)
		}
		if _, starts := s.run(minutes(5), onPrimary); starts != 0 {
			t.Errorf("%s: the five minutes must start again after it", name)
		}
	}
}

func TestDirtyChecksNeverStartFailback(t *testing.T) {
	s := newSim(Failover)
	if _, starts := s.run(minutes(30), dirty); starts != 0 || s.m.State != Failover {
		t.Fatalf("checks where providers disagree must not count toward failback: %+v", s.m)
	}
}

// A primary that is up for 6 minutes and down for 2, over and over.
func TestFlappingBacksOff(t *testing.T) {
	s := newSim(Primary)
	var holds []time.Duration
	stops := 0
	for range 8 {
		st, _ := s.run(minutes(2), onBackup)
		stops += st
		if s.failedOver() {
			holds = append(holds, s.m.Hold(policy))
		}
		s.run(minutes(6), onPrimary)
	}
	// With a 5-minute hold the first cycles fail back after 6 minutes up; once
	// the hold has doubled to 10 minutes it outlasts each recovery, so
	// Drawbridge stays failed over instead of churning containers.
	if stops > 3 {
		t.Errorf("container churn: %d failovers in 8 flaps", stops)
	}
	if holds[len(holds)-1] <= policy.FailbackAfter {
		t.Errorf("hold did not grow: %v", holds)
	}
	if !s.failedOver() {
		t.Error("a flapping primary should leave Drawbridge failed over")
	}
}

func TestHoldDoublesUpToMax(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute,
		80 * time.Minute, 2 * time.Hour, 2 * time.Hour}
	for flaps, w := range want {
		if got := (Machine{Flaps: flaps}).Hold(policy); got != w {
			t.Errorf("flaps=%d: hold %s, want %s", flaps, got, w)
		}
	}
}

func TestFlapsResetAfterQuietPeriod(t *testing.T) {
	s := newSim(Primary)
	s.m.Flaps, s.m.LastFailover, s.m.LastFailback = 4, t0.Add(-7*time.Hour), t0.Add(-7*time.Hour)
	s.run(4, onBackup)
	if s.m.State != Failover || s.m.Flaps != 0 {
		t.Fatalf("six quiet hours must clear the flap count: %+v", s.m)
	}
}

func TestLearningUntilPrimaryKnown(t *testing.T) {
	m := Machine{}
	if tr := m.Step(Observation{At: t0, Conclusive: true, Clean: true, ISP: backup}, 0, policy); tr.To != Learning || tr.Action != NoAction {
		t.Fatalf("expected learning, got %+v", tr)
	}
	if tr := m.Step(Observation{At: t0, Conclusive: true, Clean: true, ISP: primary}, primary, policy); tr.To != Primary {
		t.Fatalf("expected primary once known, got %+v", tr)
	}
}

func TestStartingOnBackupFailsOver(t *testing.T) {
	s := newSim(Learning)
	if stops, _ := s.run(4, onBackup); stops != 1 {
		t.Fatalf("learning ended on the backup: expected failover, got %v", s.log)
	}
}

func TestZeroDelaysActImmediately(t *testing.T) {
	p := DefaultPolicy(0, 0, 0, 15*time.Second)
	m := Machine{State: Primary}
	if tr := m.Step(Observation{At: t0, Conclusive: true, Clean: true, ISP: backup}, primary, p); tr.Action != StopContainers {
		t.Fatalf("zero failover delay: %+v", tr)
	}
	if tr := m.Step(Observation{At: t0.Add(time.Second), Conclusive: true, Clean: true, ISP: primary}, primary, p); tr.Action != StartContainers {
		t.Fatalf("zero failback delay: %+v", tr)
	}
}

func TestDue(t *testing.T) {
	s := newSim(Failover)
	s.check(onPrimary)
	if due := s.m.Due(policy); !due.Equal(s.now.Add(5 * time.Minute)) {
		t.Fatalf("due %s", due)
	}
}
