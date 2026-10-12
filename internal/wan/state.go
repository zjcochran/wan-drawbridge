// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wan holds the failover state machine. It is pure logic: callers feed
// it one observation per check and act on the transition it returns.
//
// Going to the backup and coming back are judged differently. Failover needs
// the backup ISP seen continuously for FailoverAfter; checks that reach nothing
// are ignored, because a dead primary often cannot be checked at all. Failback
// needs the primary stable for the failback hold: every check in that time must
// be clean and on the primary, and anything else restarts the clock. A primary
// that keeps flapping doubles the hold each time, up to MaxHold.
package wan

import "time"

// State is where the WAN connection stands relative to the primary ISP.
type State string

const (
	Learning        State = "learning"         // no primary ISP known yet
	Primary         State = "primary"          // on the primary ISP
	FailoverPending State = "failover_pending" // backup seen, confirming
	Failover        State = "failover"         // on backup, containers stopped
	FailbackPending State = "failback_pending" // primary seen again, confirming it is stable
)

// Action is what the caller must do as a result of a transition.
type Action string

const (
	NoAction        Action = ""
	StopContainers  Action = "stop"
	StartContainers Action = "start"
)

// Policy sets the timings.
type Policy struct {
	FailoverAfter time.Duration // backup seen this long before failover
	FailbackAfter time.Duration // primary stable this long before failback
	MaxHold       time.Duration // longest failback hold while flapping
	FlapWindow    time.Duration // a failover this soon after a failback is a flap
	FlapReset     time.Duration // this long without a failover clears the flap count
	StaleAfter    time.Duration // evidence older than this no longer counts
}

// DefaultPolicy fills in the timings not exposed as settings.
func DefaultPolicy(failoverAfter, failbackAfter, maxHold, checkInterval time.Duration) Policy {
	return Policy{
		FailoverAfter: failoverAfter, FailbackAfter: failbackAfter, MaxHold: maxHold,
		FlapWindow: time.Hour, FlapReset: 6 * time.Hour, StaleAfter: 3 * checkInterval,
	}
}

// Machine is the persisted state.
type Machine struct {
	State        State
	PendingSince time.Time // when the pending change was first seen
	LastEvidence time.Time // last check that supported the pending change
	Flaps        int       // consecutive flaps; each doubles the failback hold
	LastFailover time.Time
	LastFailback time.Time
}

// Observation is the result of one check.
type Observation struct {
	At         time.Time
	Conclusive bool
	Clean      bool // conclusive and no provider disagreed
	ISP        int  // ISP group traffic left through
}

// Transition describes the outcome of feeding one observation.
type Transition struct {
	From, To State
	Action   Action
	Flapped  bool // this failover counted as a flap
}

// Changed reports whether the state moved.
func (t Transition) Changed() bool { return t.From != t.To }

// Hold is the failback hold for the current flap count.
func (m Machine) Hold(p Policy) time.Duration {
	h := p.FailbackAfter
	for i := 0; i < m.Flaps && h < p.MaxHold; i++ {
		h *= 2
	}
	return min(h, max(p.MaxHold, p.FailbackAfter))
}

// Due is when the pending change completes if nothing interrupts it, or zero
// when nothing is pending.
func (m Machine) Due(p Policy) time.Time {
	switch m.State {
	case FailoverPending:
		return m.PendingSince.Add(p.FailoverAfter)
	case FailbackPending:
		return m.PendingSince.Add(m.Hold(p))
	}
	return time.Time{}
}

// IsPending reports whether a change is being confirmed, so checks should run faster.
func (m Machine) IsPending() bool {
	return m.State == FailoverPending || m.State == FailbackPending
}

// Step advances the machine by one observation. primaryISP is 0 while no
// primary is known.
func (m *Machine) Step(obs Observation, primaryISP int, p Policy) Transition {
	if m.State == "" {
		m.State = Learning
	}
	from := m.State
	t := func(a Action) Transition { return Transition{From: from, To: m.State, Action: a} }

	// A pending change whose evidence went stale starts over.
	if m.IsPending() && !m.LastEvidence.IsZero() && obs.At.Sub(m.LastEvidence) > p.StaleAfter {
		if m.State == FailoverPending {
			m.State = Primary
		} else {
			m.State = Failover
		}
	}

	if primaryISP == 0 {
		if obs.Conclusive {
			m.State = Learning
		}
		return t(NoAction)
	}
	onPrimary := obs.Conclusive && obs.ISP == primaryISP
	onBackup := obs.Conclusive && obs.ISP != primaryISP

	switch m.State {
	case Learning, Primary:
		switch {
		case onPrimary:
			m.State = Primary
		case onBackup:
			m.begin(FailoverPending, obs.At)
			return m.tryFailover(obs.At, p, from)
		}

	case FailoverPending:
		switch {
		case onPrimary:
			m.State = Primary
		case onBackup:
			m.LastEvidence = obs.At
			return m.tryFailover(obs.At, p, from)
		}
		// Inconclusive: no evidence either way; keep waiting.

	case Failover:
		if onPrimary && obs.Clean {
			m.begin(FailbackPending, obs.At)
			return m.tryFailback(obs.At, p, from)
		}

	case FailbackPending:
		if !onPrimary || !obs.Clean {
			// The primary is not stable yet: back to waiting for a clean check.
			m.State = Failover
			return t(NoAction)
		}
		m.LastEvidence = obs.At
		return m.tryFailback(obs.At, p, from)
	}
	return t(NoAction)
}

func (m *Machine) begin(s State, at time.Time) {
	m.State, m.PendingSince, m.LastEvidence = s, at, at
}

func (m *Machine) tryFailover(at time.Time, p Policy, from State) Transition {
	if at.Sub(m.PendingSince) < p.FailoverAfter {
		return Transition{From: from, To: m.State}
	}
	if m.LastFailover.IsZero() || at.Sub(m.LastFailover) >= p.FlapReset {
		m.Flaps = 0
	}
	flapped := !m.LastFailback.IsZero() && at.Sub(m.LastFailback) < p.FlapWindow
	if flapped {
		m.Flaps++
	}
	m.State, m.LastFailover = Failover, at
	return Transition{From: from, To: Failover, Action: StopContainers, Flapped: flapped}
}

func (m *Machine) tryFailback(at time.Time, p Policy, from State) Transition {
	if at.Sub(m.PendingSince) < m.Hold(p) {
		return Transition{From: from, To: m.State}
	}
	m.State, m.LastFailback = Primary, at
	return Transition{From: from, To: Primary, Action: StartContainers}
}
