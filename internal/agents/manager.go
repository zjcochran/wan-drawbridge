// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agents is the manager's side of Drawbridge agents: enrollment, the
// agent API agents call, the command queue they poll, key rotation, and a
// hosts.Runtime for every enrolled agent.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// Timings.
const (
	// OfflineAfter is how long an agent may go without polling before its
	// host counts as offline.
	OfflineAfter = 90 * time.Second
	// MaxKeyAge is the oldest agent key the manager accepts. Agents rotate
	// theirs every RotateEvery.
	MaxKeyAge = 90 * 24 * time.Hour
	// RotateEvery is how often an agent rotates its key.
	RotateEvery = 30 * 24 * time.Hour
	// KeyWarning is how long before MaxKeyAge the host_key_expiring event is
	// recorded.
	KeyWarning = 7 * 24 * time.Hour
	// statsFor is how long one stats answer serves the per-container reads.
	statsFor = 30 * time.Second
	// callTimeout bounds commands other than stop.
	callTimeout = 60 * time.Second
)

// SettingManagerKeys holds the manager's signing keys, sealed with the
// instance key.
const SettingManagerKeys = "manager_keys"

// Manager handles every agent.
type Manager struct {
	Store    *store.Store
	Secrets  *secrets.Box
	Hosts    *hosts.Registry
	Local    hosts.Host // the local host, put back in the registry when it is enabled again
	Log      *slog.Logger
	Now      func() time.Time
	Version  string
	ClientIP func(*http.Request) string // the request's client address, behind trusted proxies
	OnChange func()                     // after an agent's inventory or a host's status changes; may be nil

	life    sync.Mutex // serializes connecting and disconnecting hosts with host changes
	mu      sync.Mutex
	conns   map[int64]*conn
	nonces  agentauth.Nonces
	enrolls map[string][]time.Time // enroll attempts per client address
	refused map[int64]time.Time    // last agent_refused event per host
	warned  map[int64]bool         // host_key_expiring recorded
}

// conn is what the manager keeps in memory for one enrolled agent.
type conn struct {
	id        int64
	name      string
	keyID     string // the manager key the agent has pinned
	lastPoll  time.Time
	online    bool
	workloads []hosts.Workload
	queue     []agentauth.Command
	wake      chan struct{}
	gone      chan struct{} // closed when the host is disconnected
	waiting   map[string]chan agentauth.Result
	rekeyAt   time.Time // when the current manager key was last sent
	stats     agentauth.StatsData
	statsAt   time.Time
}

// New returns a manager; call Load before serving.
func New(st *store.Store, box *secrets.Box, reg *hosts.Registry, local hosts.Host, log *slog.Logger) *Manager {
	return &Manager{
		Store: st, Secrets: box, Hosts: reg, Local: local, Log: log, Now: time.Now,
		ClientIP: func(r *http.Request) string { return r.RemoteAddr },
		conns:    map[int64]*conn{}, enrolls: map[string][]time.Time{}, refused: map[int64]time.Time{}, warned: map[int64]bool{},
	}
}

// Load registers every enrolled, enabled agent host with its last inventory.
// Agents get OfflineAfter to poll again before they count as offline.
func (m *Manager) Load(ctx context.Context) error {
	if _, _, err := m.currentKey(ctx); err != nil {
		return err
	}
	as, err := m.Store.AgentHosts(ctx)
	if err != nil {
		return err
	}
	for _, a := range as {
		if a.PublicKey == "" || a.Status == store.HostDisabled || a.Status == store.HostRevoked {
			continue
		}
		c := m.connect(a)
		c.lastPoll, c.online = m.Now(), a.Status == store.HostOnline
		if err := json.Unmarshal([]byte(a.Inventory), &c.workloads); err != nil {
			m.Log.Warn("cannot read the stored inventory of an agent", "host", a.Name, "err", err)
		}
	}
	rows, err := m.Store.Hosts(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID != store.LocalHost {
			continue
		}
		m.Local.Name = row.Name
		if row.Status == store.HostDisabled {
			m.Hosts.Remove(store.LocalHost)
		} else if m.Local.Runtime != nil {
			m.Hosts.Set(m.Local)
		}
	}
	return nil
}

// Reload drops every agent connection and loads the hosts again, after a
// backup replaced them.
func (m *Manager) Reload(ctx context.Context) error {
	m.life.Lock()
	defer m.life.Unlock()
	m.mu.Lock()
	ids := make([]int64, 0, len(m.conns))
	for id := range m.conns {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.disconnect(id)
	}
	return m.Load(ctx)
}

// connect registers an enrolled agent host.
func (m *Manager) connect(a store.AgentHost) *conn {
	m.mu.Lock()
	c, ok := m.conns[a.ID]
	if !ok {
		c = &conn{id: a.ID, wake: make(chan struct{}, 1), gone: make(chan struct{}), waiting: map[string]chan agentauth.Result{}}
		m.conns[a.ID] = c
	}
	c.name, c.keyID = a.Name, a.ManagerKeyID
	m.mu.Unlock()
	m.Hosts.Set(hosts.Host{ID: a.ID, Name: a.Name, Runtime: runtime{m: m, id: a.ID}})
	return c
}

// disconnect forgets an agent host: commands waiting for it fail.
func (m *Manager) disconnect(id int64) {
	m.Hosts.Remove(id)
	m.mu.Lock()
	c := m.conns[id]
	delete(m.conns, id)
	m.mu.Unlock()
	if c == nil {
		return
	}
	m.mu.Lock()
	close(c.gone)
	for cid, ch := range c.waiting {
		ch <- agentauth.Result{ID: cid, Error: "the host was disconnected"}
		delete(c.waiting, cid)
	}
	m.mu.Unlock()
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}

func (m *Manager) event(ctx context.Context, typ string, payload map[string]any) {
	if err := m.Store.AddEvent(ctx, m.Now(), typ, payload); err != nil {
		m.Log.Error("cannot record event", "type", typ, "err", err)
	}
}

// ---- manager keys ----

type managerKeys struct {
	Current string            `json:"current"`
	Keys    map[string]string `json:"keys"` // ID → sealed private key
}

func (m *Manager) loadKeys(ctx context.Context) (managerKeys, error) {
	var k managerKeys
	if _, err := m.Store.GetSetting(ctx, SettingManagerKeys, &k); err != nil {
		return k, err
	}
	if k.Keys == nil {
		k.Keys = map[string]string{}
	}
	return k, nil
}

// addKey makes a new manager key and makes it current.
func (m *Manager) addKey(ctx context.Context, k managerKeys) (managerKeys, error) {
	key, err := agentauth.NewKey()
	if err != nil {
		return k, err
	}
	sealed, err := m.Secrets.Seal(key.String())
	if err != nil {
		return k, err
	}
	n := len(k.Keys) + 1
	for k.Keys["k"+strconv.Itoa(n)] != "" {
		n++
	}
	id := "k" + strconv.Itoa(n)
	k.Keys[id], k.Current = sealed, id
	return k, m.Store.SetSetting(ctx, SettingManagerKeys, k)
}

// currentKey returns the key new agents pin, making the first one if needed.
func (m *Manager) currentKey(ctx context.Context) (string, agentauth.Key, error) {
	k, err := m.loadKeys(ctx)
	if err != nil {
		return "", agentauth.Key{}, err
	}
	if k.Current == "" {
		if k, err = m.addKey(ctx, k); err != nil {
			return "", agentauth.Key{}, err
		}
	}
	key, err := m.keyByID(k, k.Current)
	return k.Current, key, err
}

func (m *Manager) keyByID(k managerKeys, id string) (agentauth.Key, error) {
	sealed, ok := k.Keys[id]
	if !ok {
		return agentauth.Key{}, fmt.Errorf("no manager key %q", id)
	}
	plain, err := m.Secrets.Open(sealed)
	if err != nil {
		return agentauth.Key{}, err
	}
	return agentauth.ParseKey(plain)
}

// ManagerKey is the public key new agents pin, for the enrollment snippet.
func (m *Manager) ManagerKey(ctx context.Context) (agentauth.PublicKey, error) {
	_, key, err := m.currentKey(ctx)
	return key.Public(), err
}

// RotateManagerKey makes a new manager key. New agents pin it at once;
// connected agents are sent it (signed with the key they have pinned) as
// soon as they poll, and old keys are dropped once no agent uses them.
func (m *Manager) RotateManagerKey(ctx context.Context, actor string) error {
	k, err := m.loadKeys(ctx)
	if err != nil {
		return err
	}
	if k, err = m.addKey(ctx, k); err != nil {
		return err
	}
	m.event(ctx, "manager_key_rotated", map[string]any{"key": k.Current, "by": actor})
	// Connected agents get the new key now; the others when they poll again.
	as, err := m.Store.AgentHosts(ctx)
	if err != nil {
		return err
	}
	for _, a := range as {
		if a.PublicKey != "" {
			m.rekeyIfNeeded(ctx, a)
		}
	}
	return m.dropUnusedKeys(ctx)
}

// KeyProgress counts the enrolled agents and those that pinned the current
// manager key.
func (m *Manager) KeyProgress(ctx context.Context) (moved, total int, err error) {
	k, err := m.loadKeys(ctx)
	if err != nil {
		return 0, 0, err
	}
	as, err := m.Store.AgentHosts(ctx)
	for _, a := range as {
		if a.PublicKey != "" {
			total++
			if a.ManagerKeyID == k.Current {
				moved++
			}
		}
	}
	return moved, total, err
}

// dropUnusedKeys deletes manager keys no agent has pinned, except the
// current one.
func (m *Manager) dropUnusedKeys(ctx context.Context) error {
	k, err := m.loadKeys(ctx)
	if err != nil {
		return err
	}
	as, err := m.Store.AgentHosts(ctx)
	if err != nil {
		return err
	}
	used := map[string]bool{k.Current: true}
	for _, a := range as {
		if a.PublicKey != "" {
			used[a.ManagerKeyID] = true
		}
	}
	dropped := false
	for id := range k.Keys {
		if !used[id] {
			delete(k.Keys, id)
			dropped = true
		}
	}
	if !dropped {
		return nil
	}
	return m.Store.SetSetting(ctx, SettingManagerKeys, k)
}

// ---- admin operations ----

// Add records a new agent host and returns its ID and one-time token.
func (m *Manager) Add(ctx context.Context, name, actor string) (int64, string, error) {
	id, token, err := m.Store.AddAgentHost(ctx, name, actor, m.Now())
	if err == nil {
		m.event(ctx, "host_added", map[string]any{"host": name, "by": actor})
	}
	return id, token, err
}

// NewToken makes a new one-time token for a host. Its agent stops working
// until it enrolls again with the token.
func (m *Manager) NewToken(ctx context.Context, id int64, actor string) (string, error) {
	a, ok, err := m.Store.Agent(ctx, id)
	if err != nil || !ok {
		return "", errors.Join(err, errors.New("no such agent host"))
	}
	m.life.Lock()
	token, err := m.Store.NewEnrollToken(ctx, id, m.Now())
	if err == nil {
		m.disconnect(id)
	}
	m.life.Unlock()
	if err != nil {
		return "", err
	}
	m.event(ctx, "host_enroll_token", map[string]any{"host": a.Name, "by": actor})
	m.changed()
	return token, nil
}

// Revoke cuts an agent off at once.
func (m *Manager) Revoke(ctx context.Context, id int64, actor string) error {
	a, ok, err := m.Store.Agent(ctx, id)
	if err != nil || !ok {
		return errors.Join(err, errors.New("no such agent host"))
	}
	m.life.Lock()
	err = m.Store.RevokeHost(ctx, id)
	if err == nil {
		m.disconnect(id)
	}
	m.life.Unlock()
	if err != nil {
		return err
	}
	m.event(ctx, "host_revoked", map[string]any{"host": a.Name, "by": actor})
	m.changed()
	return nil
}

// SetEnabled disables a host, taking it out of every failover, or enables it
// again.
func (m *Manager) SetEnabled(ctx context.Context, id int64, enabled bool, actor string) error {
	names, err := m.Store.HostNames(ctx)
	if err != nil {
		return err
	}
	if _, ok := names[id]; !ok {
		return errors.New("no such host")
	}
	m.life.Lock()
	defer m.life.Unlock()
	if err := m.Store.SetHostEnabled(ctx, id, enabled); err != nil {
		return err
	}
	switch {
	case !enabled && id == store.LocalHost:
		m.Hosts.Remove(id)
	case !enabled:
		m.disconnect(id)
	case id == store.LocalHost:
		m.Hosts.Set(m.Local)
	default:
		if a, ok, err := m.Store.Agent(ctx, id); err == nil && ok && a.PublicKey != "" {
			m.connect(a).lastPoll = m.Now()
		}
	}
	typ := "host_disabled"
	if enabled {
		typ = "host_enabled"
	}
	m.event(ctx, typ, map[string]any{"host": names[id], "by": actor})
	m.changed()
	return nil
}

// Rename renames a host.
func (m *Manager) Rename(ctx context.Context, id int64, name, actor string) error {
	names, err := m.Store.HostNames(ctx)
	if err != nil {
		return err
	}
	old, ok := names[id]
	if !ok {
		return errors.New("no such host")
	}
	if err := m.Store.RenameHost(ctx, id, name); err != nil {
		return err
	}
	if h, ok := m.Hosts.Get(id); ok {
		h.Name = name
		m.Hosts.Set(h)
	}
	if id == store.LocalHost {
		m.Local.Name = name
	}
	m.mu.Lock()
	if c := m.conns[id]; c != nil {
		c.name = name
	}
	m.mu.Unlock()
	m.event(ctx, "host_renamed", map[string]any{"host": name, "from": old, "by": actor})
	m.changed()
	return nil
}

// Remove deletes an agent host with its containers and owed starts.
func (m *Manager) Remove(ctx context.Context, id int64, actor string) error {
	a, ok, err := m.Store.Agent(ctx, id)
	if err != nil || !ok {
		return errors.Join(err, errors.New("no such agent host"))
	}
	m.life.Lock()
	err = m.Store.RemoveHost(ctx, id)
	if err == nil {
		m.disconnect(id)
	}
	m.life.Unlock()
	if err != nil {
		return err
	}
	m.event(ctx, "host_removed", map[string]any{"host": a.Name, "by": actor})
	m.changed()
	return m.dropUnusedKeys(ctx)
}

// ForgetPending drops the starts still owed to a host.
func (m *Manager) ForgetPending(ctx context.Context, id int64, actor string) error {
	names, err := m.Store.HostNames(ctx)
	if err != nil {
		return err
	}
	n, err := m.Store.ForgetPending(ctx, id)
	if err != nil {
		return err
	}
	m.event(ctx, "pending_forgotten", map[string]any{"host": names[id], "containers": n, "by": actor})
	m.changed()
	return nil
}

// RotateNow asks an agent to rotate its key at its next poll.
func (m *Manager) RotateNow(ctx context.Context, id int64) error {
	_, _, err := m.send(ctx, id, agentauth.CmdRotate, "", nil, m.Now().Add(time.Hour))
	return err
}

// ---- watching ----

// Watch marks agents that stop polling offline, and warns about keys close
// to MaxKeyAge, until ctx ends.
func (m *Manager) Watch(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		m.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (m *Manager) check(ctx context.Context) {
	now := m.Now()
	m.mu.Lock()
	var gone []*conn
	for _, c := range m.conns {
		if c.online && now.Sub(c.lastPoll) > OfflineAfter {
			c.online = false
			gone = append(gone, c)
		}
	}
	m.mu.Unlock()
	sort.Slice(gone, func(i, j int) bool { return gone[i].id < gone[j].id })
	for _, c := range gone {
		msg := "no contact since " + c.lastPoll.UTC().Format(time.RFC3339)
		if err := m.Store.SetHostReached(ctx, c.id, msg, now); err != nil {
			m.Log.Error("cannot record a host going offline", "err", err)
		}
		m.Log.Warn("agent offline", "host", c.name, "last_poll", c.lastPoll)
		m.event(ctx, "host_offline", map[string]any{"host": c.name, "last_seen": c.lastPoll.UTC().Format(time.RFC3339)})
	}
	if len(gone) > 0 {
		m.changed()
	}
	as, err := m.Store.AgentHosts(ctx)
	if err != nil {
		m.Log.Error("cannot read agent hosts", "err", err)
		return
	}
	for _, a := range as {
		if a.PublicKey == "" || a.KeyCreated.IsZero() || now.Sub(a.KeyCreated) < MaxKeyAge-KeyWarning || m.warned[a.ID] {
			continue
		}
		m.warned[a.ID] = true
		m.event(ctx, "host_key_expiring", map[string]any{"host": a.Name,
			"expires": a.KeyCreated.Add(MaxKeyAge).UTC().Format(time.RFC3339)})
	}
}
