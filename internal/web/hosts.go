// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"net/http"
	"slices"
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

// experimentalPlatforms have only been tested against fakes so far.
var experimentalPlatforms = []string{"swarm", "proxmox", "kubernetes"}

// Experimental reports whether the host's platform is not yet tested on
// real equipment.
func (h hostView) Experimental() bool { return slices.Contains(experimentalPlatforms, h.Platform) }

// newHostView is a host just added, or given a new token: the examples
// for running its agent are shown once.
type newHostView struct {
	Name, Token string
	Snippets    agentSnippets
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

// agentSnippets are the ways to run a new agent, one per platform, with
// its manager address, enrollment token and the manager's key filled in.
type agentSnippets struct {
	Docker, Swarm, Proxmox, Kubernetes string
}

func snippets(manager, managerKey, token string) agentSnippets {
	return agentSnippets{
		Docker: fmt.Sprintf(`services:
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
`, manager, token, managerKey),
		Swarm: fmt.Sprintf(`# docker stack deploy -c drawbridge-agent.yml drawbridge-agent
services:
  agent:
    image: ghcr.io/zjcochran/wan-drawbridge:latest
    command: agent
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /srv/drawbridge-agent:/data/agent
    environment:
      DRAWBRIDGE_AGENT_PLATFORM: swarm
      DRAWBRIDGE_MANAGER: %s
      DRAWBRIDGE_ENROLL_TOKEN: %s
      DRAWBRIDGE_MANAGER_KEY: %s
      # DRAWBRIDGE_AGENT_ALLOW: media,downloads
    deploy:
      replicas: 1
      placement:
        constraints:
          - node.role == manager
          - node.hostname == manager-1 # one manager node; create /srv/drawbridge-agent on it
`, manager, token, managerKey),
		Proxmox: fmt.Sprintf(`services:
  drawbridge-agent:
    image: ghcr.io/zjcochran/wan-drawbridge:latest
    command: agent
    restart: unless-stopped
    volumes:
      - ./agent:/data/agent
    environment:
      DRAWBRIDGE_AGENT_PLATFORM: proxmox
      DRAWBRIDGE_MANAGER: %s
      DRAWBRIDGE_ENROLL_TOKEN: %s
      DRAWBRIDGE_MANAGER_KEY: %s
      PROXMOX_URL: https://pve.example.com:8006
      PROXMOX_TOKEN_ID: drawbridge@pve!agent
      PROXMOX_TOKEN_SECRET_FILE: /run/secrets/proxmox-token
      PROXMOX_FINGERPRINT: "AB:CD:..." # the API certificate's SHA-256 fingerprint
      # DRAWBRIDGE_AGENT_ALLOW: media,downloads
    secrets:
      - proxmox-token
secrets:
  proxmox-token:
    file: ./proxmox-token # the API token's secret
`, manager, token, managerKey),
		Kubernetes: fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: drawbridge-agent
  namespace: drawbridge
stringData:
  DRAWBRIDGE_MANAGER: %s
  DRAWBRIDGE_ENROLL_TOKEN: %s
  DRAWBRIDGE_MANAGER_KEY: %s
`, manager, token, managerKey),
	}
}

func (s *Server) hostsFragment(w http.ResponseWriter, r *http.Request, created *newHostView) {
	v, err := s.hostsView(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if created != nil {
		created.Snippets = snippets(v.Manager, v.ManagerKey, created.Token)
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
