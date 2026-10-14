// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/monitor"
)

// Settings rows for the scheduled failover test.
const (
	SettingTestSchedule = "test_schedule"      // TestSchedule as JSON
	settingTestLast     = "test_schedule_last" // testLast as JSON
)

// testWindow is how late a scheduled test may still start, for a Drawbridge
// that was busy or restarting at the time; a later start is skipped.
const testWindow = time.Hour

// testMinutes are the durations a test can last, as on the dashboard.
var testMinutes = []int{2, 5, 10, 15}

// TestSchedule runs a failover test every week or every month at Time
// (server local time). It is off unless enabled.
type TestSchedule struct {
	Enabled bool   `json:"enabled"`
	Every   string `json:"every"`   // "week" or "month"
	Weekday int    `json:"weekday"` // 0 (Sunday) to 6, when every week
	Day     int    `json:"day"`     // 1 to 28, when every month
	Time    string `json:"time"`    // "HH:MM"
	Minutes int    `json:"minutes"` // one of testMinutes
}

func defaultTestSchedule() TestSchedule {
	return TestSchedule{Every: "month", Day: 1, Time: "04:00", Minutes: 5}
}

// testLast is what happened at the latest scheduled time.
type testLast struct {
	Slot   time.Time `json:"slot"`   // the scheduled time it was for
	At     time.Time `json:"at"`     // when Drawbridge acted on it
	Result string    `json:"result"` // "started", or why not
}

// slot returns the latest scheduled time at or before now.
func (t TestSchedule) slot(now time.Time) time.Time {
	h, _ := strconv.Atoi(t.Time[:2])
	m, _ := strconv.Atoi(t.Time[3:])
	now = now.Local()
	if t.Every == "week" {
		s := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		for int(s.Weekday()) != t.Weekday || s.After(now) {
			s = s.AddDate(0, 0, -1)
		}
		return s
	}
	s := time.Date(now.Year(), now.Month(), t.Day, h, m, 0, 0, now.Location())
	if s.After(now) {
		s = s.AddDate(0, -1, 0)
	}
	return s
}

// next returns the first scheduled time after now.
func (t TestSchedule) next(now time.Time) time.Time {
	s := t.slot(now)
	if t.Every == "week" {
		return s.AddDate(0, 0, 7)
	}
	return s.AddDate(0, 1, 0)
}

func (s *Server) testSchedule(ctx context.Context) (TestSchedule, testLast, error) {
	t, last := defaultTestSchedule(), testLast{}
	if _, err := s.Store.GetSetting(ctx, SettingTestSchedule, &t); err != nil {
		return t, last, err
	}
	_, err := s.Store.GetSetting(ctx, settingTestLast, &last)
	return t, last, err
}

// RunTestSchedule starts scheduled failover tests until ctx is cancelled,
// checking every minute.
func (s *Server) RunTestSchedule(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.scheduledTestDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) scheduledTestDue(ctx context.Context) {
	t, last, err := s.testSchedule(ctx)
	if err != nil {
		s.Log.Error("cannot read the test schedule", "err", err)
		return
	}
	now := s.now()
	slot := t.slot(now)
	if !t.Enabled || !last.Slot.Before(slot) {
		return
	}
	last = testLast{Slot: slot, At: now}
	switch {
	case now.Sub(slot) > testWindow:
		last.Result = "missed: Drawbridge was not running at that time"
	case s.Mon.Status().Paused:
		last.Result = "skipped: automation is paused"
	default:
		err := s.Mon.StartDrill(ctx, time.Duration(t.Minutes)*time.Minute, "schedule")
		switch {
		case err == nil:
			last.Result = "started"
		case errors.Is(err, monitor.ErrDrill):
			last.Result = "skipped: Drawbridge was not on the primary ISP, or containers were stopped"
		default:
			last.Result = "skipped: " + err.Error()
		}
	}
	if last.Result != "started" {
		s.Log.Warn("scheduled failover test not run", "reason", last.Result)
		s.event(ctx, "drill_skipped", map[string]any{"reason": last.Result, "by": "schedule"})
	}
	if err := s.Store.SetSetting(ctx, settingTestLast, last); err != nil {
		s.Log.Error("cannot record the scheduled test", "err", err)
	}
}

// ---- handlers ----

type testScheduleView struct {
	TestSchedule
	Last      testLast
	Next      time.Time
	IsAdmin   bool
	Durations []int // the choices for Minutes
	Weekdays  []string
}

func (s *Server) testScheduleView(r *http.Request) (testScheduleView, error) {
	t, last, err := s.testSchedule(r.Context())
	v := testScheduleView{TestSchedule: t, Last: last, IsAdmin: sessionFrom(r).User.IsAdmin(), Durations: testMinutes}
	for d := time.Sunday; d <= time.Saturday; d++ {
		v.Weekdays = append(v.Weekdays, d.String())
	}
	if t.Enabled {
		v.Next = t.next(s.now())
	}
	return v, err
}

func (s *Server) saveTestSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t := TestSchedule{Enabled: r.FormValue("enabled") != "", Every: r.FormValue("every"), Time: r.FormValue("time")}
	var err1, err2, err3 error
	t.Weekday, err1 = strconv.Atoi(r.FormValue("weekday"))
	t.Day, err2 = strconv.Atoi(r.FormValue("day"))
	t.Minutes, err3 = strconv.Atoi(r.FormValue("minutes"))
	switch {
	case t.Every != "week" && t.Every != "month":
		invalid(w, "Choose every week or every month.")
		return
	case !scheduleTime.MatchString(t.Time):
		invalid(w, "Enter the time as HH:MM, for example 04:00.")
		return
	case err1 != nil || t.Weekday < 0 || t.Weekday > 6:
		invalid(w, "Choose a day of the week.")
		return
	case err2 != nil || t.Day < 1 || t.Day > 28:
		invalid(w, "Choose a day of the month from 1 to 28.")
		return
	case err3 != nil || !slices.Contains(testMinutes, t.Minutes):
		invalid(w, "Choose 2, 5, 10 or 15 minutes.")
		return
	}
	if err := s.Store.SetSetting(ctx, SettingTestSchedule, t); err != nil {
		s.fail(w, r, err)
		return
	}
	// Count from now: a time already past today does not start a test at once.
	_, last, err := s.testSchedule(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	last.Slot = t.slot(s.now())
	if err := s.Store.SetSetting(ctx, settingTestLast, last); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(ctx, "test_schedule", map[string]any{"enabled": t.Enabled, "every": t.Every, "weekday": t.Weekday,
		"day": t.Day, "time": t.Time, "minutes": t.Minutes, "by": actorName(r)})
	v, err := s.testScheduleView(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "settings", "test-schedule", v)
}
