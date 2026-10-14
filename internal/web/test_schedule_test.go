// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

func TestTestScheduleSlots(t *testing.T) {
	// Wednesday 2026-10-14 10:00 local time.
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.Local)
	for _, c := range []struct {
		t          TestSchedule
		slot, next time.Time
	}{
		{TestSchedule{Every: "month", Day: 1, Time: "04:00"}, time.Date(2026, 10, 1, 4, 0, 0, 0, time.Local), time.Date(2026, 11, 1, 4, 0, 0, 0, time.Local)},
		{TestSchedule{Every: "month", Day: 20, Time: "04:00"}, time.Date(2026, 9, 20, 4, 0, 0, 0, time.Local), time.Date(2026, 10, 20, 4, 0, 0, 0, time.Local)},
		{TestSchedule{Every: "month", Day: 14, Time: "11:00"}, time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local), time.Date(2026, 10, 14, 11, 0, 0, 0, time.Local)},
		{TestSchedule{Every: "week", Weekday: 3, Time: "09:30"}, time.Date(2026, 10, 14, 9, 30, 0, 0, time.Local), time.Date(2026, 10, 21, 9, 30, 0, 0, time.Local)},
		{TestSchedule{Every: "week", Weekday: 3, Time: "10:30"}, time.Date(2026, 10, 7, 10, 30, 0, 0, time.Local), time.Date(2026, 10, 14, 10, 30, 0, 0, time.Local)},
		{TestSchedule{Every: "week", Weekday: 0, Time: "04:00"}, time.Date(2026, 10, 11, 4, 0, 0, 0, time.Local), time.Date(2026, 10, 18, 4, 0, 0, 0, time.Local)},
	} {
		if got := c.t.slot(now); !got.Equal(c.slot) {
			t.Errorf("%+v: slot %v, want %v", c.t, got, c.slot)
		}
		if got := c.t.next(now); !got.Equal(c.next) {
			t.Errorf("%+v: next %v, want %v", c.t, got, c.next)
		}
	}
}

func TestScheduledTests(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	clock := time.Date(2026, 10, 14, 10, 0, 0, 0, time.Local) // a Wednesday
	e.ui.now = func() time.Time { return clock }

	form := url.Values{"enabled": {"on"}, "every": {"week"}, "weekday": {"3"}, "day": {"1"}, "time": {"10:30"}, "minutes": {"10"}}
	if resp, body := c.htmx("/settings/test-schedule", form); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Next test: <time") {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	// Saving at 10:00 counts from last week's 10:30: nothing runs now.
	e.ui.scheduledTestDue(ctx)
	if e.mon.drill != 0 {
		t.Fatal("a test started on saving")
	}

	clock = clock.Add(40 * time.Minute) // 10:40, 10 minutes after the slot
	e.ui.scheduledTestDue(ctx)
	if e.mon.drill != 10*time.Minute {
		t.Fatalf("the test did not start: %v", e.mon.drill)
	}
	e.mon.drill = 0
	e.ui.scheduledTestDue(ctx) // the same slot: once only
	if e.mon.drill != 0 {
		t.Fatal("the test started twice")
	}
	if _, page := c.get("/settings/detection"); !strings.Contains(page, `<span class="ok-text">started</span>`) {
		t.Error("the last test is not shown")
	}

	skips := []struct {
		setup func()
		want  string
	}{
		{func() { e.mon.paused = true }, "automation is paused"},
		{func() { e.mon.paused, e.mon.failErr = false, monitor.ErrDrill }, "not on the primary ISP"},
		{func() { e.mon.failErr = errors.New("monitor busy") }, "monitor busy"},
		{func() { e.mon.failErr = nil; clock = clock.Add(2 * time.Hour) }, "not running at that time"},
	}
	for _, s := range skips {
		clock = clock.AddDate(0, 0, 7)
		s.setup()
		e.ui.scheduledTestDue(ctx)
		evs, _ := e.st.Events(ctx, store.EventFilter{Type: "drill_skipped", Limit: 1})
		if len(evs) == 0 || !strings.Contains(evs[0].Payload["reason"].(string), s.want) {
			t.Errorf("want a skip for %q, got %+v", s.want, evs)
		}
	}
}

func TestTestScheduleForms(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	good := url.Values{"every": {"month"}, "weekday": {"0"}, "day": {"1"}, "time": {"04:00"}, "minutes": {"5"}}
	edit := func(k, v string) url.Values {
		f := url.Values{}
		for kk, vv := range good {
			f[kk] = vv
		}
		f.Set(k, v)
		return f
	}
	for name, f := range map[string]url.Values{
		"every": edit("every", "day"), "time": edit("time", "4am"), "weekday": edit("weekday", "7"),
		"day": edit("day", "29"), "minutes": edit("minutes", "3"),
	} {
		if resp, _ := c.htmx("/settings/test-schedule", f); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("bad %s: %d", name, resp.StatusCode)
		}
	}
	// Off by default; viewers see it read-only.
	v := e.viewerClient()
	if _, page := v.get("/settings/detection"); !strings.Contains(page, "Scheduled failover test") || !strings.Contains(page, "Off.") ||
		strings.Contains(page, `hx-post="/settings/test-schedule"`) {
		t.Error("viewers should see the schedule, off and read-only")
	}
	if resp, _ := v.htmx("/settings/test-schedule", good); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer save: %d", resp.StatusCode)
	}
}
