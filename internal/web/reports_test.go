// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

func TestReportsPage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	isp, _, _ := e.st.SeeASN(ctx, 64510, "SKYLINK-64510", time.Now())
	if isp == 0 {
		t.Fatal("no ISP")
	}
	save := func(s wan.State, since time.Time) {
		t.Helper()
		if err := e.st.SaveState(ctx, store.WANState{Machine: wan.Machine{State: s}, Since: since, CurrentASN: 64510, LearningStartedAt: since}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now().Add(-3 * time.Hour)
	save(wan.Failover, start)
	id, _ := e.st.OpenOutage(ctx)
	e.st.RecordUsage(ctx, id, store.LocalHost, "media/downloader", "c1", 0, 0, start)
	e.st.RecordUsage(ctx, id, store.LocalHost, "media/downloader", "c1", 2_500_000_000, 500_000_000, start)
	save(wan.Primary, start.Add(90*time.Minute))
	save(wan.Failover, time.Now().Add(-time.Minute)) // ongoing

	// Viewers see reports too.
	if _, err := e.st.CreateUser(ctx, "viewer", "test-password-123", store.RoleViewer, time.Now()); err != nil {
		t.Fatal(err)
	}
	c := e.client()
	c.do("POST", "/login", url.Values{"username": {"viewer"}, "password": {"test-password-123"}})
	resp, page := c.get("/reports")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reports: %d", resp.StatusCode)
	}
	for _, want := range []string{"Last 7 days", "1h 30m", "3.0 GB", "media/downloader", "SKYLINK", "ongoing",
		`href="/reports/outages/` + strconv.FormatInt(id, 10) + `"`, time.Now().Format("Jan 2006")} {
		if !strings.Contains(page, want) {
			t.Errorf("reports page lacks %q", want)
		}
	}
	resp, page = c.get("/reports/outages/" + strconv.FormatInt(id, 10))
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "2.5 GB") || !strings.Contains(page, "500 MB") {
		t.Errorf("outage page: %d\n%s", resp.StatusCode, page)
	}
	if resp, _ := c.get("/reports/outages/999"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing outage: %d", resp.StatusCode)
	}
}

func TestReportsPaging(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	base := time.Now().AddDate(0, 0, -5)
	for i := range outagesPageSize + 1 {
		at := base.Add(time.Duration(i) * time.Minute)
		e.st.SaveState(ctx, store.WANState{Machine: wan.Machine{State: wan.Failover}, Since: at, LearningStartedAt: at})
		e.st.SaveState(ctx, store.WANState{Machine: wan.Machine{State: wan.Primary}, Since: at.Add(30 * time.Second), LearningStartedAt: at})
	}
	c := e.admin()
	_, page := c.get("/reports")
	if !strings.Contains(page, "/reports?before=") || !strings.Contains(page, "<dd>51</dd>") {
		t.Fatal("no Older link or wrong count")
	}
	_, page = c.get("/reports?before=2")
	if strings.Count(page, `href="/reports/outages/`) != 1 {
		t.Error("the second page should hold the oldest outage")
	}
}

func TestMonthsSplitAndBars(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.Local)
	os := []store.Outage{
		// Across the end of January: 2 h in January, 3 h in February.
		{Started: time.Date(2026, 1, 31, 22, 0, 0, 0, time.Local), Ended: time.Date(2026, 2, 1, 3, 0, 0, 0, time.Local), RX: 10, TX: 5},
		{Started: now.Add(-time.Hour)}, // ongoing
	}
	ms := months(os, monthStart(now).AddDate(0, -11, 0), now)
	if len(ms) != 12 || ms[0].Start.Month() != time.March || ms[11].Start.Month() != time.April {
		t.Fatalf("months = %d, first %v", len(ms), ms[0].Start)
	}
	mar, feb, jan := ms[0], ms[1], ms[2]
	if mar.Total != time.Hour || feb.Total != 3*time.Hour || jan.Total != 2*time.Hour {
		t.Errorf("totals = %v %v %v", mar.Total, feb.Total, jan.Total)
	}
	if jan.Bytes != 15 || feb.Bytes != 0 || jan.Count != 1 || feb.Count != 1 {
		t.Errorf("jan = %+v, feb = %+v", jan, feb)
	}
	if feb.Bar != 100 || jan.Bar < 66 || jan.Bar > 67 {
		t.Errorf("bars = %v %v", feb.Bar, jan.Bar)
	}
	p := period("p", os, now.AddDate(0, 0, -45), now)
	if p.Count != 2 || p.Total != 6*time.Hour || p.Longest != 5*time.Hour || p.Bytes != 15 {
		t.Errorf("period = %+v", p)
	}
}

func TestSpanAndBytes(t *testing.T) {
	spans := map[time.Duration]string{
		45 * time.Second: "45s", 12*time.Minute + 5*time.Second: "12m",
		3*time.Hour + 20*time.Minute: "3h 20m", 52 * time.Hour: "2d 4h",
	}
	for d, want := range spans {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
	sizes := map[int64]string{980: "980 B", 1_200_000: "1.2 MB", 345_000_000_000: "345 GB", 2_000_000_000_000_000_000: "2000 PB"}
	for n, want := range sizes {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
