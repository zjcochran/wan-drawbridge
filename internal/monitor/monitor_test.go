// SPDX-License-Identifier: AGPL-3.0-or-later

package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/asn"
	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

type countingActor struct {
	mu                             sync.Mutex
	stops, starts, applys, retries int
}

func (a *countingActor) Stop(context.Context, string, int) error { a.inc(&a.stops); return nil }
func (a *countingActor) Start(context.Context) error             { a.inc(&a.starts); return nil }
func (a *countingActor) Apply(context.Context) error             { a.inc(&a.applys); return nil }
func (a *countingActor) Retry(context.Context) error             { a.inc(&a.retries); return nil }

func (a *countingActor) retried() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.retries
}

func (a *countingActor) inc(n *int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	*n++
}

func (a *countingActor) counts() (stops, starts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stops, a.starts
}

// fakeWAN serves a switchable public IP and an ipinfo-style ASN lookup.
type fakeWAN struct {
	ip      atomic.Value
	dissent atomic.Value // when set, provider /c reports this IP instead
	srv     *httptest.Server
}

var orgs = map[string]string{
	"203.0.113.3":  "AS64500 EXAMPLENET-64500",
	"203.0.113.9":  "AS64501 EXAMPLENET-64501", // same ISP, another ASN
	"198.51.100.3": "AS64510 SKYLINK-AS64510",
	"192.0.2.9":    "", // unresolvable
}

func newFakeWAN(t *testing.T) *fakeWAN {
	f := &fakeWAN{}
	f.ip.Store("203.0.113.3")
	f.dissent.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := f.dissent.Load().(string); r.URL.Path == "/c" && d != "" {
			io.WriteString(w, d)
			return
		}
		if r.URL.Path == "/no-db" {
			http.NotFound(w, r)
			return
		}
		if ip, ok := strings.CutPrefix(r.URL.Path, "/asn/"); ok {
			fmt.Fprintf(w, `{"org":%q}`, orgs[ip])
			return
		}
		io.WriteString(w, f.ip.Load().(string))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newMonitor(t *testing.T, f *fakeWAN, actor Actor) *Monitor {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	res := asn.NewResolver(dir, log)
	res.OnlineURL = f.srv.URL + "/asn/%s"
	res.DBURL = f.srv.URL + "/no-db" // 404: online lookups only
	cfg := config.Config{Detection: config.DefaultDetection()}
	cfg.CheckInterval = time.Minute
	// Three providers on one server, quorum 2: /c can be made to disagree.
	cfg.IPProviders = []string{f.srv.URL + "/a", f.srv.URL + "/b", f.srv.URL + "/c"}
	cfg.Timeout = 2 * time.Second

	m := New(cfg, st, res, actor, log, nil)
	clock.Store(1_800_000_000)
	m.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	if err := st.SetSetting(ctx, store.SettingSetupComplete, true); err != nil {
		t.Fatal(err)
	}
	if err := m.load(ctx); err != nil {
		t.Fatal(err)
	}
	return m
}

// clock is the fake time; tests are not parallel, so one is enough.
var clock atomic.Int64

// checks advances the clock by every and runs a check, n times.
func checks(m *Monitor, n int, every time.Duration) {
	for range n {
		clock.Add(int64(every.Seconds()))
		m.Check(context.Background())
	}
}

const pending = 15 * time.Second // the check interval while a change is pending

func TestFailoverAndFailbackEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	actor := &countingActor{}
	m := newMonitor(t, f, actor)
	m.cfg.PrimaryASN = 64500

	m.Check(ctx)
	if m.state.Machine.State != wan.Primary {
		t.Fatalf("expected primary, got %s", m.state.Machine.State)
	}
	actor.starts = 0 // leaving Learning finishes any open run (a no-op here)

	f.ip.Store("198.51.100.3")
	checks(m, 3, pending) // backup seen for 0, 15, 30 s
	if actor.stops != 0 {
		t.Fatal("failed over before 45 s")
	}
	checks(m, 1, pending)
	if m.state.Machine.State != wan.Failover || actor.stops != 1 {
		t.Fatalf("expected failover with one stop, got %s stops=%d", m.state.Machine.State, actor.stops)
	}

	f.ip.Store("203.0.113.3")
	checks(m, 20, pending) // primary stable for 0 … 4m45s
	if actor.starts != 0 {
		t.Fatal("started before the primary was stable for 5 minutes")
	}
	checks(m, 1, pending)
	if m.state.Machine.State != wan.Primary || actor.starts != 1 {
		t.Fatalf("expected failback with one start, got %s starts=%d", m.state.Machine.State, actor.starts)
	}

	// State survives a restart.
	saved, err := m.store.LoadState(ctx, time.Now())
	if err != nil || saved.Machine.State != wan.Primary || saved.CurrentASN != 64500 {
		t.Fatalf("saved state = %+v, %v", saved, err)
	}
}

func TestUnresolvableIPIsInconclusive(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	actor := &countingActor{}
	m := newMonitor(t, f, actor)
	m.cfg.PrimaryASN = 64500
	m.Check(ctx)

	f.ip.Store("192.0.2.9")
	checks(m, 10, pending)
	if m.state.Machine.State != wan.Primary || actor.stops != 0 {
		t.Fatalf("unresolvable checks moved the machine: %s", m.state.Machine.State)
	}
}

func TestLearnsPrimaryAfterLearningPeriod(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	m := newMonitor(t, f, &countingActor{})
	m.cfg.LearningPeriod = 10 * time.Minute

	checks(m, 5, time.Minute)
	if m.state.Machine.State != wan.Learning {
		t.Fatalf("decided too early: %s", m.state.Machine.State)
	}
	checks(m, 10, time.Minute)
	primary, pinned, err := m.store.Primary(ctx)
	examplenet, _ := m.store.ISPOfASN(ctx, 64500)
	if err != nil || primary != examplenet || pinned || m.state.Machine.State != wan.Primary {
		t.Fatalf("primary=%d pinned=%v state=%s err=%v", primary, pinned, m.state.Machine.State, err)
	}
}

func TestRecoverFinishesInterruptedRuns(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	cases := map[wan.State][2]int{ // state -> {stops, starts}
		wan.Failover:        {1, 0},
		wan.Primary:         {0, 1},
		wan.Learning:        {0, 1},
		wan.FailoverPending: {0, 0},
		wan.FailbackPending: {0, 0},
	}
	for state, want := range cases {
		actor := &countingActor{}
		m := newMonitor(t, f, actor)
		m.state.Machine.State = state
		m.recover(ctx)
		if actor.stops != want[0] || actor.starts != want[1] {
			t.Errorf("%s: stops=%d starts=%d, want %v", state, actor.stops, actor.starts, want)
		}
	}
}

func TestSameISPOnAnotherASNIsNotAFailover(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	actor := &countingActor{}
	m := newMonitor(t, f, actor)
	m.cfg.PrimaryASN = 64500
	m.Check(ctx)

	f.ip.Store("203.0.113.9") // reconnect lands on AS64501
	checks(m, 10, pending)
	if m.state.Machine.State != wan.Primary || actor.stops != 0 {
		t.Fatalf("another Examplenet ASN triggered failover: state=%s stops=%d", m.state.Machine.State, actor.stops)
	}
}

func TestUnmeteredBackupIsNotAFailover(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	actor := &countingActor{}
	m := newMonitor(t, f, actor)
	m.cfg.PrimaryASN = 64500
	m.Check(ctx)

	f.ip.Store("198.51.100.3")
	m.Check(ctx) // the backup is seen, and starts metered: failover pending
	if m.state.Machine.State != wan.FailoverPending {
		t.Fatalf("state = %s", m.state.Machine.State)
	}
	isp, _ := m.store.ISPOfASN(ctx, 64510)
	if err := m.store.SetMetered(ctx, isp, false); err != nil {
		t.Fatal(err)
	}
	checks(m, 10, pending)
	if m.state.Machine.State != wan.Primary || actor.stops != 0 {
		t.Fatalf("an unmetered backup triggered failover: state=%s stops=%d", m.state.Machine.State, actor.stops)
	}
	if s := m.Status(); s.ISP != isp || !s.Unmetered {
		t.Errorf("status = ISP %d, unmetered %v", s.ISP, s.Unmetered)
	}
}

func TestPausedObservesButDoesNotAct(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	actor := &countingActor{}
	m := newMonitor(t, f, actor)
	m.cfg.PrimaryASN = 64500
	m.paused = true
	m.Check(ctx)

	f.ip.Store("198.51.100.3")
	checks(m, 4, pending)
	if m.state.Machine.State != wan.Failover || actor.stops != 0 {
		t.Fatalf("paused: state=%s stops=%d", m.state.Machine.State, actor.stops)
	}
}

func TestSetupIncompleteStartsPaused(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, newFakeWAN(t), &countingActor{})
	if err := m.store.SetSetting(ctx, store.SettingSetupComplete, false); err != nil {
		t.Fatal(err)
	}
	if err := m.load(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Paused {
		t.Fatal("monitor must stay paused until the setup wizard is complete")
	}
}

// runMonitor starts the loop and returns a stop function.
func runMonitor(m *Monitor) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The loop lets the actor catch up with unreachable hosts, but not while
// automation is paused.
func TestRetryLoop(t *testing.T) {
	ctx := context.Background()
	actor := &countingActor{}
	m := newMonitor(t, newFakeWAN(t), actor)
	m.retryEvery = 5 * time.Millisecond
	if err := m.store.SetSetting(ctx, store.SettingSetupComplete, true); err != nil {
		t.Fatal(err)
	}
	if err := m.store.SetSetting(ctx, store.SettingPaused, true); err != nil {
		t.Fatal(err)
	}
	stop := runMonitor(m)
	waitFor(t, "paused", func() bool { return m.Status().Paused })
	time.Sleep(30 * time.Millisecond)
	if n := actor.retried(); n != 0 {
		t.Errorf("retried %d times while paused", n)
	}
	if err := m.SetPaused(ctx, false, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a retry", func() bool { return actor.retried() > 0 })
	stop()
}

func TestManualFailoverRestoreAndResume(t *testing.T) {
	ctx := context.Background()
	actor := &countingActor{}
	m := newMonitor(t, newFakeWAN(t), actor)
	m.cfg.PrimaryASN = 64500
	stop := runMonitor(m)
	defer stop()
	waitFor(t, "primary", func() bool { return m.Status().State == wan.Primary })

	if err := m.FailoverNow(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "manual stop", func() bool { s, _ := actor.counts(); return s == 1 })
	if m.Status().State != wan.Failover {
		t.Fatalf("state = %s", m.Status().State)
	}

	_, before := actor.counts()
	if err := m.RestoreNow(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "manual start", func() bool { _, s := actor.counts(); return s > before })
	if m.Status().State != wan.Primary {
		t.Fatalf("state = %s", m.Status().State)
	}
	if os, err := m.store.Outages(ctx, store.OutageFilter{}); err != nil || len(os) != 1 || os[0].Open() ||
		os[0].StartedBy != "test" || os[0].EndedBy != "test" {
		t.Fatalf("outages = %+v, %v", os, err)
	}

	if err := m.SetPaused(ctx, true, "test"); err != nil || !m.Status().Paused {
		t.Fatalf("pause: %v", err)
	}
	_, before = actor.counts()
	if err := m.SetPaused(ctx, false, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resume recover", func() bool { _, s := actor.counts(); return s > before })
}

func TestApplyDetectionValidates(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, newFakeWAN(t), &countingActor{})
	stop := runMonitor(m)
	defer stop()

	d, _ := m.Detection(ctx)
	d.CheckInterval = time.Second
	if err := m.ApplyDetection(ctx, d); err == nil {
		t.Fatal("1s interval accepted")
	}
	d.CheckInterval = 2 * time.Minute
	if err := m.ApplyDetection(ctx, d); err != nil {
		t.Fatal(err)
	}
	var saved config.Detection
	if ok, _ := m.store.GetSetting(ctx, store.SettingDetection, &saved); !ok || saved.CheckInterval != 2*time.Minute {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestDisagreeingProviderHoldsOffFailback(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	actor := &countingActor{}
	m := newMonitor(t, f, actor)
	m.cfg.PrimaryASN = 64500
	m.Check(ctx)
	actor.starts = 0 // leaving Learning finishes any open run (a no-op here)
	f.ip.Store("198.51.100.3")
	checks(m, 4, pending)

	// The primary is back, but one provider still sees the backup address.
	f.ip.Store("203.0.113.3")
	f.dissent.Store("198.51.100.3")
	checks(m, 60, pending) // 15 minutes
	if _, starts := actor.counts(); starts != 0 || m.state.Machine.State != wan.Failover {
		t.Fatalf("failed back while providers disagreed: state=%s starts=%d", m.state.Machine.State, starts)
	}
	f.dissent.Store("")
	checks(m, 21, pending)
	if _, starts := actor.counts(); starts != 1 {
		t.Fatalf("did not fail back after five clean minutes: state=%s", m.state.Machine.State)
	}
}

func TestFlappingIsReportedAndBacksOff(t *testing.T) {
	ctx := context.Background()
	f := newFakeWAN(t)
	actor := &countingActor{}
	m := newMonitor(t, f, actor)
	m.cfg.PrimaryASN = 64500
	m.Check(ctx)
	for range 3 { // down 2 minutes, up 6 minutes
		f.ip.Store("198.51.100.3")
		checks(m, 8, pending)
		f.ip.Store("203.0.113.3")
		checks(m, 24, pending)
	}
	st := m.Status()
	if st.Flaps != 1 || st.Hold != 10*time.Minute || st.Counters.Flaps != 1 {
		t.Fatalf("flaps=%d hold=%s counter=%d", st.Flaps, st.Hold, st.Counters.Flaps)
	}
	if stops, _ := actor.counts(); stops != 2 {
		t.Fatalf("stops = %d, want 2: the doubled wait must outlast the next recovery", stops)
	}
	events, _ := m.store.Events(ctx, store.EventFilter{Type: "flapping"})
	if len(events) != 1 {
		t.Fatalf("flapping events = %d", len(events))
	}
}

func TestLoadBackupReloadsAndRefusesDuringFailover(t *testing.T) {
	ctx := context.Background()
	actor := &countingActor{}
	m := newMonitor(t, newFakeWAN(t), actor)
	m.cfg.PrimaryASN = 64500
	stop := runMonitor(m)
	defer stop()
	waitFor(t, "primary", func() bool { return m.Status().State == wan.Primary })

	pause := func(ctx context.Context) error { return m.store.SetSetting(ctx, store.SettingPaused, true) }
	if err := m.LoadBackup(ctx, pause); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Paused {
		t.Fatal("settings from the backup were not reloaded")
	}
	broken := errors.New("broken backup")
	if err := m.LoadBackup(ctx, func(context.Context) error { return broken }); !errors.Is(err, broken) {
		t.Fatalf("replace error: %v", err)
	}

	if err := m.FailoverNow(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	called := false
	err := m.LoadBackup(ctx, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, ErrStopped) || called {
		t.Fatalf("during failover: err=%v called=%v", err, called)
	}
}

func TestLoadBackupRefusesWithOpenRun(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, newFakeWAN(t), &countingActor{})
	if _, err := m.store.CreateRun(ctx, "auto", 64510, time.Now()); err != nil {
		t.Fatal(err)
	}
	stop := runMonitor(m)
	defer stop()
	if err := m.LoadBackup(ctx, func(context.Context) error { return nil }); !errors.Is(err, ErrStopped) {
		t.Fatalf("open run: %v", err)
	}
}

func TestPauseFor(t *testing.T) {
	ctx := context.Background()
	actor := &countingActor{}
	m := newMonitor(t, newFakeWAN(t), actor)
	stop := runMonitor(m)
	defer stop()

	if err := m.PauseFor(ctx, 0, "test"); err == nil {
		t.Fatal("zero duration accepted")
	}
	if err := m.PauseFor(ctx, time.Second, "alice"); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if !st.Paused || !st.PausedTill.Equal(m.now().Add(time.Second)) {
		t.Fatalf("status = paused %v till %v", st.Paused, st.PausedTill)
	}

	// A restart keeps the end time.
	other := New(m.cfg, m.store, m.resolver, actor, m.log, nil)
	other.now = m.now
	if err := other.load(ctx); err != nil || !other.paused || !other.pausedTil.Equal(st.PausedTill) {
		t.Fatalf("reloaded: paused %v till %v (%v)", other.paused, other.pausedTil, err)
	}

	// The timer fires on the wall clock but resumes only once the monitor's
	// clock has reached the end time.
	clock.Add(1)
	waitFor(t, "timed resume", func() bool { return !m.Status().Paused })
	if !m.Status().PausedTill.IsZero() {
		t.Error("end time kept after resuming")
	}
	evs, err := m.store.Events(ctx, store.EventFilter{Type: "resumed", Limit: 1})
	if err != nil || len(evs) != 1 || evs[0].Payload["by"] != "timer" {
		t.Fatalf("resumed event = %v (%v)", evs, err)
	}
	paused, _ := m.store.Flag(ctx, store.SettingPaused)
	var till time.Time
	m.store.GetSetting(ctx, store.SettingPausedUntil, &till)
	if paused || !till.IsZero() {
		t.Errorf("saved: paused %v till %v", paused, till)
	}

	// Pausing until resumed clears an earlier end time.
	m.PauseFor(ctx, time.Hour, "alice")
	m.SetPaused(ctx, true, "alice")
	if st := m.Status(); !st.Paused || !st.PausedTill.IsZero() {
		t.Fatalf("open-ended pause: %v %v", st.Paused, st.PausedTill)
	}
}

func TestTimedPauseIgnoredBeforeSetup(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, newFakeWAN(t), &countingActor{})
	m.store.SetSetting(ctx, store.SettingSetupComplete, false)
	m.store.SetSetting(ctx, store.SettingPaused, true)
	m.store.SetSetting(ctx, store.SettingPausedUntil, m.now().Add(-time.Minute))
	if err := m.load(ctx); err != nil {
		t.Fatal(err)
	}
	m.resumeTimed(ctx)
	if !m.paused {
		t.Fatal("a timed pause resumed automation before setup was finished")
	}
}

type failingActor struct{ countingActor }

func (a *failingActor) Stop(context.Context, string, int) error {
	return errors.New("docker unreachable")
}

func TestRunFailedEvent(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, newFakeWAN(t), &failingActor{})
	m.run(ctx, "stopping", func() error { return m.actor.Stop(ctx, "manual", 0) })
	evs, err := m.store.Events(ctx, store.EventFilter{Type: "run_failed"})
	if err != nil || len(evs) != 1 || evs[0].Payload["action"] != "stopping" || evs[0].Payload["error"] != "docker unreachable" {
		t.Fatalf("events = %v (%v)", evs, err)
	}
	if m.Status().Counters.RunErrors != 1 {
		t.Error("run error not counted")
	}
}

func drillEvents(t *testing.T, m *Monitor, typ string) []store.Event {
	t.Helper()
	evs, err := m.store.Events(context.Background(), store.EventFilter{Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestDrill(t *testing.T) {
	ctx := context.Background()
	actor := &countingActor{}
	m := newMonitor(t, newFakeWAN(t), actor)
	m.cfg.PrimaryASN = 64500
	stop := runMonitor(m)
	defer stop()
	waitFor(t, "primary", func() bool { return m.Status().State == wan.Primary })

	if err := m.StartDrill(ctx, 0, "alice"); err == nil {
		t.Fatal("zero duration accepted")
	}
	if err := m.StartDrill(ctx, time.Second, "alice"); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); !st.DrillTill.Equal(m.now().Add(time.Second)) || st.State != wan.Primary {
		t.Fatalf("status: %+v", st)
	}
	waitFor(t, "drill stop", func() bool { s, _ := actor.counts(); return s == 1 })
	if err := m.StartDrill(ctx, time.Second, "alice"); !errors.Is(err, ErrDrill) {
		t.Fatalf("second test: %v", err)
	}
	if evs := drillEvents(t, m, "drill_started"); len(evs) != 1 || evs[0].Payload["by"] != "alice" {
		t.Fatalf("started: %v", evs)
	}

	// Time is up: the containers start again.
	_, before := actor.counts()
	clock.Add(1)
	waitFor(t, "drill end", func() bool { _, s := actor.counts(); return s > before })
	if !m.Status().DrillTill.IsZero() {
		t.Error("still shown as running")
	}
	if evs := drillEvents(t, m, "drill_ended"); len(evs) != 1 || evs[0].Payload["by"] != "timer" {
		t.Fatalf("ended: %v", evs)
	}

	// Ended early by hand.
	m.StartDrill(ctx, time.Hour, "alice")
	_, before = actor.counts()
	m.EndDrill(ctx, "bob")
	waitFor(t, "early end", func() bool { _, s := actor.counts(); return s > before })
	if evs := drillEvents(t, m, "drill_ended"); evs[0].Payload["by"] != "bob" {
		t.Fatalf("early end: %v", evs[0])
	}
	m.EndDrill(ctx, "bob") // nothing running: no event
	if n := len(drillEvents(t, m, "drill_ended")); n != 2 {
		t.Errorf("%d drill_ended events", n)
	}

	// Restore now ends it; the restore starts the containers.
	m.StartDrill(ctx, time.Hour, "alice")
	m.RestoreNow(ctx, "carol")
	if evs := drillEvents(t, m, "drill_ended"); evs[0].Payload["by"] != "carol" || evs[0].Payload["kept_stopped"] != nil {
		t.Fatalf("restore: %v", evs[0])
	}

	// A failover takes over: the containers stay stopped.
	m.StartDrill(ctx, time.Hour, "alice")
	m.FailoverNow(ctx, "dave")
	if evs := drillEvents(t, m, "drill_ended"); evs[0].Payload["by"] != "dave" || evs[0].Payload["kept_stopped"] != true {
		t.Fatalf("failover: %v", evs[0])
	}
	if err := m.StartDrill(ctx, time.Minute, "alice"); !errors.Is(err, ErrDrill) {
		t.Fatalf("test during a failover: %v", err)
	}
	if !m.Status().DrillTill.IsZero() {
		t.Error("test still shown after the failover")
	}
}

func TestDrillRefusedWithStoppedContainers(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, newFakeWAN(t), &countingActor{})
	stop := runMonitor(m)
	defer stop()
	if _, err := m.store.CreateRun(ctx, "manual", 0, m.now()); err != nil {
		t.Fatal(err)
	}
	if err := m.StartDrill(ctx, time.Minute, "alice"); !errors.Is(err, ErrDrill) {
		t.Fatalf("open run: %v", err)
	}
}

func TestDrillEndsWithAutomaticFailover(t *testing.T) {
	ctx := context.Background()
	m := newMonitor(t, newFakeWAN(t), &countingActor{})
	m.drillTil = m.now().Add(time.Hour)
	m.state.Machine.State = wan.Failover
	m.drillTakenOver(ctx, "failover")
	if evs := drillEvents(t, m, "drill_ended"); len(evs) != 1 || evs[0].Payload["by"] != "failover" || evs[0].Payload["kept_stopped"] != true {
		t.Fatalf("events: %v", evs)
	}
}
