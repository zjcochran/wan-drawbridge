// SPDX-License-Identifier: AGPL-3.0-or-later

// Package monitor runs the check loop: probe, resolve the ISP, learn the
// primary, step the state machine and persist everything. One goroutine owns
// the state; the web UI talks to it through commands and reads Status.
package monitor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/asn"
	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/learner"
	"github.com/zjcochran/wan-drawbridge/internal/probe"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

// Actor carries out container actions. Every call must be safe to repeat.
type Actor interface {
	Stop(ctx context.Context, trigger string, toASN int) error
	Start(ctx context.Context) error
	Apply(ctx context.Context) error
	// Retry catches up with hosts that could not be reached before.
	Retry(ctx context.Context) error
}

// RetryEvery is how often the monitor lets the actor catch up with hosts that
// could not be reached, unless automation is paused.
const RetryEvery = time.Minute

// Status is a snapshot for the UI.
type Status struct {
	State      wan.State
	Since      time.Time
	Due        time.Time     // when the pending change completes if nothing interrupts it
	Hold       time.Duration // current failback wait; grows while the primary flaps
	Flaps      int           // consecutive flaps behind that wait
	Paused     bool
	PausedTill time.Time // when a timed pause ends; zero until resumed by hand
	DrillTill  time.Time // when a failover test ends; zero when none is running
	LastCheck  time.Time
	LastIP     string
	Conclusive bool
	ASN        int    // last conclusive ASN
	ASNName    string // its name from the ASN database
	ISP        int    // its ISP group
	Unmetered  bool   // that ISP is unmetered, so it counts like the primary
	PrimaryISP int
	Busy       string // "stopping" / "starting" while a run is in progress
	Counters   Counters
}

// Counters count since start, for metrics.
type Counters struct {
	ChecksConclusive   uint64
	ChecksInconclusive uint64
	Failovers          uint64 // confirmed by checks
	Failbacks          uint64
	ManualFailovers    uint64
	ManualRestores     uint64
	RunErrors          uint64 // container runs that ended with an error
	Flaps              uint64 // failovers soon after a failback
	LastCheckSeconds   float64
}

// Monitor runs the checks and owns the WAN state: only Run's goroutine reads
// or changes it, and everything else sends it commands.
type Monitor struct {
	retryEvery time.Duration // how often Retry runs
	store      *store.Store
	resolver   *asn.Resolver
	actor      Actor
	log        *slog.Logger
	now        func() time.Time
	notify     func() // called whenever Status changes; may be nil
	cmds       chan func(context.Context)
	done       chan struct{} // closed when Run returns

	// Owned by the loop goroutine.
	cfg       config.Config
	prober    *probe.Prober
	state     store.WANState
	paused    bool
	pausedTil time.Time // end of a timed pause
	drillTil  time.Time // end of a failover test
	lastCheck time.Time
	// announced remembers learner notices already logged, so each is logged once.
	announcedUndecided bool
	announcedSuggest   int

	mu     sync.RWMutex
	status Status
}

// New returns a Monitor; Run starts it.
func New(cfg config.Config, st *store.Store, r *asn.Resolver, a Actor, log *slog.Logger, notify func()) *Monitor {
	return &Monitor{
		cfg: cfg, store: st, resolver: r, actor: a, log: log, notify: notify,
		now:        time.Now,
		retryEvery: RetryEvery,
		cmds:       make(chan func(context.Context), 16),
		done:       make(chan struct{}),
		prober:     probe.New(cfg.IPProviders, cfg.Quorum, cfg.Timeout),
	}
}

// Status returns the latest snapshot.
func (m *Monitor) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// load reads persisted state and settings. Run calls it; tests call it directly.
func (m *Monitor) load(ctx context.Context) error {
	var err error
	if m.state, err = m.store.LoadState(ctx, m.now()); err != nil {
		return err
	}
	d := config.DefaultDetection() // settings saved by older versions lack newer fields
	if ok, err := m.store.GetSetting(ctx, store.SettingDetection, &d); err != nil {
		return err
	} else if ok && d.Validate() == nil {
		m.setDetection(d)
	}
	setup, err := m.store.Flag(ctx, store.SettingSetupComplete)
	if err != nil {
		return err
	}
	paused, err := m.store.Flag(ctx, store.SettingPaused)
	if err != nil {
		return err
	}
	var till time.Time
	if _, err := m.store.GetSetting(ctx, store.SettingPausedUntil, &till); err != nil {
		return err
	}
	// Until the setup wizard is finished, Drawbridge only watches.
	m.paused = paused || !setup
	m.pausedTil = time.Time{}
	if paused && setup {
		m.pausedTil = till
	}
	m.publish(func(*Status) {})
	return nil
}

// Run checks until ctx is cancelled, handling commands between checks.
func (m *Monitor) Run(ctx context.Context) error {
	defer close(m.done)
	if err := m.load(ctx); err != nil {
		return err
	}
	if err := m.resolver.Refresh(ctx); err != nil {
		m.log.Warn("asn database unavailable; using online lookups", "err", err)
	}
	m.log.Info("monitor started", "state", m.state.Machine.State, "interval", m.cfg.CheckInterval, "paused", m.paused)
	if !m.paused {
		m.recover(ctx)
	}

	go m.housekeeping(ctx)

	next := time.After(0)
	retry := time.NewTicker(m.retryEvery)
	defer retry.Stop()
	for {
		var resume, drill <-chan time.Time
		if m.paused && !m.pausedTil.IsZero() {
			resume = time.After(m.pausedTil.Sub(m.now()))
		}
		if !m.drillTil.IsZero() {
			drill = time.After(m.drillTil.Sub(m.now()))
		}
		select {
		case <-ctx.Done():
			return nil
		case cmd := <-m.cmds:
			cmd(ctx)
		case <-resume:
			m.resumeTimed(ctx)
		case <-drill:
			if !m.now().Before(m.drillTil) {
				m.endDrill(ctx, "timer")
			}
		case <-retry.C:
			if !m.paused {
				m.report(m.actor.Retry(ctx))
			}
		case <-next:
			m.Check(ctx)
			wait := m.cfg.CheckInterval
			if m.state.Machine.IsPending() {
				wait = m.cfg.PendingCheck()
			}
			next = time.After(wait)
		}
	}
}

// do runs fn on the loop goroutine and waits for it.
func (m *Monitor) do(ctx context.Context, fn func(context.Context) error) error {
	done := make(chan error, 1)
	select {
	case m.cmds <- func(ctx context.Context) { done <- fn(ctx) }:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return context.Canceled
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return context.Canceled
	}
}

// post runs fn on the loop goroutine without waiting, for long container runs.
func (m *Monitor) post(fn func(context.Context)) {
	go func() {
		select {
		case m.cmds <- fn:
		case <-m.done:
		}
	}()
}

// ApplyDetection stores new detection settings and uses them from the next check.
func (m *Monitor) ApplyDetection(ctx context.Context, d config.Detection) error {
	if err := d.Validate(); err != nil {
		return err
	}
	return m.do(ctx, func(ctx context.Context) error {
		if err := m.store.SetSetting(ctx, store.SettingDetection, d); err != nil {
			return err
		}
		m.setDetection(d)
		m.publish(func(*Status) {})
		m.log.Info("detection settings changed", "interval", d.CheckInterval)
		return nil
	})
}

func (m *Monitor) setDetection(d config.Detection) {
	m.cfg.Detection = d
	m.prober = probe.New(d.IPProviders, d.Quorum, d.Timeout)
}

// Detection returns the settings in use.
func (m *Monitor) Detection(ctx context.Context) (config.Detection, error) {
	var d config.Detection
	err := m.do(ctx, func(context.Context) error { d = m.cfg.Detection; return nil })
	return d, err
}

// SetPaused pauses automation until it is resumed, or resumes it. Resuming
// brings containers in line with the current state, like a restart would.
func (m *Monitor) SetPaused(ctx context.Context, paused bool, actor string) error {
	return m.do(ctx, func(ctx context.Context) error { return m.setPaused(ctx, paused, 0, actor) })
}

// PauseFor pauses automation for d, then resumes it on its own. Pausing again
// replaces the end time.
func (m *Monitor) PauseFor(ctx context.Context, d time.Duration, actor string) error {
	if d <= 0 {
		return errors.New("pause duration must be positive")
	}
	return m.do(ctx, func(ctx context.Context) error { return m.setPaused(ctx, true, d, actor) })
}

// setPaused runs on the loop. d > 0 makes a pause end on its own.
func (m *Monitor) setPaused(ctx context.Context, paused bool, d time.Duration, actor string) error {
	var till time.Time
	if paused && d > 0 {
		till = m.now().Add(d).Truncate(time.Second)
	}
	if err := m.store.SetSetting(ctx, store.SettingPaused, paused); err != nil {
		return err
	}
	if err := m.store.SetSetting(ctx, store.SettingPausedUntil, till); err != nil {
		return err
	}
	if m.paused == paused && m.pausedTil.Equal(till) {
		return nil
	}
	payload := map[string]any{"by": actor}
	if !till.IsZero() {
		payload["until"] = till.UTC().Format(time.RFC3339)
	}
	wasPaused := m.paused
	m.paused, m.pausedTil = paused, till
	if paused || wasPaused {
		m.event(ctx, map[bool]string{true: "paused", false: "resumed"}[paused], payload)
	}
	m.publish(func(*Status) {})
	if !paused && wasPaused {
		m.post(m.recover)
	}
	return nil
}

// resumeTimed ends a timed pause whose time has come.
func (m *Monitor) resumeTimed(ctx context.Context) {
	if !m.paused || m.pausedTil.IsZero() || m.now().Before(m.pausedTil) {
		return
	}
	if err := m.setPaused(ctx, false, 0, "timer"); err != nil {
		m.log.Error("cannot end the timed pause", "err", err)
		return
	}
	m.log.Info("timed pause ended; automation resumed")
}

// CompleteSetup marks the wizard done and turns automation on.
func (m *Monitor) CompleteSetup(ctx context.Context, actor string) error {
	if err := m.store.SetSetting(ctx, store.SettingSetupComplete, true); err != nil {
		return err
	}
	return m.SetPaused(ctx, false, actor)
}

// FailoverNow stops the selected containers as if the backup ISP had been
// confirmed. Drawbridge restores them after the usual failback checks once it
// sees the primary ISP, unless automation is paused.
func (m *Monitor) FailoverNow(ctx context.Context, actor string) error {
	return m.do(ctx, func(ctx context.Context) error {
		m.transition(ctx, wan.Failover, "manual_failover", map[string]any{"by": actor})
		m.publish(func(s *Status) { s.Counters.ManualFailovers++ })
		m.post(func(ctx context.Context) {
			m.run(ctx, "stopping", func() error { return m.actor.Stop(ctx, "manual", m.state.CurrentASN) })
		})
		return nil
	})
}

// RestoreNow starts what the open run stopped and returns to the primary
// state. If traffic is still on a backup ISP, the usual checks fail over again.
func (m *Monitor) RestoreNow(ctx context.Context, actor string) error {
	return m.do(ctx, func(ctx context.Context) error {
		m.transition(ctx, wan.Primary, "manual_restore", map[string]any{"by": actor})
		m.publish(func(s *Status) { s.Counters.ManualRestores++ })
		m.post(func(ctx context.Context) { m.run(ctx, "starting", func() error { return m.actor.Start(ctx) }) })
		return nil
	})
}

// ErrDrill refuses a failover test while containers are already stopped or
// the WAN is not settled on the primary.
var ErrDrill = errors.New("a failover test needs Drawbridge on the primary ISP (or learning) with no containers stopped")

// StartDrill is a failover test: it stops the containers set to stop on
// failover, without a failover, and starts them again after d. The WAN state
// does not change, so checks carry on as usual.
func (m *Monitor) StartDrill(ctx context.Context, d time.Duration, actor string) error {
	if d <= 0 {
		return errors.New("test duration must be positive")
	}
	return m.do(ctx, func(ctx context.Context) error {
		run, err := m.store.OpenRun(ctx)
		if err != nil {
			return err
		}
		st := m.state.Machine.State
		if run != nil || !m.drillTil.IsZero() || (st != wan.Primary && st != wan.Learning) {
			return ErrDrill
		}
		m.drillTil = m.now().Add(d).Truncate(time.Second)
		m.event(ctx, "drill_started", map[string]any{"by": actor, "minutes": int(d.Minutes()),
			"until": m.drillTil.UTC().Format(time.RFC3339)})
		m.publish(func(*Status) {})
		m.post(func(ctx context.Context) {
			m.run(ctx, "stopping", func() error { return m.actor.Stop(ctx, "drill", 0) })
		})
		return nil
	})
}

// EndDrill ends a failover test early.
func (m *Monitor) EndDrill(ctx context.Context, actor string) error {
	return m.do(ctx, func(ctx context.Context) error { m.endDrill(ctx, actor); return nil })
}

// endDrill starts the containers again, unless a real failover has them
// stopped by now. by is who ended it: a user, "timer", or the state change
// that took over.
func (m *Monitor) endDrill(ctx context.Context, by string) {
	if m.drillTil.IsZero() {
		return
	}
	m.drillTil = time.Time{}
	payload := map[string]any{"by": by}
	if m.state.Machine.State == wan.Failover {
		payload["kept_stopped"] = true // the failover now owns them
	} else {
		m.post(func(ctx context.Context) { m.run(ctx, "starting", func() error { return m.actor.Start(ctx) }) })
	}
	m.event(ctx, "drill_ended", payload)
	m.publish(func(*Status) {})
}

// CheckNow runs a check right away and waits for it.
func (m *Monitor) CheckNow(ctx context.Context) error {
	return m.do(ctx, func(ctx context.Context) error { m.Check(ctx); return nil })
}

// Relearn forgets the primary ISP and restarts the learning period. No
// actions are taken until a primary is known again.
func (m *Monitor) Relearn(ctx context.Context, actor string) error {
	return m.do(ctx, func(ctx context.Context) error {
		if err := m.store.ClearPrimary(ctx); err != nil {
			return err
		}
		m.state.LearningStartedAt = m.now()
		m.announcedUndecided, m.announcedSuggest = false, 0
		m.report(m.store.SaveState(ctx, m.state))
		m.event(ctx, "relearn", map[string]any{"by": actor})
		m.publish(func(s *Status) { s.PrimaryISP = 0 })
		return nil
	})
}

// ErrStopped refuses a restore while Drawbridge has containers stopped: the
// open run is what lets it start them again.
var ErrStopped = errors.New("a backup cannot be restored while containers are stopped for a failover; restore after failback")

// LoadBackup runs replace, which overwrites the stored configuration, between
// checks, then reloads the settings and checks right away. It refuses while
// a failover has containers stopped.
func (m *Monitor) LoadBackup(ctx context.Context, replace func(context.Context) error) error {
	return m.do(ctx, func(ctx context.Context) error {
		run, err := m.store.OpenRun(ctx)
		if err != nil {
			return err
		}
		if run != nil || m.state.Machine.State == wan.Failover || m.state.Machine.State == wan.FailbackPending {
			return ErrStopped
		}
		if err := replace(ctx); err != nil {
			return err
		}
		m.announcedUndecided, m.announcedSuggest = false, 0
		if err := m.load(ctx); err != nil {
			return err
		}
		m.log.Info("backup restored", "paused", m.paused)
		m.post(m.Check)
		return nil
	})
}

// PoliciesChanged applies stack and container policy changes right away when
// in failover.
func (m *Monitor) PoliciesChanged() {
	m.post(func(ctx context.Context) {
		if m.state.Machine.State == wan.Failover && !m.paused {
			m.run(ctx, "applying", func() error { return m.actor.Apply(ctx) })
		}
	})
}

func (m *Monitor) transition(ctx context.Context, to wan.State, event string, payload map[string]any) {
	now := m.now()
	from := m.state.Machine.State
	m.state.Machine.State = to // keep the flap history
	m.state.Machine.PendingSince, m.state.Machine.LastEvidence = time.Time{}, time.Time{}
	m.state.Since = now
	m.report(m.store.SaveState(ctx, m.state))
	payload["from"], payload["to"] = from, to
	m.event(ctx, event, payload)
	if by, _ := payload["by"].(string); outage(from) != outage(to) {
		m.report(m.store.SetOutageBy(ctx, outage(to), by))
	}
	m.log.Info("state changed", "from", from, "to", to, "reason", event)
	if to == wan.Failover || event == "manual_restore" {
		by, _ := payload["by"].(string)
		m.drillTakenOver(ctx, by)
	}
	m.publish(func(*Status) {})
}

// drillTakenOver ends a failover test because a failover or a manual restore
// now decides about the containers it stopped: a failover keeps them
// stopped, a restore starts them.
func (m *Monitor) drillTakenOver(ctx context.Context, by string) {
	if m.drillTil.IsZero() {
		return
	}
	m.drillTil = time.Time{}
	payload := map[string]any{"by": by}
	if m.state.Machine.State == wan.Failover {
		payload["kept_stopped"] = true
	}
	m.event(ctx, "drill_ended", payload)
}

// run performs a container run while showing it as busy.
func (m *Monitor) run(ctx context.Context, busy string, fn func() error) {
	m.publish(func(s *Status) { s.Busy = busy })
	err := fn()
	m.report(err)
	if err != nil {
		m.event(ctx, "run_failed", map[string]any{"action": busy, "error": err.Error()})
	}
	m.publish(func(s *Status) {
		s.Busy = ""
		if err != nil {
			s.Counters.RunErrors++
		}
	})
}

// Check runs one probe and advances the state machine.
func (m *Monitor) Check(ctx context.Context) {
	now := m.now()
	started := time.Now()
	res := m.prober.Probe(ctx)
	obs, info, metered := m.observe(ctx, res, now)

	var latency int64
	for _, p := range res.Providers {
		latency = max(latency, p.LatencyMS)
	}
	ip := ""
	if res.Conclusive {
		ip = res.IP.String()
	}
	m.report(m.store.RecordCheck(ctx, store.Check{
		TS: now, IP: ip, ASN: info.ASN, Conclusive: obs.Conclusive, Providers: res.Providers, LatencyMS: latency,
	}))

	if obs.Conclusive {
		m.report(m.store.Observe(ctx, info.ASN, m.elapsed(now), now))
		m.state.CurrentASN = info.ASN
	}
	m.lastCheck = now

	primary := m.learn(ctx, now)
	step := obs
	if obs.Conclusive && primary != 0 && !metered {
		step.ISP = primary // an unmetered ISP counts like the primary
	}
	tr := m.state.Machine.Step(step, primary, m.policy())

	m.log.Debug("check", "ip", ip, "asn", info.ASN, "name", info.Name, "isp", obs.ISP, "conclusive", obs.Conclusive,
		"state", m.state.Machine.State, "due", m.state.Machine.Due(m.policy()))
	if !obs.Conclusive {
		m.log.Info("check inconclusive", "providers", res.Providers)
	}

	if tr.Changed() {
		m.state.Since = now
		m.log.Info("state changed", "from", tr.From, "to", tr.To, "asn", info.ASN, "name", info.Name, "isp", obs.ISP)
		m.event(ctx, "state_changed", map[string]any{
			"from": tr.From, "to": tr.To, "asn": info.ASN, "name": info.Name, "isp": obs.ISP,
		})
	}
	// Save before acting: if Drawbridge dies mid-run, recover() finishes the job.
	m.report(m.store.SaveState(ctx, m.state))
	took := time.Since(started).Seconds()
	m.publish(func(s *Status) {
		s.LastCheck, s.LastIP, s.Conclusive = now, ip, obs.Conclusive
		s.Counters.LastCheckSeconds = took
		if obs.Conclusive {
			s.Counters.ChecksConclusive++
		} else {
			s.Counters.ChecksInconclusive++
		}
		if obs.Conclusive {
			s.ASN, s.ASNName, s.ISP, s.Unmetered = info.ASN, info.Name, obs.ISP, !metered
		}
		s.PrimaryISP = primary
	})

	m.act(ctx, tr, info, obs.ISP)
}

// observe turns a probe result into an observation: conclusive only when the
// IP's ASN is known and its ISP is not ignored. metered is the ISP's flag
// (true when there is none).
func (m *Monitor) observe(ctx context.Context, res probe.Result, now time.Time) (obs wan.Observation, info asn.Info, metered bool) {
	obs, metered = wan.Observation{At: now}, true
	if !res.Conclusive {
		return obs, info, metered
	}
	var err error
	if info, err = m.resolver.Lookup(ctx, res.IP); err != nil {
		m.log.Warn("asn lookup failed", "ip", res.IP, "err", err)
	} else if id, role, err := m.store.SeeASN(ctx, info.ASN, info.Name, now); err != nil {
		m.report(err)
	} else if role != store.RoleIgnored {
		obs = wan.Observation{At: now, Conclusive: true, Clean: res.Clean, ISP: id}
		if metered, err = m.store.Metered(ctx, id); err != nil {
			m.report(err)
			metered = true
		}
	}
	return obs, info, metered
}

// act carries out a transition's action: events, counters and the container
// run, unless automation is paused.
func (m *Monitor) act(ctx context.Context, tr wan.Transition, info asn.Info, isp int) {
	if tr.Action == wan.NoAction {
		// Leaving Learning for Primary (after a relearn mid-failover, say) must
		// still bring back anything an open run holds. Start is a no-op otherwise.
		if tr.From == wan.Learning && tr.To == wan.Primary && !m.paused {
			m.run(ctx, "starting", func() error { return m.actor.Start(ctx) })
		}
		return
	}
	payload := map[string]any{"asn": info.ASN, "name": info.Name, "isp": isp}
	if m.paused {
		m.log.Warn("automation paused: not acting", "action", tr.Action)
		payload["paused"] = true
	}
	switch tr.Action {
	case wan.StopContainers:
		m.publish(func(s *Status) {
			s.Counters.Failovers++
			if tr.Flapped {
				s.Counters.Flaps++
			}
		})
		if tr.Flapped {
			hold := m.state.Machine.Hold(m.policy())
			m.log.Warn("primary ISP is flapping: waiting longer before failback", "failback_wait", hold, "flaps", m.state.Machine.Flaps)
			m.event(ctx, "flapping", map[string]any{"flaps": m.state.Machine.Flaps, "failback_wait": hold.String()})
		}
		m.event(ctx, "failover", payload)
		m.drillTakenOver(ctx, "failover")
		if !m.paused {
			m.run(ctx, "stopping", func() error { return m.actor.Stop(ctx, "auto", info.ASN) })
		}
	case wan.StartContainers:
		m.publish(func(s *Status) { s.Counters.Failbacks++ })
		m.event(ctx, "failback", payload)
		if !m.paused {
			m.run(ctx, "starting", func() error { return m.actor.Start(ctx) })
		}
	}
}

func (m *Monitor) policy() wan.Policy {
	return wan.DefaultPolicy(m.cfg.FailoverAfter, m.cfg.FailbackAfter, m.cfg.FailbackMaxHold, m.cfg.CheckInterval)
}

// recover brings containers in line with the state after a restart, a crash
// mid-run or a resume: in failover it makes sure the selected containers are
// stopped; on the primary it starts whatever an unfinished run still holds.
// Pending states wait for the checks.
func (m *Monitor) recover(ctx context.Context) {
	switch m.state.Machine.State {
	case wan.Failover:
		m.log.Info("in failover: making sure selected containers are stopped")
		m.run(ctx, "stopping", func() error { return m.actor.Stop(ctx, "auto", m.state.CurrentASN) })
	case wan.Primary, wan.Learning:
		m.run(ctx, "starting", func() error { return m.actor.Start(ctx) })
	}
}

// elapsed is the time to credit to the current ISP: the time since the last
// check, capped so a long pause (sleep, restart) is not all credited at once.
func (m *Monitor) elapsed(now time.Time) int64 {
	d := m.cfg.CheckInterval
	if !m.lastCheck.IsZero() {
		d = min(now.Sub(m.lastCheck), 2*m.cfg.CheckInterval)
	}
	return int64(d.Seconds())
}

// learn runs the learner and applies its decision. It returns the primary ISP
// group, or 0 while there is none.
func (m *Monitor) learn(ctx context.Context, now time.Time) int {
	current, pinned, err := m.store.Primary(ctx)
	m.report(err)

	// DRAWBRIDGE_PRIMARY_ASN pins the group that ASN belongs to, once it is seen.
	if m.cfg.PrimaryASN != 0 {
		id, err := m.store.ISPOfASN(ctx, m.cfg.PrimaryASN)
		m.report(err)
		if id == 0 {
			return 0 // not seen yet: no actions until it is
		}
		if id != current || !pinned {
			m.report(m.store.SetRole(ctx, id, store.RolePrimary, true))
			m.log.Info("primary ISP pinned", "isp", id, "asn", m.cfg.PrimaryASN)
			m.event(ctx, "primary_pinned", map[string]any{"isp": id, "asn": m.cfg.PrimaryASN})
		}
		return id
	}

	week, err := m.store.Shares(ctx, now.Add(-7*24*time.Hour))
	m.report(err)
	recent, err := m.store.Shares(ctx, now.Add(-learner.RelearnWindow))
	m.report(err)

	in := learner.Input{
		Current:      current,
		LearningDone: now.Sub(m.state.LearningStartedAt) >= m.cfg.LearningPeriod,
		Week:         week,
		Recent:       recent,
	}
	if pinned {
		in.Pinned = current
	}
	d := learner.Decide(in)

	if d.Primary != 0 && d.Primary != current {
		m.report(m.store.SetRole(ctx, d.Primary, store.RolePrimary, false))
		m.log.Info("primary ISP learned", "isp", d.Primary)
		m.event(ctx, "primary_learned", map[string]any{"isp": d.Primary})
	}
	if d.NeedsUser && !m.announcedUndecided {
		m.announcedUndecided = true
		m.log.Warn("no ISP is clearly primary; pin one on the ISPs page", "shares", week)
		m.event(ctx, "primary_undecided", map[string]any{"shares": week})
	}
	if d.Suggest != 0 && d.Suggest != m.announcedSuggest {
		m.announcedSuggest = d.Suggest
		m.log.Warn("a different ISP now looks primary; pin it if your provider changed", "isp", d.Suggest)
		m.event(ctx, "primary_suggested", map[string]any{"isp": d.Suggest, "current": current})
	}
	return d.Primary
}

// housekeeping prunes old rows and refreshes the ASN database (when it is
// older than a week) once a day.
func (m *Monitor) housekeeping(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		now := m.now()
		var d config.Detection
		if err := m.do(ctx, func(context.Context) error { d = m.cfg.Detection; return nil }); err != nil {
			return
		}
		n, err := m.store.Prune(ctx, now.Add(-d.Retention), now.Add(-max(d.Retention, 8*24*time.Hour)))
		m.report(err)
		if n > 0 {
			m.log.Info("pruned old checks", "rows", n)
		}
		m.report(m.store.PruneSessions(ctx, now))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := m.resolver.Refresh(ctx); err != nil {
			m.log.Warn("asn database refresh failed", "err", err)
		}
	}
}

// publish updates the status snapshot under the lock and notifies listeners.
func (m *Monitor) publish(fn func(*Status)) {
	m.mu.Lock()
	fn(&m.status)
	m.status.State = m.state.Machine.State
	m.status.Due = m.state.Machine.Due(m.policy())
	m.status.Hold = m.state.Machine.Hold(m.policy())
	m.status.Flaps = m.state.Machine.Flaps
	m.status.Since = m.state.Since
	m.status.Paused = m.paused
	m.status.PausedTill = m.pausedTil
	m.status.DrillTill = m.drillTil
	m.mu.Unlock()
	if m.notify != nil {
		m.notify()
	}
}

func (m *Monitor) event(ctx context.Context, typ string, payload map[string]any) {
	m.report(m.store.AddEvent(ctx, m.now(), typ, payload))
}

// report logs an error. The loop keeps running: a failed write should not
// stop monitoring.
func (m *Monitor) report(err error) {
	if err != nil {
		m.log.Error("monitor error", "err", err)
	}
}

// outage reports whether s is part of an outage, which lasts from the
// failover until the failback.
func outage(s wan.State) bool { return s == wan.Failover || s == wan.FailbackPending }
