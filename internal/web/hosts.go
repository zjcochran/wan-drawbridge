// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/agents"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// ---- Settings → Hosts ----

type hostsView struct {
	Hosts      []hostView
	IsAdmin    bool
	Manager    string // the address agents use
	ManagerKey string
	Moved      int // agents that pinned the current manager key
	Total      int // enrolled agents
	New        *newHostView
	Enabled    bool // agents can be added (the manager runs the agent API)
}

type hostView struct {
	store.HostRow
	Agent         bool
	Version       string
	Fingerprint   string // of the agent's key
	KeyCreated    time.Time
	LastAddress   string
	EnrollPending bool
	EnrollExpires time.Time
	Owed          int // starts owed to it
	OldVersion    bool
}

// newHostView is a host just added, or given a new token: its enrollment
// snippet is shown once.
type newHostView struct {
	Name, Token, Snippet string
}

func (s *Server) hostsView(r *http.Request) (hostsView, error) {
	ctx := r.Context()
	v := hostsView{IsAdmin: sessionFrom(r).User.IsAdmin(), Manager: s.externalBase(r), Enabled: s.Agents != nil}
	rows, err := s.Store.Hosts(ctx)
	if err != nil {
		return v, err
	}
	as, err := s.Store.AgentHosts(ctx)
	if err != nil {
		return v, err
	}
	agentByID := map[int64]store.AgentHost{}
	for _, a := range as {
		agentByID[a.ID] = a
	}
	owed, err := s.Store.PendingStarts(ctx)
	if err != nil {
		return v, err
	}
	for _, h := range rows {
		hv := hostView{HostRow: h}
		if a, ok := agentByID[h.ID]; ok {
			hv.Agent, hv.Version, hv.KeyCreated, hv.LastAddress = true, a.Version, a.KeyCreated, a.LastAddress
			hv.EnrollPending, hv.EnrollExpires = a.EnrollPending, a.EnrollExpires
			hv.OldVersion = a.Version != "" && a.Version != s.Version
			if k, err := agentauth.ParsePublicKey(a.PublicKey); err == nil {
				hv.Fingerprint = k.Fingerprint()
			}
		}
		for _, p := range owed {
			if p.HostID == h.ID {
				hv.Owed++
			}
		}
		v.Hosts = append(v.Hosts, hv)
	}
	if s.Agents != nil && v.IsAdmin {
		k, err := s.Agents.ManagerKey(ctx)
		if err != nil {
			return v, err
		}
		v.ManagerKey = k.String()
		if v.Moved, v.Total, err = s.Agents.KeyProgress(ctx); err != nil {
			return v, err
		}
	}
	return v, nil
}

// externalBase is the address the browser uses, which agents use too.
func (s *Server) externalBase(r *http.Request) string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	scheme := "http"
	if s.secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// snippet is the Compose file for a new agent.
func snippet(manager, managerKey, token string) string {
	return fmt.Sprintf(`services:
  drawbridge-agent:
    image: ghcr.io/zjcochran/wan-drawbridge:latest
    command: agent
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./agent:/data/agent
    environment:
      DRAWBRIDGE_MANAGER: %s
      DRAWBRIDGE_ENROLL_TOKEN: %s
      DRAWBRIDGE_MANAGER_KEY: %s
      # DRAWBRIDGE_AGENT_ALLOW: media,downloads
`, manager, token, managerKey)
}

func (s *Server) hostsFragment(w http.ResponseWriter, r *http.Request, created *newHostView) {
	v, err := s.hostsView(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if created != nil {
		created.Snippet = snippet(v.Manager, v.ManagerKey, created.Token)
		v.New = created
	}
	s.Hub.Publish(TopicInventory)
	s.partial(w, r, "settings", "hosts", v)
}

func (s *Server) addHost(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	_, token, err := s.Agents.Add(r.Context(), name, actorName(r))
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.hostsFragment(w, r, &newHostView{Name: name, Token: token})
}

func (s *Server) hostOp(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		http.NotFound(w, r)
		return
	}
	ctx, who := r.Context(), actorName(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var created *newHostView
	switch r.PathValue("op") {
	case "rename":
		err = s.Agents.Rename(ctx, id, r.PostFormValue("name"), who)
	case "token":
		var token string
		if token, err = s.Agents.NewToken(ctx, id, who); err == nil {
			names, _ := s.Store.HostNames(ctx)
			created = &newHostView{Name: names[id], Token: token}
		}
	case "revoke":
		err = s.Agents.Revoke(ctx, id, who)
	case "disable", "enable":
		err = s.Agents.SetEnabled(ctx, id, r.PathValue("op") == "enable", who)
	case "remove":
		err = s.Agents.Remove(ctx, id, who)
	case "rotate":
		err = s.Agents.RotateNow(ctx, id)
	case "forget":
		err = s.Agents.ForgetPending(ctx, id, who)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.Mon.PoliciesChanged()
	s.hostsFragment(w, r, created)
}

func (s *Server) rotateManagerKey(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Agents.RotateManagerKey(r.Context(), actorName(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hostsFragment(w, r, nil)
}

// keyAge says how old an agent's key is and when it rotates.
func keyAge(created, now time.Time) string {
	if created.IsZero() {
		return ""
	}
	days := int(now.Sub(created).Hours() / 24)
	left := int((agents.RotateEvery - now.Sub(created)).Hours() / 24)
	if left < 0 {
		return fmt.Sprintf("%d days old; rotation overdue", days)
	}
	return fmt.Sprintf("%d days old; rotates in %d days", days, left)
}
