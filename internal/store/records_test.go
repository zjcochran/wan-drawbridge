// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func TestRunLifecycle(t *testing.T) {
	ctx, s := context.Background(), open(t)
	if run, err := s.OpenRun(ctx); run != nil || err != nil {
		t.Fatalf("fresh store has an open run: %+v, %v", run, err)
	}
	run, err := s.CreateRun(ctx, "auto", 64510, t0)
	if err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenRun(ctx); open == nil || open.ID != run.ID || open.ToASN != 64510 || open.Trigger != "auto" {
		t.Fatalf("OpenRun = %+v", open)
	}

	db := RunAction{ContainerID: "c1", Key: "app/db", Name: "db", WasRunning: true}
	web := RunAction{ContainerID: "c2", Key: "app/web", Name: "web", WasRunning: true}
	for _, a := range []RunAction{db, web} {
		if err := s.AddRunAction(ctx, run.ID, a); err != nil {
			t.Fatal(err)
		}
	}
	// Adding again (a second stop pass) keeps the original was_running.
	db.WasRunning = false
	s.AddRunAction(ctx, run.ID, db)

	s.MarkStopped(ctx, run.ID, "c1", "", t0)
	s.MarkStopped(ctx, run.ID, "c2", "timeout", t0)
	s.MarkStarted(ctx, run.ID, "c1", "", t0)
	s.MarkStopped(ctx, run.ID, "c1", "", t0) // disabled again: the earlier start is cleared
	s.MarkStarted(ctx, run.ID, "c2", "no such container", t0)

	actions, err := s.RunActions(ctx, run.ID)
	if err != nil || len(actions) != 2 {
		t.Fatalf("RunActions = %+v, %v", actions, err)
	}
	want := []RunAction{
		{ContainerID: "c1", Key: "app/db", Name: "db", WasRunning: true, Stopped: true},
		{ContainerID: "c2", Key: "app/web", Name: "web", WasRunning: true, Error: "no such container"},
	}
	if !slices.Equal(actions, want) {
		t.Fatalf("actions\n got %+v\nwant %+v", actions, want)
	}

	if err := s.CloseRun(ctx, run.ID, "completed", t0); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenRun(ctx); open != nil {
		t.Fatalf("closed run still open: %+v", open)
	}

	manual, _ := s.CreateRun(ctx, "manual", 0, t0)
	if open, _ := s.OpenRun(ctx); open == nil || open.ID != manual.ID || open.ToASN != 0 {
		t.Fatalf("manual run without an ASN: %+v", open)
	}
}

func TestSettings(t *testing.T) {
	ctx, s := context.Background(), open(t)
	var v struct{ N int }
	if ok, err := s.GetSetting(ctx, "x", &v); ok || err != nil {
		t.Fatalf("unset setting: %v, %v", ok, err)
	}
	if on, err := s.Flag(ctx, SettingPaused); on || err != nil {
		t.Fatalf("unset flag: %v, %v", on, err)
	}
	s.SetSetting(ctx, "x", struct{ N int }{1})
	s.SetSetting(ctx, "x", struct{ N int }{2})
	if ok, err := s.GetSetting(ctx, "x", &v); !ok || err != nil || v.N != 2 {
		t.Fatalf("GetSetting = %+v, %v, %v", v, ok, err)
	}
	s.SetSetting(ctx, SettingPaused, true)
	if on, _ := s.Flag(ctx, SettingPaused); !on {
		t.Fatal("flag not set")
	}
	if err := s.SetSetting(ctx, "bad", func() {}); err == nil {
		t.Fatal("unencodable value should fail")
	}
	s.SetSetting(ctx, "str", "text")
	if _, err := s.GetSetting(ctx, "str", &v); err == nil {
		t.Fatal("decoding into the wrong type should fail")
	}
}

func TestEventsFilterAndPage(t *testing.T) {
	ctx, s := context.Background(), open(t)
	for i := range 5 {
		typ := "failover"
		if i%2 == 1 {
			typ = "check"
		}
		if err := s.AddEvent(ctx, t0.Add(time.Duration(i)*time.Minute), typ, map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddEvent(ctx, t0, "bad", func() {}); err == nil {
		t.Fatal("unencodable payload should fail")
	}

	all, err := s.Events(ctx, EventFilter{})
	if err != nil || len(all) != 5 || all[0].Payload["i"] != float64(4) || !all[0].TS.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("Events = %+v, %v", all, err)
	}
	fo, _ := s.Events(ctx, EventFilter{Type: "failover", Limit: 2})
	if len(fo) != 2 || fo[0].Type != "failover" || fo[0].Payload["i"] != float64(4) {
		t.Fatalf("filtered = %+v", fo)
	}
	older, _ := s.Events(ctx, EventFilter{Type: "failover", BeforeID: fo[1].ID})
	if len(older) != 1 || older[0].Payload["i"] != float64(0) {
		t.Fatalf("next page = %+v", older)
	}
	types, err := s.EventTypes(ctx)
	if err != nil || !slices.Equal(types, []string{"check", "failover"}) {
		t.Fatalf("EventTypes = %v, %v", types, err)
	}
}

func TestChecksTimelineAndPrune(t *testing.T) {
	ctx, s := context.Background(), open(t)
	isp, _, _ := s.SeeASN(ctx, 64500, "EXAMPLENET-64500", t0)
	s.Observe(ctx, 64500, 60, t0.Add(-40*24*time.Hour))
	checks := []Check{
		{TS: t0.Add(-2 * time.Hour), IP: "203.0.113.3", ASN: 64500, Conclusive: true, Providers: []string{"a"}},
		{TS: t0, IP: "203.0.113.3", ASN: 64500, Conclusive: true},
		{TS: t0.Add(time.Minute)},
	}
	for _, c := range checks {
		if err := s.RecordCheck(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordCheck(ctx, Check{TS: t0, Providers: func() {}}); err == nil {
		t.Fatal("unencodable providers should fail")
	}
	pts, err := s.ChecksSince(ctx, t0.Add(-time.Hour))
	want := []CheckPoint{{TS: t0, ISP: isp, Conclusive: true}, {TS: t0.Add(time.Minute)}}
	if err != nil || !slices.Equal(pts, want) {
		t.Fatalf("ChecksSince = %+v, %v", pts, err)
	}

	n, err := s.Prune(ctx, t0.Add(-time.Hour), t0.Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	if shares, _ := s.Shares(ctx, t0.Add(-60*24*time.Hour)); len(shares) != 0 {
		t.Fatalf("old daily totals not pruned: %+v", shares)
	}
}

func TestRenameAndClearPrimary(t *testing.T) {
	ctx, s := context.Background(), open(t)
	id, _, _ := s.SeeASN(ctx, 64500, "EXAMPLENET-64500", t0)
	if err := s.RenameISP(ctx, id, "  Home cable  "); err != nil {
		t.Fatal(err)
	}
	if names, err := s.ISPNames(ctx); err != nil || names[id] != "Home cable" {
		t.Fatalf("ISPNames = %v, %v", names, err)
	}
	s.RenameISP(ctx, id, "")
	if names, _ := s.ISPNames(ctx); names[id] != "EXAMPLENET" {
		t.Fatalf("empty name should reset to the group name: %v", names)
	}
	if err := s.RenameISP(ctx, 999, "x"); err == nil {
		t.Fatal("renaming an unknown ISP should fail")
	}

	s.SetRole(ctx, id, RolePrimary, true)
	if err := s.ClearPrimary(ctx); err != nil {
		t.Fatal(err)
	}
	if p, pinned, _ := s.Primary(ctx); p != 0 || pinned {
		t.Fatalf("primary after clear: %d pinned=%v", p, pinned)
	}
}

func TestUsersListLoginAndSessions(t *testing.T) {
	ctx, s := context.Background(), open(t)
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("fresh store has %d users", n)
	}
	admin, _ := s.CreateUser(ctx, "zed", "test-password-123", RoleAdmin, t0)
	viewer, _ := s.CreateUser(ctx, "amy", "test-password-123", RoleViewer, t0)
	if n, _ := s.CountUsers(ctx); n != 2 {
		t.Fatalf("CountUsers = %d", n)
	}
	users, err := s.Users(ctx)
	if err != nil || len(users) != 2 || users[0].Username != "amy" || !users[1].IsAdmin() {
		t.Fatalf("Users = %+v, %v", users, err)
	}

	if err := s.TouchLogin(ctx, viewer.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, viewer.ID); !u.LastLogin.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last login = %s", u.LastLogin)
	}

	if err := s.SetUserRole(ctx, admin.ID, RoleViewer); err == nil {
		t.Fatal("demoted the last admin")
	}
	if err := s.SetUserRole(ctx, viewer.ID, "owner"); err == nil {
		t.Fatal("accepted an unknown role")
	}
	if err := s.SetUserRole(ctx, viewer.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, admin.ID, RoleViewer); err != nil {
		t.Fatalf("demoting one of two admins: %v", err)
	}

	long := string(make([]byte, 400))
	tok, err := s.CreateSession(ctx, viewer.ID, "192.0.2.1", long, t0)
	if err != nil {
		t.Fatal(err)
	}
	expired, _ := s.CreateSession(ctx, viewer.ID, "192.0.2.1", "ua", t0.Add(-2*SessionTTL))
	if err := s.PruneSessions(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionByToken(ctx, expired, t0.Add(-2*SessionTTL)); ok {
		t.Fatal("expired session survived pruning")
	}
	if _, ok, _ := s.SessionByToken(ctx, tok, t0.Add(2*time.Hour)); !ok {
		t.Fatal("live session lost")
	}
	if err := s.DeleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionByToken(ctx, tok, t0); ok {
		t.Fatal("deleted session still valid")
	}
}

func TestStackStatus(t *testing.T) {
	for want, r := range map[string]StackRow{
		"disabled": {Disabled: true, Disabling: 3},
		"partial":  {Disabling: 1},
		"none":     {},
	} {
		if got := r.Status(); got != want {
			t.Errorf("%+v.Status() = %s, want %s", r, got, want)
		}
	}
}

func TestOpenFailsOnBadPath(t *testing.T) {
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing", "x.db")); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

// Every query reports the database error instead of panicking or hiding it.
func TestClosedStoreReturnsErrors(t *testing.T) {
	ctx, s := context.Background(), open(t)
	s.Close()
	var v any
	calls := map[string]func() error{
		"LoadState":     func() error { _, err := s.LoadState(ctx, t0); return err },
		"SaveState":     func() error { return s.SaveState(ctx, WANState{}) },
		"RecordCheck":   func() error { return s.RecordCheck(ctx, Check{}) },
		"AddEvent":      func() error { return s.AddEvent(ctx, t0, "x", nil) },
		"Prune":         func() error { _, err := s.Prune(ctx, t0, t0); return err },
		"Events":        func() error { _, err := s.Events(ctx, EventFilter{}); return err },
		"EventTypes":    func() error { _, err := s.EventTypes(ctx); return err },
		"ChecksSince":   func() error { _, err := s.ChecksSince(ctx, t0); return err },
		"GetSetting":    func() error { _, err := s.GetSetting(ctx, "x", &v); return err },
		"SetSetting":    func() error { return s.SetSetting(ctx, "x", 1) },
		"OpenRun":       func() error { _, err := s.OpenRun(ctx); return err },
		"CreateRun":     func() error { _, err := s.CreateRun(ctx, "auto", 1, t0); return err },
		"AddRunAction":  func() error { return s.AddRunAction(ctx, 1, RunAction{}) },
		"RunActions":    func() error { _, err := s.RunActions(ctx, 1); return err },
		"MarkStopped":   func() error { return s.MarkStopped(ctx, 1, "c", "", t0) },
		"MarkStarted":   func() error { return s.MarkStarted(ctx, 1, "c", "", t0) },
		"CloseRun":      func() error { return s.CloseRun(ctx, 1, "completed", t0) },
		"SeeASN":        func() error { _, _, err := s.SeeASN(ctx, 1, "X", t0); return err },
		"ISPOfASN":      func() error { _, err := s.ISPOfASN(ctx, 1); return err },
		"Observe":       func() error { return s.Observe(ctx, 1, 1, t0) },
		"Shares":        func() error { _, err := s.Shares(ctx, t0); return err },
		"Primary":       func() error { _, _, err := s.Primary(ctx); return err },
		"SetRole":       func() error { return s.SetRole(ctx, 1, RolePrimary, true) },
		"MoveASN":       func() error { _, err := s.MoveASN(ctx, 1, 0, t0); return err },
		"ISPs":          func() error { _, err := s.ISPs(ctx, t0); return err },
		"RenameISP":     func() error { return s.RenameISP(ctx, 1, "x") },
		"ISPNames":      func() error { _, err := s.ISPNames(ctx); return err },
		"ClearPrimary":  func() error { return s.ClearPrimary(ctx) },
		"Containers":    func() error { _, err := s.Containers(ctx); return err },
		"Stacks":        func() error { _, err := s.Stacks(ctx); return err },
		"SetStack":      func() error { return s.SetStackPolicy(ctx, "p", true, "x", t0) },
		"CountUsers":    func() error { _, err := s.CountUsers(ctx); return err },
		"CreateUser":    func() error { _, err := s.CreateUser(ctx, "u", "long enough password", RoleAdmin, t0); return err },
		"Authenticate":  func() error { _, err := s.Authenticate(ctx, "u", "p", t0); return err },
		"Users":         func() error { _, err := s.Users(ctx); return err },
		"SetPassword":   func() error { return s.SetPassword(ctx, 1, "long enough password") },
		"SetDisabled":   func() error { return s.SetUserDisabled(ctx, 1, true) },
		"CreateSession": func() error { _, err := s.CreateSession(ctx, 1, "", "", t0); return err },
		"Session":       func() error { _, _, err := s.SessionByToken(ctx, "t", t0); return err },
		"DeleteSession": func() error { return s.DeleteSession(ctx, "t") },
		"PruneSessions": func() error { return s.PruneSessions(ctx, t0) },
		"UserByIdent":   func() error { _, _, err := s.UserByIdentity(ctx, "i", "s"); return err },
		"LinkIdentity":  func() error { return s.LinkIdentity(ctx, 1, "i", "s", "", t0) },
		"Unlink":        func() error { return s.UnlinkIdentities(ctx, 1) },
		"Identities":    func() error { _, err := s.Identities(ctx, 1); return err },
		"CreateSSOUser": func() error { _, err := s.CreateSSOUser(ctx, "u", RoleViewer, "i", "s", "", t0); return err },
		"SetUserRole":   func() error { return s.SetUserRole(ctx, 1, RoleViewer) },
		"TouchLogin":    func() error { return s.TouchLogin(ctx, 1, t0) },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s on a closed store returned no error", name)
		}
	}
}
