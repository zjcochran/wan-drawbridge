// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// reportPeriod sums the outages that overlap the last Days days.
type reportPeriod struct {
	Label   string
	Count   int
	Total   time.Duration // time on the backup within the period
	Longest time.Duration // the longest outage, in full
	Bytes   int64         // traffic of those outages, in full
}

// reportMonth sums the outages that overlap a calendar month (local time).
type reportMonth struct {
	Start time.Time
	Count int
	Total time.Duration // time on the backup within the month
	Bytes int64         // traffic of the outages that started in the month
	Bar   float64       // Total as a percentage of the busiest month's
}

type reportsData struct {
	Now     time.Time
	Periods []reportPeriod
	Months  []reportMonth // the last 12, newest first
	Top     []store.Usage // containers by traffic, last 30 days
	Outages []store.Outage
	Next    int64 // BeforeID for the next page, 0 if none
}

const (
	outagesPageSize = 50
	topContainers   = 10
)

func (s *Server) reportsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now()
	d := reportsData{Now: now}
	year := monthStart(now).AddDate(0, -11, 0)
	recent, err := s.Store.Outages(ctx, store.OutageFilter{Since: year.Add(-24 * time.Hour)})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, p := range []struct {
		label string
		days  int
	}{{"Last 7 days", 7}, {"Last 30 days", 30}, {"Last 12 months", 365}} {
		d.Periods = append(d.Periods, period(p.label, recent, now.AddDate(0, 0, -p.days), now))
	}
	d.Months = months(recent, year, now)
	if d.Top, err = s.Store.UsageSince(ctx, now.AddDate(0, 0, -30)); err != nil {
		s.fail(w, r, err)
		return
	}
	d.Top = d.Top[:min(len(d.Top), topContainers)]
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if d.Outages, err = s.Store.Outages(ctx, store.OutageFilter{BeforeID: before, Limit: outagesPageSize}); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(d.Outages) == outagesPageSize {
		d.Next = d.Outages[len(d.Outages)-1].ID
	}
	s.page(w, r, "reports", "Reports", d)
}

// overlap is how much of outage o falls between from and to.
func overlap(o store.Outage, from, to time.Time) time.Duration {
	start, end := o.Started, o.Ended
	if o.Open() {
		end = to
	}
	start, end = later(start, from), earlier(end, to)
	return max(end.Sub(start), 0)
}

// touches reports whether outage o lasted at some point between from and to.
func touches(o store.Outage, from, to time.Time) bool {
	return o.Started.Before(to) && (o.Open() || !o.Ended.Before(from))
}

func period(label string, os []store.Outage, from, now time.Time) reportPeriod {
	p := reportPeriod{Label: label}
	for _, o := range os {
		if !touches(o, from, now) {
			continue
		}
		p.Count++
		p.Total += overlap(o, from, now)
		p.Longest = max(p.Longest, o.Duration(now))
		p.Bytes += o.RX + o.TX
	}
	return p
}

func months(os []store.Outage, first, now time.Time) []reportMonth {
	var out []reportMonth
	var busiest time.Duration
	for m := first; !m.After(now); m = m.AddDate(0, 1, 0) {
		rm := reportMonth{Start: m}
		end := earlier(m.AddDate(0, 1, 0), now)
		for _, o := range os {
			if touches(o, m, end) {
				rm.Count++
				rm.Total += overlap(o, m, end)
			}
			if !o.Started.Before(m) && o.Started.Before(end) {
				rm.Bytes += o.RX + o.TX
			}
		}
		busiest = max(busiest, rm.Total)
		out = append([]reportMonth{rm}, out...)
	}
	for i := range out {
		if busiest > 0 {
			out[i].Bar = 100 * float64(out[i].Total) / float64(busiest)
		}
	}
	return out
}

type outageData struct {
	Now    time.Time
	Outage store.Outage
	Usage  []store.Usage
}

func (s *Server) outagePage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	o, ok, err := s.Store.OutageByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	d := outageData{Now: s.now(), Outage: o}
	if d.Usage, err = s.Store.OutageUsage(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.page(w, r, "outage", "Outage", d)
}

func monthStart(t time.Time) time.Time {
	t = t.Local()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.Local)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
