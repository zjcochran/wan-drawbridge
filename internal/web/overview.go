// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/notify"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

// overview is the top of the dashboard: the date and greeting, and the stat
// tiles under the status card.
type overview struct {
	Date, Greeting, Summary string
	OnPrimary               string        // share of the last 30 days not on a backup, "99.91%"
	Backup                  time.Duration // time on a backup in the last 30 days
	Failovers               int           // outages that started in the last 30 days
	LastFailover            time.Time     // when the newest outage started, zero if none
	HostsUp, HostsAll       int
	HostNames               []string
	Stopped                 int // containers Drawbridge has stopped now
}

// failoverPlan is what a failover would stop now, by its settings.
type failoverPlan struct {
	Stop, Keep int
	Stacks     []stackCount // stacks with containers that stop, most first
}

type stackCount struct {
	Name string
	N    int
}

func greeting(now time.Time) string {
	switch h := now.Local().Hour(); {
	case h < 5 || h >= 18:
		return "Good evening"
	case h < 12:
		return "Good morning"
	}
	return "Good afternoon"
}

// stateSummary is one sentence on where the WAN stands.
func stateSummary(st statusData) string {
	switch st.S.State {
	case wan.Primary:
		if st.Primary != "" {
			return "All quiet on " + st.Primary + "."
		}
		return "All quiet."
	case wan.Failover:
		return fmt.Sprintf("On %s since %s.", orISP(st.ISPName), clock(st.S.Since))
	case wan.FailoverPending:
		return "Traffic is on " + orISP(st.ISPName) + ": confirming before anything stops."
	case wan.FailbackPending:
		return "The primary is back: making sure it stays."
	}
	return "Still learning which ISP is the primary."
}

func orISP(name string) string {
	if name == "" {
		return "an unknown ISP"
	}
	return name
}

func (s *Server) overview(ctx context.Context, user string, st statusData) (overview, error) {
	now := s.now()
	o := overview{
		Date:     now.Local().Format("Monday, January 2"),
		Greeting: greeting(now) + ", " + user + ".",
		Summary:  stateSummary(st),
		Stopped:  st.Stopped,
	}
	from := now.AddDate(0, 0, -30)
	outages, err := s.Store.Outages(ctx, store.OutageFilter{Since: from})
	if err != nil {
		return o, err
	}
	p := period("", outages, from, now)
	o.Backup = p.Total
	o.OnPrimary = fmt.Sprintf("%.2f%%", 100*(1-p.Total.Seconds()/now.Sub(from).Seconds()))
	for _, out := range outages {
		if !out.Started.Before(from) {
			o.Failovers++
		}
		if out.Started.After(o.LastFailover) {
			o.LastFailover = out.Started
		}
	}
	hosts, err := s.Store.Hosts(ctx)
	if err != nil {
		return o, err
	}
	for _, h := range hosts {
		if h.Status == store.HostDisabled {
			continue
		}
		o.HostsAll++
		o.HostNames = append(o.HostNames, h.Name)
		if h.Status == store.HostOnline || h.Kind == "local" {
			o.HostsUp++
		}
	}
	return o, nil
}

// plan counts what a failover would stop, from each container's setting.
func plan(rows []store.ContainerRow) failoverPlan {
	var p failoverPlan
	per := map[string]int{}
	for _, c := range rows {
		if c.Effective != store.Disable {
			p.Keep++
			continue
		}
		p.Stop++
		name := c.Project
		if name == "" {
			name = "standalone"
		}
		per[name]++
	}
	for name, n := range per {
		p.Stacks = append(p.Stacks, stackCount{name, n})
	}
	sort.Slice(p.Stacks, func(i, j int) bool {
		if p.Stacks[i].N != p.Stacks[j].N {
			return p.Stacks[i].N > p.Stacks[j].N
		}
		return p.Stacks[i].Name < p.Stacks[j].Name
	})
	return p
}

// eventLine is an event as a sentence: notifications' wording where there is
// one, else a short line made from its fields.
type eventLine struct {
	store.Event
	Title, Text string
}

func eventLines(events []store.Event, names map[int]string) []eventLine {
	out := make([]eventLine, 0, len(events))
	for _, e := range events {
		m := notify.Format(e, names)
		l := eventLine{Event: e, Title: m.Title, Text: m.Text}
		if l.Title == e.Type || l.Title == "" {
			l.Title, l.Text = plainEvent(e)
		}
		out = append(out, l)
	}
	return out
}

// plainEvent words the events notifications do not.
func plainEvent(e store.Event) (title, text string) {
	str := func(k string) string {
		if v, ok := e.Payload[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	by := str("by")
	switch e.Type {
	case "state_changed":
		return stateLabel(wan.State(str("to"))), "from " + strings.ToLower(stateLabel(wan.State(str("from")))) + ", on " + orISP(str("name"))
	case "stack_policy":
		what := "keep running"
		if e.Payload["disable"] == true {
			what = "stop"
		}
		return fmt.Sprintf("%s set the stack %s to %s on failover", by, str("stack"), what), ""
	case "container_policy":
		what := map[string]string{store.Disable: "stop on failover", store.Keep: "keep running", store.Inherit: "follow its stack"}[str("policy")]
		return fmt.Sprintf("%s set %s to %s", by, str("container"), what), ""
	case "settings_changed":
		return by + " changed the detection settings", ""
	case "user_created":
		return fmt.Sprintf("%s added the %s %s", by, str("role"), str("user")), ""
	}
	t := strings.ReplaceAll(e.Type, "_", " ")
	return strings.ToUpper(t[:1]) + t[1:], summary(e.Payload)
}

// timelineRange is a window the dashboard's timeline can show.
type timelineRange struct {
	ID     string
	Label  string // on its button
	Title  string // the card's heading
	Window time.Duration
	Ticks  int    // tick marks, evenly spaced, the last being "now"
	Format string // time layout of the other ticks
}

var timelineRanges = []timelineRange{
	{"24h", "24 h", "Last 24 hours", 24 * time.Hour, 4, "15:04"},
	{"7d", "7 d", "Last 7 days", 7 * 24 * time.Hour, 7, "Mon"},
	{"30d", "30 d", "Last 30 days", 30 * 24 * time.Hour, 5, "Jan 2"},
}

func rangeOf(id string) timelineRange {
	for _, r := range timelineRanges {
		if r.ID == id {
			return r
		}
	}
	return timelineRanges[0]
}
