// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

// metrics serves Prometheus text format. It is off unless MetricsToken is
// set, and then requires "Authorization: Bearer <token>".
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.MetricsToken == "" {
		http.NotFound(w, r)
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.MetricsToken)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="drawbridge"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	st := s.Mon.Status()
	names, err := s.Store.ISPNames(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	containers, err := s.Store.Containers(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stopped := 0
	if run, err := s.Store.OpenRun(ctx); err == nil && run != nil {
		if actions, err := s.Store.RunActions(ctx, run.ID); err == nil {
			for _, a := range actions {
				if a.Stopped && !a.Started {
					stopped++
				}
			}
		}
	}

	var b strings.Builder
	metric := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	b01 := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}

	metric("drawbridge_build_info", "Drawbridge version.", "gauge")
	fmt.Fprintf(&b, "drawbridge_build_info{version=%q} 1\n", label(s.Version))

	metric("drawbridge_wan_state", "1 for the current failover state.", "gauge")
	for _, state := range []wan.State{wan.Learning, wan.Primary, wan.FailoverPending, wan.Failover, wan.FailbackPending} {
		fmt.Fprintf(&b, "drawbridge_wan_state{state=%q} %d\n", state, b01(st.State == state))
	}
	metric("drawbridge_state_since_timestamp_seconds", "When the current state began.", "gauge")
	fmt.Fprintf(&b, "drawbridge_state_since_timestamp_seconds %d\n", unix(st.Since))

	metric("drawbridge_on_primary", "1 when the last conclusive check was on the primary ISP.", "gauge")
	fmt.Fprintf(&b, "drawbridge_on_primary %d\n", b01(st.PrimaryISP != 0 && st.ISP == st.PrimaryISP))
	metric("drawbridge_paused", "1 while automation is paused.", "gauge")
	fmt.Fprintf(&b, "drawbridge_paused %d\n", b01(st.Paused))

	metric("drawbridge_isp_info", "The ISP traffic currently leaves through.", "gauge")
	if st.ISP != 0 {
		fmt.Fprintf(&b, "drawbridge_isp_info{isp=%q,asn=\"%d\",primary=\"%t\"} 1\n",
			label(names[st.ISP]), st.ASN, st.ISP == st.PrimaryISP)
	}

	metric("drawbridge_last_check_timestamp_seconds", "When the last check ran.", "gauge")
	fmt.Fprintf(&b, "drawbridge_last_check_timestamp_seconds %d\n", unix(st.LastCheck))
	metric("drawbridge_last_check_duration_seconds", "How long the last check took.", "gauge")
	fmt.Fprintf(&b, "drawbridge_last_check_duration_seconds %g\n", st.Counters.LastCheckSeconds)

	c := st.Counters
	metric("drawbridge_checks_total", "Checks since start, by result.", "counter")
	fmt.Fprintf(&b, "drawbridge_checks_total{result=\"conclusive\"} %d\n", c.ChecksConclusive)
	fmt.Fprintf(&b, "drawbridge_checks_total{result=\"inconclusive\"} %d\n", c.ChecksInconclusive)
	metric("drawbridge_failovers_total", "Failovers since start.", "counter")
	fmt.Fprintf(&b, "drawbridge_failovers_total{trigger=\"auto\"} %d\n", c.Failovers)
	fmt.Fprintf(&b, "drawbridge_failovers_total{trigger=\"manual\"} %d\n", c.ManualFailovers)
	metric("drawbridge_failbacks_total", "Failbacks since start.", "counter")
	fmt.Fprintf(&b, "drawbridge_failbacks_total{trigger=\"auto\"} %d\n", c.Failbacks)
	fmt.Fprintf(&b, "drawbridge_failbacks_total{trigger=\"manual\"} %d\n", c.ManualRestores)
	metric("drawbridge_run_errors_total", "Container stop/start runs that ended with an error.", "counter")
	fmt.Fprintf(&b, "drawbridge_run_errors_total %d\n", c.RunErrors)
	metric("drawbridge_flaps_total", "Failovers that came within an hour of a failback.", "counter")
	fmt.Fprintf(&b, "drawbridge_flaps_total %d\n", c.Flaps)
	metric("drawbridge_failback_hold_seconds", "How long the primary must now be stable before failback; grows while it flaps.", "gauge")
	fmt.Fprintf(&b, "drawbridge_failback_hold_seconds %g\n", st.Hold.Seconds())
	metric("drawbridge_pending_due_timestamp_seconds", "When a pending failover or failback completes if nothing interrupts it; 0 when none.", "gauge")
	fmt.Fprintf(&b, "drawbridge_pending_due_timestamp_seconds %d\n", unix(st.Due))

	sent, failed := s.Notify.Counts()
	metric("drawbridge_notifications_total", "Notifications since start, by result (failed means after every retry).", "counter")
	fmt.Fprintf(&b, "drawbridge_notifications_total{result=\"sent\"} %d\n", sent)
	fmt.Fprintf(&b, "drawbridge_notifications_total{result=\"failed\"} %d\n", failed)

	metric("drawbridge_containers_stopped", "Containers Drawbridge has stopped and not yet started.", "gauge")
	fmt.Fprintf(&b, "drawbridge_containers_stopped %d\n", stopped)
	stop, keep := 0, 0
	for _, ct := range containers {
		if ct.Effective == store.Disable {
			stop++
		} else {
			keep++
		}
	}
	metric("drawbridge_containers", "Known containers by what happens to them on failover.", "gauge")
	fmt.Fprintf(&b, "drawbridge_containers{on_failover=\"stop\"} %d\n", stop)
	fmt.Fprintf(&b, "drawbridge_containers{on_failover=\"keep\"} %d\n", keep)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}

// label makes a value safe inside a %q-quoted Prometheus label.
func label(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
}
