// SPDX-License-Identifier: AGPL-3.0-or-later

package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// Limits of the agent API.
const (
	maxBody       = 4 << 20 // an inventory of a few thousand workloads
	enrollPerMin  = 10      // enroll attempts per client address per minute
	refusedRepeat = time.Hour
)

// Handler serves the agent API under /api/agent/v1/.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+agentauth.PathEnroll, m.enroll)
	mux.HandleFunc("GET "+agentauth.PathCommands, m.authed(m.poll))
	mux.HandleFunc("POST "+agentauth.PathResults, m.authed(m.results))
	mux.HandleFunc("POST "+agentauth.PathInventory, m.authed(m.inventory))
	mux.HandleFunc("POST "+agentauth.PathRotate, m.authed(m.rotate))
	return http.MaxBytesHandler(mux, maxBody)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v) //nolint:errcheck // the agent retries
}

func (m *Manager) enroll(w http.ResponseWriter, r *http.Request) {
	ctx, now, addr := r.Context(), m.Now(), m.ClientIP(r)
	if !m.allowEnroll(addr, now) {
		http.Error(w, "too many enrollment attempts; wait a minute", http.StatusTooManyRequests)
		return
	}
	var req agentauth.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pub, err := agentauth.ParsePublicKey(req.PublicKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	keyID, key, err := m.currentKey(ctx)
	if err != nil {
		m.fail(w, err)
		return
	}
	a, err := m.Store.Enroll(ctx, store.Enrollment{
		Token: req.Token, PublicKey: pub.String(), Platform: req.Platform, Version: req.Version,
		Address: addr, ManagerKeyID: keyID,
	}, now)
	if errors.Is(err, store.ErrEnrollToken) || errors.Is(err, store.ErrEnrollExpired) {
		m.Log.Warn("agent enrollment refused", "address", addr, "err", err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if err != nil {
		m.fail(w, err)
		return
	}
	c := m.connect(a)
	m.mu.Lock()
	c.lastPoll, c.online = now, true
	m.mu.Unlock()
	m.Log.Info("agent enrolled", "host", a.Name, "address", addr, "key", pub.Fingerprint())
	m.event(ctx, "host_enrolled", map[string]any{"host": a.Name, "platform": req.Platform, "version": req.Version,
		"address": addr, "key": pub.Fingerprint()})
	m.changed()
	writeJSON(w, agentauth.EnrollResponse{HostID: a.ID, Name: a.Name, ManagerKeyID: keyID, ManagerKey: key.Public().String()})
}

// allowEnroll limits enroll attempts per client address.
func (m *Manager) allowEnroll(addr string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	recent := slices.DeleteFunc(m.enrolls[addr], func(t time.Time) bool { return now.Sub(t) > time.Minute })
	if len(recent) >= enrollPerMin {
		m.enrolls[addr] = recent
		return false
	}
	m.enrolls[addr] = append(recent, now)
	return true
}

// agentRequest is an authenticated request.
type agentRequest struct {
	host     store.AgentHost
	body     []byte
	previous bool // signed with the key a rotation replaced
}

// authed checks an agent's signature before calling h.
func (m *Manager) authed(h func(http.ResponseWriter, *http.Request, agentRequest)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, now := r.Context(), m.Now()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		a, err := agentauth.ParseAuth(r.Header.Get("Authorization"))
		if err != nil {
			unauthorized(w, err.Error())
			return
		}
		host, ok, err := m.Store.Agent(ctx, a.Host)
		if err != nil {
			m.fail(w, err)
			return
		}
		if !ok || host.PublicKey == "" {
			unauthorized(w, "unknown, revoked or not enrolled host")
			return
		}
		if host.Status == store.HostDisabled {
			http.Error(w, "this host is disabled in Drawbridge", http.StatusForbidden)
			return
		}
		keys := []agentauth.PublicKey{}
		for _, s := range []string{host.PublicKey, host.PreviousKey} {
			if k, err := agentauth.ParsePublicKey(s); err == nil {
				keys = append(keys, k)
			}
		}
		used, err := a.Verify(keys, r.Method, r.URL.RequestURI(), body, now)
		if err != nil {
			m.refuse(ctx, host, err)
			unauthorized(w, err.Error())
			return
		}
		if !m.nonces.Use(strconv.FormatInt(a.Host, 10)+"/"+a.Nonce, now) {
			unauthorized(w, "replayed request")
			return
		}
		if age := now.Sub(host.KeyCreated); age > MaxKeyAge {
			m.refuse(ctx, host, errors.New("key older than 90 days"))
			unauthorized(w, "the agent's key is older than 90 days; make a new enrollment token in Settings → Hosts")
			return
		}
		previous := host.PreviousKey != "" && used.String() == host.PreviousKey
		if !previous && host.PreviousKey != "" {
			// The new key is in use: the rotation is complete.
			if err := m.Store.RetirePreviousKey(ctx, host.ID); err != nil {
				m.fail(w, err)
				return
			}
		}
		m.seen(ctx, host, m.ClientIP(r), now)
		h(w, r, agentRequest{host: host, body: body, previous: previous})
	}
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", agentauth.Scheme)
	http.Error(w, msg, http.StatusUnauthorized)
}

func (m *Manager) fail(w http.ResponseWriter, err error) {
	m.Log.Error("agent API request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// refuse logs a refused request and records agent_refused, at most once an
// hour per host.
func (m *Manager) refuse(ctx context.Context, host store.AgentHost, err error) {
	m.Log.Warn("agent request refused", "host", host.Name, "err", err)
	m.mu.Lock()
	last, now := m.refused[host.ID], m.Now()
	repeat := !last.IsZero() && now.Sub(last) < refusedRepeat
	if !repeat {
		m.refused[host.ID] = now
	}
	m.mu.Unlock()
	if !repeat {
		m.event(ctx, "agent_refused", map[string]any{"host": host.Name, "reason": err.Error()})
	}
}

// seen records that an agent made a valid request; a host that was offline
// is back.
func (m *Manager) seen(ctx context.Context, host store.AgentHost, addr string, now time.Time) {
	if err := m.Store.AgentSeen(ctx, host.ID, addr, now); err != nil {
		m.Log.Error("cannot record an agent request", "err", err)
	}
	m.mu.Lock()
	c := m.conns[host.ID]
	back := c != nil && !c.online
	if c != nil {
		c.lastPoll, c.online = now, true
	}
	m.mu.Unlock()
	if c == nil {
		// Enrolled before this manager started, or enabled again. Read the
		// host again: it may have been revoked or disabled since this
		// request was checked.
		m.life.Lock()
		defer m.life.Unlock()
		fresh, ok, err := m.Store.Agent(ctx, host.ID)
		if err == nil && ok && fresh.PublicKey != "" && fresh.Status != store.HostDisabled && fresh.Status != store.HostRevoked {
			m.connect(fresh).lastPoll = now
		}
		return
	}
	if back {
		m.Log.Info("agent back online", "host", host.Name)
		m.event(ctx, "host_online", map[string]any{"host": host.Name})
		m.changed()
	}
}

func (m *Manager) poll(w http.ResponseWriter, r *http.Request, req agentRequest) {
	ctx := r.Context()
	wait := agentauth.PollWait
	if s, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && s >= 0 {
		wait = min(wait, time.Duration(s)*time.Second)
	}
	m.rekeyIfNeeded(ctx, req.host)
	m.mu.Lock()
	c := m.conns[req.host.ID]
	m.mu.Unlock()
	if c == nil {
		writeJSON(w, agentauth.Poll{Commands: []agentauth.Command{}})
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		if cmds := m.take(c); len(cmds) > 0 {
			writeJSON(w, agentauth.Poll{Commands: cmds})
			return
		}
		select {
		case <-c.wake:
		case <-c.gone:
			unauthorized(w, "this host was revoked, disabled or removed")
			return
		case <-t.C:
			writeJSON(w, agentauth.Poll{Commands: []agentauth.Command{}})
			return
		case <-ctx.Done():
			return
		}
	}
}

// take removes and returns the queued commands that have not expired.
func (m *Manager) take(c *conn) []agentauth.Command {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now().Unix()
	var out []agentauth.Command
	for _, cmd := range c.queue {
		if cmd.Expires >= now {
			out = append(out, cmd)
		}
	}
	c.queue = nil
	return out
}

// rekeyIfNeeded sends an agent the current manager key, signed with the one
// it has pinned, until it has moved: again after an hour without an answer.
func (m *Manager) rekeyIfNeeded(ctx context.Context, host store.AgentHost) {
	k, err := m.loadKeys(ctx)
	if err != nil || host.ManagerKeyID == k.Current {
		return
	}
	m.mu.Lock()
	c := m.conns[host.ID]
	busy := c == nil || m.Now().Sub(c.rekeyAt) < time.Hour
	if !busy {
		c.rekeyAt = m.Now()
	}
	m.mu.Unlock()
	if busy {
		return
	}
	key, err := m.keyByID(k, k.Current)
	if err != nil {
		m.Log.Error("cannot read the manager key", "err", err)
		return
	}
	if _, _, err := m.send(ctx, host.ID, agentauth.CmdRekey, "",
		agentauth.RekeyArgs{KeyID: k.Current, Key: key.Public().String()}, m.Now().Add(time.Hour)); err != nil {
		m.Log.Error("cannot send the new manager key", "host", host.Name, "err", err)
	}
}

func (m *Manager) results(w http.ResponseWriter, r *http.Request, req agentRequest) {
	ctx := r.Context()
	var rs []agentauth.Result
	if err := json.Unmarshal(req.body, &rs); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, res := range rs {
		m.mu.Lock()
		c := m.conns[req.host.ID]
		var ch chan agentauth.Result
		if c != nil {
			ch = c.waiting[res.ID]
			delete(c.waiting, res.ID)
		}
		m.mu.Unlock()
		if ch == nil {
			continue // no one waits any more
		}
		ch <- res
		m.handleKeyResult(ctx, req.host, res)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleKeyResult completes a manager key change an agent confirmed.
func (m *Manager) handleKeyResult(ctx context.Context, host store.AgentHost, res agentauth.Result) {
	var moved agentauth.RekeyArgs
	if !res.OK || json.Unmarshal(res.Data, &moved) != nil || moved.KeyID == "" {
		return
	}
	if err := m.Store.SetManagerKeyID(ctx, host.ID, moved.KeyID); err != nil {
		m.Log.Error("cannot record the agent's new manager key", "err", err)
		return
	}
	m.mu.Lock()
	if c := m.conns[host.ID]; c != nil {
		c.keyID, c.rekeyAt = moved.KeyID, time.Time{}
	}
	m.mu.Unlock()
	m.Log.Info("agent moved to the new manager key", "host", host.Name, "key", moved.KeyID)
	if err := m.dropUnusedKeys(ctx); err != nil {
		m.Log.Error("cannot drop old manager keys", "err", err)
	}
}

func (m *Manager) inventory(w http.ResponseWriter, r *http.Request, req agentRequest) {
	var inv agentauth.Inventory
	if err := json.Unmarshal(req.body, &inv); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for i := range inv.Workloads {
		inv.Workloads[i].Host = req.host.ID
	}
	stored, err := json.Marshal(inv.Workloads)
	if err != nil {
		m.fail(w, err)
		return
	}
	if err := m.Store.SaveInventory(r.Context(), req.host.ID, string(stored), inv.Platform, inv.Version); err != nil {
		m.fail(w, err)
		return
	}
	m.mu.Lock()
	changed := false
	if c := m.conns[req.host.ID]; c != nil {
		old, _ := json.Marshal(c.workloads)
		changed = string(old) != string(stored)
		c.workloads = inv.Workloads
	}
	m.mu.Unlock()
	if changed {
		m.changed()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) rotate(w http.ResponseWriter, r *http.Request, req agentRequest) {
	ctx, now := r.Context(), m.Now()
	var rr agentauth.RotateRequest
	if err := json.Unmarshal(req.body, &rr); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	newKey, err := agentauth.ParsePublicKey(rr.NewKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if newKey.String() == req.host.PublicKey {
		// A repeat of a rotation that was done but not confirmed.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.previous {
		unauthorized(w, "a rotation must be signed with the current key")
		return
	}
	if d := now.Sub(time.Unix(rr.TS, 0)); d > agentauth.MaxSkew || d < -agentauth.MaxSkew ||
		!newKey.Verify(agentauth.RotationProof(req.host.ID, newKey, rr.TS), rr.Proof) {
		http.Error(w, "the new key's proof does not verify", http.StatusBadRequest)
		return
	}
	if err := m.Store.RotateAgentKey(ctx, req.host.ID, newKey.String(), now); err != nil {
		m.fail(w, err)
		return
	}
	m.mu.Lock()
	delete(m.warned, req.host.ID)
	m.mu.Unlock()
	m.Log.Info("agent key rotated", "host", req.host.Name, "key", newKey.Fingerprint())
	m.event(ctx, "host_key_rotated", map[string]any{"host": req.host.Name, "key": newKey.Fingerprint()})
	w.WriteHeader(http.StatusNoContent)
}
