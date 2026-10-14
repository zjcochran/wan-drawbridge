// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// apiISP is an ISP in the JSON status. ASN and Metered are set for the
// current ISP only.
type apiISP struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	ASN     int    `json:"asn,omitempty"`
	Metered *bool  `json:"metered,omitempty"`
}

// apiStatus is the body of GET /api/status. Times are RFC 3339 in UTC, null
// when not set.
type apiStatus struct {
	State       string     `json:"state"`
	StateLabel  string     `json:"state_label"`
	Since       time.Time  `json:"since"`
	Due         *time.Time `json:"due"` // when a pending failover or failback completes
	CurrentISP  *apiISP    `json:"current_isp"`
	PrimaryISP  *apiISP    `json:"primary_isp"`
	PublicIP    string     `json:"public_ip"`
	LastCheck   *time.Time `json:"last_check"`
	Conclusive  bool       `json:"conclusive"`
	Paused      bool       `json:"paused"`
	PausedUntil *time.Time `json:"paused_until"`
	TestUntil   *time.Time `json:"failover_test_until"`
	Busy        string     `json:"busy"`    // "stopping", "starting", "applying" or ""
	Stopped     []string   `json:"stopped"` // containers Drawbridge has stopped
	Version     string     `json:"version"`
}

func utcOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// apiAuth checks the Bearer token of an API request, answering 401 when it
// is missing or unknown.
func (s *Server) apiAuth(w http.ResponseWriter, r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok {
		var err error
		if _, ok, err = s.Store.UseAPIToken(r.Context(), strings.TrimSpace(token), s.now()); err != nil {
			s.fail(w, r, err)
			return false
		}
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="drawbridge"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return ok
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuth(w, r) {
		return
	}
	ctx := r.Context()
	st := s.Mon.Status()
	names, err := s.Store.ISPNames(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := apiStatus{
		State: string(st.State), StateLabel: stateLabel(st.State), Since: st.Since.UTC(),
		PublicIP: st.LastIP, LastCheck: utcOrNil(st.LastCheck), Conclusive: st.Conclusive,
		Paused: st.Paused, PausedUntil: utcOrNil(st.PausedTill), TestUntil: utcOrNil(st.DrillTill),
		Busy: st.Busy, Stopped: []string{}, Version: s.Version,
	}
	if st.State == "failover_pending" || st.State == "failback_pending" {
		out.Due = utcOrNil(st.Due)
	}
	if st.ISP != 0 {
		metered := !st.Unmetered && st.ISP != st.PrimaryISP
		out.CurrentISP = &apiISP{ID: st.ISP, Name: names[st.ISP], ASN: st.ASN, Metered: &metered}
	}
	if st.PrimaryISP != 0 {
		out.PrimaryISP = &apiISP{ID: st.PrimaryISP, Name: names[st.PrimaryISP]}
	}
	run, err := s.Store.OpenRun(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if run != nil {
		actions, err := s.Store.RunActions(ctx, run.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, a := range actions {
			if a.Stopped && !a.Started {
				out.Stopped = append(out.Stopped, a.Name)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		s.Log.Warn("api status write failed", "err", err)
	}
}

// ---- tokens, in Settings ----

type apiTokensData struct {
	Tokens []store.APIToken
	New    string // a token just made, shown once
	Name   string // its name
	Base   string // the address the API is reached at, for the example
}

func (s *Server) apiTokensData(r *http.Request) (apiTokensData, error) {
	d := apiTokensData{Base: s.BaseURL}
	if d.Base == "" {
		d.Base = "http://" + r.Host
	}
	var err error
	d.Tokens, err = s.Store.APITokens(r.Context())
	return d, err
}

func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	token, err := s.Store.CreateAPIToken(r.Context(), name, actorName(r), s.now())
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "api_token_created", map[string]any{"name": strings.TrimSpace(name), "by": actorName(r)})
	s.apiTokensList(w, r, token, strings.TrimSpace(name))
}

func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	name, err := s.Store.DeleteAPIToken(r.Context(), id)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "api_token_deleted", map[string]any{"name": name, "by": actorName(r)})
	s.apiTokensList(w, r, "", "")
}

func (s *Server) apiTokensList(w http.ResponseWriter, r *http.Request, token, name string) {
	d, err := s.apiTokensData(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d.New, d.Name = token, name
	s.partial(w, r, "settings", "api-tokens", d)
}
