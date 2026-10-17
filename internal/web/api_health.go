// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

// Health checks of GET /api/health, in the order they are reported. Each
// can be left out with ?ignore=<name>,...
var healthChecks = []string{"failover", "checks", "inconclusive", "hosts", "paused"}

// apiProblem is one failing health check.
type apiProblem struct {
	Check   string `json:"check"`
	Message string `json:"message"`
}

// apiHealth is the body of GET /api/health.
type apiHealth struct {
	Healthy  bool         `json:"healthy"`
	State    string       `json:"state"`
	Problems []apiProblem `json:"problems"`
}

// apiHealthCheck answers 200 when nothing is wrong and 503 with the
// problems otherwise, for uptime monitors that only look at the status code.
func (s *Server) apiHealthCheck(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuth(w, r) {
		return
	}
	ignore := map[string]bool{}
	for _, name := range strings.Split(r.URL.Query().Get("ignore"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			ignore[name] = true
		}
	}
	for name := range ignore {
		if !slices.Contains(healthChecks, name) {
			http.Error(w, "unknown check "+name+"; known: "+strings.Join(healthChecks, ", "), http.StatusBadRequest)
			return
		}
	}
	problems, err := s.healthProblems(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := apiHealth{State: string(s.Mon.Status().State), Problems: []apiProblem{}}
	for _, p := range problems {
		if !ignore[p.Check] {
			out.Problems = append(out.Problems, p)
		}
	}
	out.Healthy = len(out.Problems) == 0
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !out.Healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		s.Log.Warn("api health write failed", "err", err)
	}
}

// staleAfter is how long after the last check (or the start, before the
// first) the checks count as stopped.
func staleAfter(interval, timeout time.Duration) time.Duration {
	return 3*interval + timeout
}

// healthProblems runs every health check.
func (s *Server) healthProblems(r *http.Request) ([]apiProblem, error) {
	ctx := r.Context()
	st := s.Mon.Status()
	now := s.now()
	var out []apiProblem
	if (st.State == wan.Failover || st.State == wan.FailbackPending) && st.DrillTill.IsZero() {
		out = append(out, apiProblem{"failover", "on a backup ISP since " + st.Since.UTC().Format(time.RFC3339)})
	}
	det, err := s.Mon.Detection(ctx)
	if err != nil {
		return nil, err
	}
	limit := staleAfter(det.CheckInterval, det.Timeout)
	switch {
	case st.LastCheck.IsZero() && now.Sub(s.started) > limit:
		out = append(out, apiProblem{"checks", fmt.Sprintf("no check since the start %s ago", now.Sub(s.started).Round(time.Second))})
	case !st.LastCheck.IsZero() && now.Sub(st.LastCheck) > limit:
		out = append(out, apiProblem{"checks", fmt.Sprintf("the last check was %s ago", now.Sub(st.LastCheck).Round(time.Second))})
	}
	if !st.LastCheck.IsZero() && !st.Conclusive {
		out = append(out, apiProblem{"inconclusive", "the last check could not tell which ISP is in use"})
	}
	hs, err := s.Store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if h.Status == store.HostOffline {
			out = append(out, apiProblem{"hosts", "host " + h.Name + " is offline"})
		}
	}
	if st.Paused {
		out = append(out, apiProblem{"paused", "automation is paused"})
	}
	return out, nil
}
