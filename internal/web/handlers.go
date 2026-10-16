// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// liveTimeout is how long the UI waits for a host to list its containers.
const liveTimeout = 5 * time.Second

type timeDuration = time.Duration

func withSession(ctx context.Context, s *store.Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

func atoi(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// ---- live container state ----

// liveKey identifies a container across hosts.
type liveKey struct {
	host int64
	key  string
}

// liveState maps containers to "running" / "stopped", from every host that
// can be reached. down holds the hosts that cannot be, whose containers the
// UI shows as "unknown".
func (s *Server) liveState(ctx context.Context) (live map[liveKey]string, down map[int64]bool) {
	live, down = map[liveKey]string{}, map[int64]bool{}
	for _, l := range s.Hosts.List(ctx, liveTimeout) {
		if l.Err != nil {
			s.Log.Warn("cannot list containers", "host", l.Host.Name, "err", l.Err)
			down[l.Host.ID] = true
			continue
		}
		for _, w := range l.Workloads {
			st := "stopped"
			if w.Running {
				st = "running"
			}
			// Scaled services share a key; any running replica counts as running.
			k := liveKey{w.Host, w.Key()}
			if live[k] != "running" {
				live[k] = st
			}
		}
	}
	return live, down
}

// multiHost reports whether there is more than one host, so pages show
// which host each container is on.
func (s *Server) multiHost(ctx context.Context) (bool, []store.HostRow, error) {
	hs, err := s.Store.Hosts(ctx)
	return len(hs) > 1, hs, err
}

type containerRow struct {
	store.ContainerRow
	State  string // running, stopped or unknown
	Hidden bool   // matches a container filter
}

// Source explains where the effective policy comes from.
func (c containerRow) Source() string {
	switch {
	case c.NeedsReview:
		return "new, not configured"
	case c.Policy != store.Inherit:
		return "own setting"
	case c.Label != "":
		return "from label"
	case c.Project == "":
		return "default"
	}
	return "from stack"
}

func (s *Server) containerRows(ctx context.Context) ([]containerRow, error) {
	rows, err := s.Store.Containers(ctx)
	if err != nil {
		return nil, err
	}
	live, down := s.liveState(ctx)
	out := make([]containerRow, 0, len(rows))
	for _, r := range rows {
		st := "unknown"
		if _, ok := s.Hosts.Get(r.HostID); ok && !down[r.HostID] {
			if st = live[liveKey{r.HostID, r.Key}]; st == "" {
				continue // removed from its host; its row is deleted after store.GoneAfter
			}
		}
		out = append(out, containerRow{ContainerRow: r, State: st})
	}
	return out, nil
}

// ---- dashboard ----

type dashboardData struct {
	Status      statusData
	Timeline    timeline
	Events      []store.Event
	NeedsReview int
}

type statusData struct {
	S         monitor.Status
	ISPName   string
	Primary   string
	Run       *store.Run
	Actions   []store.RunAction
	Stopped   int
	Owed      map[string]int // per host: containers to start once it is back
	IsAdmin   bool
	NoPrimary bool
	MultiHost bool
}

func (s *Server) statusData(r *http.Request) (statusData, error) {
	ctx := r.Context()
	st := s.Mon.Status()
	names, err := s.Store.ISPNames(ctx)
	if err != nil {
		return statusData{}, err
	}
	d := statusData{
		S: st, ISPName: names[st.ISP], Primary: names[st.PrimaryISP],
		IsAdmin: sessionFrom(r).User.IsAdmin(), NoPrimary: st.PrimaryISP == 0,
	}
	if d.MultiHost, _, err = s.multiHost(ctx); err != nil {
		return d, err
	}
	if d.Run, err = s.Store.OpenRun(ctx); err != nil {
		return d, err
	}
	owed, err := s.Store.PendingStarts(ctx)
	if err != nil {
		return d, err
	}
	for _, p := range owed {
		if d.Owed == nil {
			d.Owed = map[string]int{}
		}
		d.Owed[p.Host]++
	}
	if d.Run != nil {
		if d.Actions, err = s.Store.RunActions(ctx, d.Run.ID); err != nil {
			return d, err
		}
		for _, a := range d.Actions {
			if a.Stopped && !a.Started {
				d.Stopped++
			}
		}
	}
	return d, nil
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if setup, _ := s.Store.Flag(ctx, store.SettingSetupComplete); !setup && sessionFrom(r).User.IsAdmin() {
		redirect(w, r, "/setup?step=2")
		return
	}
	var d dashboardData
	var err error
	if d.Status, err = s.statusData(r); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Timeline, err = s.timeline(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Events, err = s.Store.Events(ctx, store.EventFilter{Limit: 8}); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Store.Containers(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, c := range rows {
		if c.NeedsReview {
			d.NeedsReview++
		}
	}
	s.page(w, r, "dashboard", "Dashboard", d)
}

func (s *Server) statusPartial(w http.ResponseWriter, r *http.Request) {
	d, err := s.statusData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "dashboard", "status", d)
}

func (s *Server) timelinePartial(w http.ResponseWriter, r *http.Request) {
	t, err := s.timeline(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "dashboard", "timeline", t)
}

// timeline is the last 24 hours of checks as coloured segments.
type timeline struct {
	Segments []segment
	Legend   []legendItem
	Ticks    []tick
}

type segment struct {
	X, W  float64 // in a 0..1000 wide viewBox
	Class string
	Title string
}

type legendItem struct{ Class, Label string }

type tick struct {
	X     float64 // percent from the left
	Label string
}

func (s *Server) timeline(ctx context.Context) (timeline, error) {
	end := s.now()
	start := end.Add(-24 * time.Hour)
	pts, err := s.Store.ChecksSince(ctx, start)
	if err != nil {
		return timeline{}, err
	}
	names, err := s.Store.ISPNames(ctx)
	if err != nil {
		return timeline{}, err
	}
	primary := s.Mon.Status().PrimaryISP
	d, err := s.Mon.Detection(ctx)
	if err != nil {
		return timeline{}, err
	}
	gap := 3 * d.CheckInterval // longer than this between checks = no data

	x := func(t time.Time) float64 { return 1000 * t.Sub(start).Seconds() / (24 * time.Hour).Seconds() }
	class := func(p store.CheckPoint) string {
		switch {
		case !p.Conclusive || p.ISP == 0:
			return "inconclusive"
		case p.ISP == primary:
			return "primary"
		}
		return "backup"
	}
	label := func(p store.CheckPoint) string {
		if !p.Conclusive || p.ISP == 0 {
			return "Inconclusive"
		}
		return names[p.ISP]
	}

	var t timeline
	for i := 0; i < len(pts); {
		j := i
		for j+1 < len(pts) && class(pts[j+1]) == class(pts[i]) && pts[j+1].ISP == pts[i].ISP &&
			pts[j+1].TS.Sub(pts[j].TS) <= gap {
			j++
		}
		segEnd := pts[j].TS.Add(d.CheckInterval)
		if j+1 < len(pts) && pts[j+1].TS.Sub(pts[j].TS) <= gap {
			segEnd = pts[j+1].TS
		}
		segEnd = minTime(segEnd, end)
		t.Segments = append(t.Segments, segment{
			X: x(pts[i].TS), W: max(x(segEnd)-x(pts[i].TS), 0.5), Class: class(pts[i]),
			Title: fmt.Sprintf("%s · %s – %s", label(pts[i]), pts[i].TS.Local().Format("15:04"), segEnd.Local().Format("15:04")),
		})
		i = j + 1
	}
	if primary != 0 {
		t.Legend = append(t.Legend, legendItem{"primary", "Primary: " + names[primary]})
	}
	t.Legend = append(t.Legend, legendItem{"backup", "Backup"}, legendItem{"inconclusive", "Inconclusive"})
	for h := 0; h <= 24; h += 6 {
		at := start.Add(time.Duration(h) * time.Hour)
		lbl := at.Local().Format("15:04")
		if h == 24 {
			lbl = "now"
		}
		t.Ticks = append(t.Ticks, tick{X: x(at) / 10, Label: lbl}) // percent
	}
	return t, nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// pauseDurations are the choices next to Pause automation; "" pauses until
// resumed by hand.
var pauseDurations = map[string]time.Duration{
	"": 0, "30m": 30 * time.Minute, "1h": time.Hour, "4h": 4 * time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour,
}

// drillDurations are the choices next to Test failover.
var drillDurations = map[string]time.Duration{
	"2m": 2 * time.Minute, "5m": 5 * time.Minute, "10m": 10 * time.Minute, "15m": 15 * time.Minute,
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	ctx, who := r.Context(), actorName(r)
	var err error
	switch r.PathValue("action") {
	case "pause":
		d, ok := pauseDurations[r.FormValue("for")]
		switch {
		case !ok:
			http.Error(w, "unknown pause duration", http.StatusBadRequest)
			return
		case d == 0:
			err = s.Mon.SetPaused(ctx, true, who)
		default:
			err = s.Mon.PauseFor(ctx, d, who)
		}
	case "resume":
		err = s.Mon.SetPaused(ctx, false, who)
	case "failover":
		err = s.Mon.FailoverNow(ctx, who)
	case "drill":
		d, ok := drillDurations[r.FormValue("for")]
		if !ok {
			http.Error(w, "unknown test duration", http.StatusBadRequest)
			return
		}
		if err = s.Mon.StartDrill(ctx, d, who); errors.Is(err, monitor.ErrDrill) {
			invalid(w, "Cannot test now: "+err.Error()+".")
			return
		}
	case "drill-end":
		err = s.Mon.EndDrill(ctx, who)
	case "restore":
		err = s.Mon.RestoreNow(ctx, who)
	case "check":
		err = s.Mon.CheckNow(ctx)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.statusPartial(w, r)
}

// ---- stacks ----

type stackRow struct {
	store.StackRow
	Running int
	Down    bool // its host cannot be reached
}

func (s *Server) stacksData(r *http.Request, host string) (stacksData, error) {
	ctx := r.Context()
	d := stacksData{Host: host, IsAdmin: sessionFrom(r).User.IsAdmin()}
	var err error
	if d.MultiHost, d.Hosts, err = s.multiHost(ctx); err != nil {
		return d, err
	}
	stacks, err := s.Store.Stacks(ctx)
	if err != nil {
		return d, err
	}
	cs, err := s.containerRows(ctx)
	if err != nil {
		return d, err
	}
	for _, st := range stacks {
		if host != "" && strconv.FormatInt(st.HostID, 10) != host {
			continue
		}
		row := stackRow{StackRow: st}
		for _, c := range cs {
			if c.HostID == st.HostID && c.Project == st.Project {
				switch c.State {
				case "running":
					row.Running++
				case "unknown":
					row.Down = true
				}
			}
		}
		d.Stacks = append(d.Stacks, row)
	}
	return d, nil
}

type stacksData struct {
	Stacks    []stackRow
	Hosts     []store.HostRow
	Host      string // the host filter: "" for all, else a host ID
	MultiHost bool
	IsAdmin   bool
}

func (s *Server) stacksPage(w http.ResponseWriter, r *http.Request) {
	d, err := s.stacksData(r, r.URL.Query().Get("host"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if isHTMX(r) && r.URL.Query().Get("partial") == "1" {
		s.partial(w, r, "stacks", "stacks-table", d)
		return
	}
	s.page(w, r, "stacks", "Stacks", d)
}

// pathHost reads the {host} route value.
func pathHost(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("host"), 10, 64)
	return id, err == nil
}

func (s *Server) stackContainers(w http.ResponseWriter, r *http.Request) {
	host, ok := pathHost(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cs, err := s.containerRows(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project := r.PathValue("project")
	var rows []containerRow
	for _, c := range cs {
		if c.HostID == host && c.Project == project {
			rows = append(rows, c)
		}
	}
	s.partial(w, r, "stacks", "stack-containers", rows)
}

func (s *Server) setStackPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	host, ok := pathHost(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	project := r.PathValue("project")
	on := r.PostFormValue("disable") == "on"
	if err := s.Store.SetStackPolicy(ctx, host, project, on, actorName(r), s.now()); err != nil {
		invalid(w, err.Error())
		return
	}
	names, err := s.Store.HostNames(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(ctx, "stack_policy", map[string]any{"host": names[host], "stack": project, "disable": on, "by": actorName(r)})
	s.Mon.PoliciesChanged()
	s.Hub.Publish(TopicInventory)
	d, err := s.stacksData(r, r.PostFormValue("host"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "stacks", "stacks-table", d)
}

// ---- containers ----

type containersData struct {
	Rows      []containerRow
	Stacks    []string
	Hosts     []store.HostRow
	MultiHost bool
	Filter    containerFilter
	IsAdmin   bool
	Total     int
	Hidden    int             // rows left out because they match a container filter
	HasRules  bool            // container filters are set
	disabled  map[liveKey]int // per host and stack: containers following a disabled stack
}

// DisabledIn counts the containers following a disabled stack.
func (d containersData) DisabledIn(host int64, project string) int {
	return d.disabled[liveKey{host, project}]
}

type containerFilter struct {
	Host, Stack, State, Policy, Q string
	ShowHidden                    bool // include containers matching a container filter
}

func (f containerFilter) match(c containerRow) bool {
	switch {
	case f.Host != "" && strconv.FormatInt(c.HostID, 10) != f.Host,
		f.Stack == "-" && c.Project != "",
		f.Stack != "" && f.Stack != "-" && c.Project != f.Stack,
		f.State != "" && c.State != f.State,
		f.Policy == "review" && !c.NeedsReview,
		(f.Policy == store.Disable || f.Policy == store.Keep) && c.Effective != f.Policy,
		f.Q != "" && !strings.Contains(strings.ToLower(c.Key+" "+c.Name), strings.ToLower(f.Q)):
		return false
	}
	return true
}

func (s *Server) containersData(r *http.Request, f containerFilter) (containersData, error) {
	cs, err := s.containerRows(r.Context())
	if err != nil {
		return containersData{}, err
	}
	d := containersData{Filter: f, IsAdmin: sessionFrom(r).User.IsAdmin(), Total: len(cs), disabled: map[liveKey]int{}}
	if d.MultiHost, d.Hosts, err = s.multiHost(r.Context()); err != nil {
		return d, err
	}
	rules, err := s.hideRules(r.Context())
	if err != nil {
		return d, err
	}
	d.HasRules = len(rules) > 0
	seen := map[string]bool{}
	for _, c := range cs {
		inHost := f.Host == "" || strconv.FormatInt(c.HostID, 10) == f.Host
		if c.Project != "" && !seen[c.Project] && inHost {
			seen[c.Project] = true
			d.Stacks = append(d.Stacks, c.Project)
		}
		if c.StackDisabled && c.Policy == store.Inherit {
			d.disabled[liveKey{c.HostID, c.Project}]++
		}
		if !f.match(c) {
			continue
		}
		if c.Hidden = rules.hides(c); c.Hidden && !f.ShowHidden {
			d.Hidden++
			continue
		}
		d.Rows = append(d.Rows, c)
	}
	sort.Strings(d.Stacks)
	return d, nil
}

func filterFrom(get func(string) string) containerFilter {
	return containerFilter{Host: get("host"), Stack: get("stack"), State: get("state"), Policy: get("policy"), Q: strings.TrimSpace(get("q")), ShowHidden: get("hidden") == "1"}
}

func (s *Server) containersPage(w http.ResponseWriter, r *http.Request) {
	d, err := s.containersData(r, filterFrom(r.URL.Query().Get))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if isHTMX(r) && r.URL.Query().Get("partial") == "1" {
		s.partial(w, r, "containers", "containers-table", d)
		return
	}
	s.page(w, r, "containers", "Containers", d)
}

func (s *Server) setContainerPolicy(w http.ResponseWriter, r *http.Request) {
	host, err := strconv.ParseInt(r.PostFormValue("container_host"), 10, 64)
	if err != nil {
		host = store.LocalHost
	}
	s.applyPolicies(w, r, []liveKey{{host, r.PostFormValue("key")}}, r.PostFormValue("policy"))
}

// parseKeys reads bulk selections, each "<host ID>:<container key>".
func parseKeys(vals []string) ([]liveKey, bool) {
	out := make([]liveKey, 0, len(vals))
	for _, v := range vals {
		h, k, ok := strings.Cut(v, ":")
		id, err := strconv.ParseInt(h, 10, 64)
		if !ok || err != nil || k == "" {
			return nil, false
		}
		out = append(out, liveKey{id, k})
	}
	return out, true
}

func (s *Server) bulkContainerPolicy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		invalid(w, "Could not read the form.")
		return
	}
	keys, ok := parseKeys(r.PostForm["keys"])
	if !ok {
		invalid(w, "Could not read the selection.")
		return
	}
	if len(keys) == 0 {
		invalid(w, "Select at least one container.")
		return
	}
	s.applyPolicies(w, r, keys, r.PostFormValue("bulk_policy"))
}

func (s *Server) applyPolicies(w http.ResponseWriter, r *http.Request, keys []liveKey, policy string) {
	ctx, now, who := r.Context(), s.now(), actorName(r)
	names, err := s.Store.HostNames(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, k := range keys {
		converted, err := s.Store.SetContainerPolicy(ctx, k.host, k.key, policy, who, now)
		if err != nil {
			invalid(w, err.Error())
			return
		}
		s.event(ctx, "container_policy", map[string]any{
			"host": names[k.host], "container": k.key, "policy": policy, "converted_stack": converted, "by": who,
		})
	}
	s.Mon.PoliciesChanged()
	s.Hub.Publish(TopicInventory)
	d, err := s.containersData(r, filterFrom(r.PostFormValue))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "containers", "containers-table", d)
}

// ---- ISPs ----

type ispsData struct {
	ISPs      []store.ISPRow
	WeekTotal int64
	IsAdmin   bool
	CurrentID int
}

func (s *Server) ispsData(r *http.Request) (ispsData, error) {
	rows, err := s.Store.ISPs(r.Context(), s.now())
	if err != nil {
		return ispsData{}, err
	}
	d := ispsData{ISPs: rows, IsAdmin: sessionFrom(r).User.IsAdmin(), CurrentID: s.Mon.Status().ISP}
	for _, i := range rows {
		if i.Role != store.RoleIgnored {
			d.WeekTotal += i.WeekSeconds
		}
	}
	return d, nil
}

func (s *Server) ispsPage(w http.ResponseWriter, r *http.Request) {
	d, err := s.ispsData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.page(w, r, "isps", "ISPs", d)
}

func (s *Server) ispsTable(w http.ResponseWriter, r *http.Request) {
	s.Hub.Publish(TopicState)
	d, err := s.ispsData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "isps", "isps-table", d)
}

func (s *Server) setISPRole(w http.ResponseWriter, r *http.Request) {
	id := atoi(r.PathValue("id"), 0)
	role := r.PostFormValue("role")
	if err := s.Store.SetRole(r.Context(), id, role, true); err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "isp_role", map[string]any{"isp": id, "role": role, "by": actorName(r)})
	s.ispsTable(w, r)
}

func (s *Server) setISPMetered(w http.ResponseWriter, r *http.Request) {
	id := atoi(r.PathValue("id"), 0)
	metered := r.PostFormValue("metered") == "on"
	if err := s.Store.SetMetered(r.Context(), id, metered); err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "isp_metered", map[string]any{"isp": id, "metered": metered, "by": actorName(r)})
	s.ispsTable(w, r)
}

func (s *Server) renameISP(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RenameISP(r.Context(), atoi(r.PathValue("id"), 0), r.PostFormValue("name")); err != nil {
		invalid(w, err.Error())
		return
	}
	s.ispsTable(w, r)
}

func (s *Server) relearn(w http.ResponseWriter, r *http.Request) {
	if err := s.Mon.Relearn(r.Context(), actorName(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ispsTable(w, r)
}

func (s *Server) moveASN(w http.ResponseWriter, r *http.Request) {
	asn := atoi(r.PathValue("asn"), 0)
	to := r.PostFormValue("to")
	target := 0
	if to != "new" {
		if target = atoi(to, -1); target <= 0 {
			invalid(w, "Choose an ISP.")
			return
		}
	}
	id, err := s.Store.MoveASN(r.Context(), asn, target, s.now())
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "asn_moved", map[string]any{"asn": asn, "isp": id, "by": actorName(r)})
	s.ispsTable(w, r)
}

// ---- events ----

type eventsData struct {
	Events []store.Event
	Types  []string
	Type   string
	Next   int64 // BeforeID for the next page, 0 if none
}

const eventsPageSize = 100

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := eventsData{Type: q.Get("type")}
	var err error
	if d.Events, err = s.Store.Events(ctx, store.EventFilter{
		Type: d.Type, BeforeID: int64(atoi(q.Get("before"), 0)), Limit: eventsPageSize,
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(d.Events) == eventsPageSize {
		d.Next = d.Events[len(d.Events)-1].ID
	}
	if d.Types, err = s.Store.EventTypes(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	s.page(w, r, "events", "Events", d)
}

func (s *Server) eventsCSV(w http.ResponseWriter, r *http.Request) {
	events, err := s.Store.Events(r.Context(), store.EventFilter{Type: r.URL.Query().Get("type"), Limit: 1000})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="drawbridge-events.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"time", "type", "actor", "details"})
	for _, e := range events {
		cw.Write([]string{e.TS.UTC().Format(time.RFC3339), e.Type, e.Actor, csvSafe(summary(e.Payload))})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		s.Log.Warn("events export interrupted", "err", err) // headers are sent: nothing else to do
	}
}

// csvSafe stops spreadsheet apps from treating a cell as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ---- settings ----

type settingsData struct {
	Tab       string        // the tab shown
	Tabs      []settingsTab // the tabs this user can open
	Users     usersData
	SSO       ssoForm
	Linked    []store.Identity
	Picture   string // source of your profile picture: store.PictureUpload, store.PictureSSO or ""
	HasLocal  bool
	Detection detectionForm
	Docker    string
	DockerOK  bool
	DBPath    string
	DBSize    string
	IsAdmin   bool
	Version   string
	Themes    []choice
	Modes     []choice
	Notify    notifyData
	Schedule  scheduleView
	API       apiTokensData
	Filters   string // container filter patterns, one per line
	Hosts     hostsView
	TwoFactor twoFactorView
	Test      testScheduleView
}

type choice struct{ ID, Name string }

// Themes are the colour themes users can choose, in menu order. Their
// colours are in static/app.css.
var Themes = []choice{
	{"default", "Drawbridge"}, {"nord", "Nord"}, {"solarized", "Solarized"},
	{"gruvbox", "Gruvbox"}, {"catppuccin", "Catppuccin"}, {"tokyo-night", "Tokyo Night"},
}

// Modes choose light or dark; auto follows the device.
var Modes = []choice{{"auto", "Automatic"}, {"light", "Light"}, {"dark", "Dark"}}

func validChoice(list []choice, id string) bool {
	return slices.ContainsFunc(list, func(c choice) bool { return c.ID == id })
}

func (s *Server) saveAppearance(w http.ResponseWriter, r *http.Request) {
	theme, mode := r.FormValue("theme"), r.FormValue("mode")
	if !validChoice(Themes, theme) || !validChoice(Modes, mode) {
		http.Error(w, "unknown theme or mode", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetAppearance(r.Context(), sessionFrom(r).User.ID, theme, mode); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// settingsTab is one tab of the Settings page, at /settings/{ID}.
type settingsTab struct {
	ID, Name string
	Admin    bool // only admins see and open it
}

// settingsTabs are the Settings tabs in order. The first is also /settings.
var settingsTabs = []settingsTab{
	{ID: "detection", Name: "WAN detection"},
	{ID: "notifications", Name: "Notifications", Admin: true},
	{ID: "backup", Name: "Backup and restore", Admin: true},
	{ID: "users", Name: "Users", Admin: true},
	{ID: "sso", Name: "Single sign-on", Admin: true},
	{ID: "api", Name: "API tokens", Admin: true},
	{ID: "hosts", Name: "Hosts"},
	{ID: "container-filters", Name: "Container filters"},
	{ID: "account", Name: "Your account"},
	{ID: "system", Name: "System"},
}

// settingsPage shows one tab of Settings, loading only what that tab needs.
func (s *Server) settingsPage(tab settingsTab) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me := sessionFrom(r).User
		d := settingsData{Tab: tab.ID, IsAdmin: me.IsAdmin(), HasLocal: me.HasLocal}
		for _, t := range settingsTabs {
			if !t.Admin || d.IsAdmin {
				d.Tabs = append(d.Tabs, t)
			}
		}
		if err := s.loadSettingsTab(r, &d); err != nil {
			s.fail(w, r, err)
			return
		}
		s.page(w, r, "settings", tab.Name+" · Settings", d)
	}
}

func (s *Server) loadSettingsTab(r *http.Request, d *settingsData) error {
	ctx := r.Context()
	var err error
	switch d.Tab {
	case "detection":
		if err = s.loadDetection(ctx, d); err == nil {
			d.Test, err = s.testScheduleView(r)
		}
	case "notifications":
		d.Notify, err = s.notifyData(ctx)
	case "backup":
		if err = s.loadDetection(ctx, d); err == nil { // for the retention hint
			d.Schedule, err = s.scheduleView(ctx)
		}
	case "users":
		d.Users, err = s.usersData(r)
	case "sso":
		err = s.loadSSO(r, d)
	case "api":
		d.API, err = s.apiTokensData(r)
	case "hosts":
		d.Hosts, err = s.hostsView(r)
	case "container-filters":
		var h hideRules
		h, err = s.hideRules(ctx)
		d.Filters = strings.Join(h, "\n")
	case "account":
		d.Themes, d.Modes = Themes, Modes
		if err = s.loadSSO(r, d); err == nil {
			d.Linked, err = s.Store.Identities(ctx, sessionFrom(r).User.ID)
		}
		if err == nil {
			var p store.Picture
			p, _, err = s.Store.UserPicture(ctx, sessionFrom(r).User.ID)
			d.Picture = p.Source
		}
		if err == nil {
			d.TwoFactor, err = s.twoFactorView(r)
		}
	case "system":
		s.loadSystem(ctx, d)
	}
	return err
}

func (s *Server) loadDetection(ctx context.Context, d *settingsData) error {
	det, err := s.Mon.Detection(ctx)
	d.Detection = formFromDetection(det)
	return err
}

func (s *Server) loadSSO(r *http.Request, d *settingsData) error {
	o, err := s.oidcSettings(r.Context())
	d.SSO = s.ssoForm(r, o)
	return err
}

func (s *Server) loadSystem(ctx context.Context, d *settingsData) {
	d.Version, d.DBPath = s.Version, s.DBPath
	var err error
	d.Docker, err = s.Docker.Ping(ctx)
	d.DockerOK = err == nil
	if err != nil {
		d.Docker = err.Error()
	}
	var size int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.DBPath + suffix); err == nil {
			size += fi.Size()
		}
	}
	d.DBSize = fmt.Sprintf("%.1f MB", float64(size)/1e6)
}

func (s *Server) saveDetection(w http.ResponseWriter, r *http.Request) {
	d, err := s.Mon.Detection(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := applyDetectionForm(r, &d); err != nil {
		invalid(w, err.Error())
		return
	}
	if err := s.Mon.ApplyDetection(r.Context(), d); err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "settings_changed", map[string]any{"by": actorName(r)})
	w.Header().Set("HX-Trigger", `{"toast":"Detection settings saved."}`)
	s.partial(w, r, "settings", "detection-form", settingsData{Detection: formFromDetection(d), IsAdmin: true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := sessionFrom(r).User
	if _, err := s.Store.Authenticate(ctx, u.Username, r.PostFormValue("current"), s.now()); err != nil {
		invalid(w, "Your current password is wrong.")
		return
	}
	if r.PostFormValue("new") != r.PostFormValue("confirm") {
		invalid(w, "The new passwords do not match.")
		return
	}
	if err := s.Store.SetPassword(ctx, u.ID, r.PostFormValue("new")); err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(ctx, "password_changed", map[string]any{"user": u.Username})
	// SetPassword ended every session, this one included: sign in again.
	redirect(w, r, "/login?next=/settings/account")
}

// ---- users ----

type usersData struct {
	Users      []store.User
	Me         int
	Error      string
	GroupRoles bool // the SSO admin group sets the role of accounts without a password
}

func (s *Server) usersData(r *http.Request) (usersData, error) {
	d := usersData{Me: sessionFrom(r).User.ID}
	o, err := s.oidcSettings(r.Context())
	if err != nil {
		return d, err
	}
	d.GroupRoles = o.Enabled && o.AdminGroup != ""
	d.Users, err = s.Store.Users(r.Context())
	return d, err
}

func (s *Server) usersTable(w http.ResponseWriter, r *http.Request) {
	d, err := s.usersData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "settings", "users-table", d)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.CreateUser(r.Context(), r.PostFormValue("username"), r.PostFormValue("password"),
		r.PostFormValue("role"), s.now())
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "user_created", map[string]any{"user": u.Username, "role": u.Role, "by": actorName(r)})
	s.usersTable(w, r)
}

// setUserRole changes another user's role. Accounts whose role the SSO admin
// group sets are refused: their next sign-in would change it back.
func (s *Server) setUserRole(w http.ResponseWriter, r *http.Request, id int) {
	ctx := r.Context()
	if id == sessionFrom(r).User.ID {
		invalid(w, "You cannot change your own role.")
		return
	}
	d, err := s.usersData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.Store.UserByID(ctx, id)
	if err != nil {
		invalid(w, "no such user")
		return
	}
	if d.GroupRoles && !u.HasLocal {
		invalid(w, u.Username+"'s role comes from the single sign-on admin group.")
		return
	}
	role := r.PostFormValue("role")
	if err := s.Store.SetUserRole(ctx, id, role); err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(ctx, "user_role", map[string]any{"user": u.Username, "role": role, "by": actorName(r)})
	s.usersTable(w, r)
}

func (s *Server) userOp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := atoi(r.PathValue("id"), 0)
	var err error
	op := r.PathValue("op")
	switch op {
	case "disable":
		if id == sessionFrom(r).User.ID {
			invalid(w, "You cannot disable yourself.")
			return
		}
		err = s.Store.SetUserDisabled(ctx, id, true)
	case "enable":
		err = s.Store.SetUserDisabled(ctx, id, false)
	case "delete":
		if id == sessionFrom(r).User.ID {
			invalid(w, "You cannot delete yourself.")
			return
		}
		err = s.Store.DeleteUser(ctx, id)
	case "role":
		s.setUserRole(w, r, id)
		return
	case "two-factor":
		s.userTwoFactorOff(w, r, id)
		return
	case "password":
		var u store.User
		if u, err = s.Store.UserByID(ctx, id); err == nil && !u.HasLocal {
			invalid(w, u.Username+" signs in with single sign-on only and has no password.")
			return
		}
		if err == nil {
			err = s.Store.SetPassword(ctx, id, r.PostFormValue("password"))
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(ctx, "user_"+op, map[string]any{"user_id": id, "by": actorName(r)})
	s.usersTable(w, r)
}

// userTwoFactorOff turns off another user's two-factor sign-in, for a lost
// phone with no recovery codes left.
func (s *Server) userTwoFactorOff(w http.ResponseWriter, r *http.Request, id int) {
	ctx := r.Context()
	u, err := s.Store.UserByID(ctx, id)
	if err != nil {
		invalid(w, "Unknown user.")
		return
	}
	on, err := s.Store.DisableTwoFactor(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if on {
		s.event(ctx, "two_factor_off", map[string]any{"user": u.Username, "by": actorName(r)})
	}
	s.usersTable(w, r)
}
