// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agent is Drawbridge's agent mode: it runs next to a Docker engine
// the manager cannot reach directly, calls the manager's agent API, and stops
// and starts containers when the manager asks. It opens no port.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
)

// Timings.
const (
	// RotateEvery is how often the agent rotates its key.
	RotateEvery = 30 * 24 * time.Hour
	// HealthyFor is how recent the last successful poll must be for the
	// healthcheck to pass.
	HealthyFor = 90 * time.Second
	// revokedWait is how long the agent waits after the manager refuses it.
	revokedWait = 5 * time.Minute
	// rememberRun is how long command IDs are remembered against replays.
	rememberRun = 24 * time.Hour
	// previousKeyGrace is how long commands signed with the manager key a
	// rekey replaced are still accepted: they may have been sent before it.
	previousKeyGrace = 10 * time.Minute
)

// Files in the agent's folder.
const (
	keyFile       = "key"
	nextKeyFile   = "key.next"
	stateFile     = "state.json"
	heartbeatFile = "heartbeat"
)

// Platforms an agent can control.
const (
	PlatformDocker     = "docker"
	PlatformSwarm      = "swarm"
	PlatformProxmox    = "proxmox"
	PlatformKubernetes = "kubernetes"
)

// Platforms lists the values of DRAWBRIDGE_AGENT_PLATFORM.
var Platforms = []string{PlatformDocker, PlatformSwarm, PlatformProxmox, PlatformKubernetes}

// Config is the agent's settings, from DRAWBRIDGE_* variables.
type Config struct {
	Manager    string   // the manager's address, as the web UI is reached
	Token      string   // one-time enrollment token; ignored once enrolled
	ManagerKey string   // the manager key to pin at enrollment
	Allow      []string // stacks the agent lists and acts on; empty: all
	Dir        string   // where the key and state are kept
	Platform   string
	Version    string
	Hostname   string
}

// ConfigFromEnv reads the agent's settings.
func ConfigFromEnv(getenv func(string) string, version string) (Config, error) {
	c := Config{
		Manager: strings.TrimRight(strings.TrimSpace(getenv("DRAWBRIDGE_MANAGER")), "/"), Token: strings.TrimSpace(getenv("DRAWBRIDGE_ENROLL_TOKEN")),
		ManagerKey: strings.TrimSpace(getenv("DRAWBRIDGE_MANAGER_KEY")), Dir: getenv("DRAWBRIDGE_AGENT_DIR"),
		Platform: strings.TrimSpace(getenv("DRAWBRIDGE_AGENT_PLATFORM")), Version: version,
	}
	if c.Platform == "" {
		c.Platform = PlatformDocker
	}
	if !slices.Contains(Platforms, c.Platform) {
		return c, fmt.Errorf("DRAWBRIDGE_AGENT_PLATFORM must be one of %s", strings.Join(Platforms, ", "))
	}
	if c.Dir == "" {
		c.Dir = "/data/agent"
	}
	for _, g := range strings.Split(getenv("DRAWBRIDGE_AGENT_ALLOW"), ",") {
		if g = strings.TrimSpace(g); g != "" {
			c.Allow = append(c.Allow, g)
		}
	}
	u, err := url.Parse(c.Manager)
	if c.Manager == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, errors.New("DRAWBRIDGE_MANAGER must be the manager's address, for example https://drawbridge.example.com")
	}
	c.Hostname, _ = os.Hostname()
	return c, nil
}

// EventSource streams Docker container events.
type EventSource interface {
	Events(ctx context.Context, fn func(action, id string)) error
}

// Agent is one running agent.
type Agent struct {
	Cfg            Config
	Runtime        hosts.Runtime
	Events         EventSource // may be nil
	HTTP           *http.Client
	Log            *slog.Logger
	Now            func() time.Time
	InventoryEvery time.Duration
	RetryEvery     time.Duration // first wait after a failed poll; doubles up to a minute

	rot sync.Mutex // one key rotation at a time
	mu  sync.Mutex
	key agentauth.Key
	st  state
	ran map[string]time.Time
}

// state is what the agent keeps between runs, besides its key.
type state struct {
	HostID       int64     `json:"host_id"`
	Name         string    `json:"name"`
	ManagerKeyID string    `json:"manager_key_id"`
	ManagerKey   string    `json:"manager_key"`
	KeyCreated   time.Time `json:"key_created"`
	// The manager key a rekey replaced, accepted until PreviousUntil.
	PreviousManagerKey string    `json:"previous_manager_key,omitempty"`
	PreviousUntil      time.Time `json:"previous_until,omitzero"`
}

// New returns an agent with the default timings.
func New(cfg Config, rt hosts.Runtime, events EventSource, log *slog.Logger) *Agent {
	return &Agent{
		Cfg: cfg, Runtime: rt, Events: events, Log: log, Now: time.Now,
		HTTP:           &http.Client{Timeout: agentauth.PollWait + 35*time.Second},
		InventoryEvery: time.Minute, RetryEvery: 5 * time.Second, ran: map[string]time.Time{},
	}
}

// Run enrolls if needed, then polls for commands and pushes the inventory
// until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.Cfg.Dir, 0o700); err != nil {
		return err
	}
	if err := a.loadKey(); err != nil {
		return err
	}
	if err := a.load(); err != nil {
		return err
	}
	if strings.HasPrefix(a.Cfg.Manager, "http://") {
		a.Log.Warn("the manager is reached over plain HTTP: requests and commands are signed, but the inventory can be read on the way")
	}
	if a.st.HostID == 0 {
		if err := a.enroll(ctx); err != nil {
			return err
		}
	}
	a.Log.Info("agent running", "host", a.st.Name, "manager", a.Cfg.Manager, "key", a.key.Public().Fingerprint())
	if err := a.finishRotation(ctx); err != nil {
		a.Log.Warn("cannot finish an earlier key rotation; trying again later", "err", err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { a.pollLoop(ctx) })
	wg.Go(func() { a.inventoryLoop(ctx) })
	wg.Go(func() { a.rotateLoop(ctx) })
	wg.Wait()
	return nil
}

// ---- files ----

func (a *Agent) path(name string) string { return filepath.Join(a.Cfg.Dir, name) }

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *Agent) loadKey() error {
	b, err := os.ReadFile(a.path(keyFile))
	if errors.Is(err, os.ErrNotExist) {
		k, err := agentauth.NewKey()
		if err != nil {
			return err
		}
		a.key = k
		return writeFile(a.path(keyFile), []byte(k.String()+"\n"))
	}
	if err != nil {
		return err
	}
	a.key, err = agentauth.ParseKey(string(b))
	return err
}

func (a *Agent) load() error {
	b, err := os.ReadFile(a.path(stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &a.st)
}

func (a *Agent) save() error {
	b, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(a.path(stateFile), b)
}

// Healthy reports whether the agent in dir polled the manager recently.
func Healthy(dir string, now time.Time) error {
	fi, err := os.Stat(filepath.Join(dir, heartbeatFile))
	if err != nil {
		return errors.New("the agent has not reached the manager yet")
	}
	if age := now.Sub(fi.ModTime()); age > HealthyFor {
		return fmt.Errorf("the agent last reached the manager %s ago", age.Round(time.Second))
	}
	return nil
}

func (a *Agent) heartbeat() {
	now := a.Now()
	p := a.path(heartbeatFile)
	if err := os.WriteFile(p, nil, 0o600); err == nil {
		os.Chtimes(p, now, now) //nolint:errcheck // only the healthcheck reads it
	}
}

// ---- requests ----

// errRefused is the manager refusing the agent: revoked, or a bad key.
var errRefused = errors.New("refused by the manager")

// do sends a signed request; out, if not nil, receives the JSON answer.
func (a *Agent) do(ctx context.Context, method, path string, body, out any) error {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	a.mu.Lock()
	key, host := a.key, a.st.HostID
	a.mu.Unlock()
	auth, err := agentauth.SignRequest(key, host, method, path, raw, a.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Cfg.Manager+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.send(req, out)
}

func (a *Agent) send(req *http.Request, out any) error {
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%w: %s", errRefused, strings.TrimSpace(string(msg)))
	case resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("manager: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	case out != nil && resp.StatusCode != http.StatusNoContent:
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// enroll trades the one-time token for the host's identity and the manager
// key, which must match the pinned one.
func (a *Agent) enroll(ctx context.Context) error {
	if a.Cfg.Token == "" || a.Cfg.ManagerKey == "" {
		return errors.New("not enrolled yet: set DRAWBRIDGE_ENROLL_TOKEN and DRAWBRIDGE_MANAGER_KEY from Settings → Hosts in Drawbridge")
	}
	pin, err := agentauth.ParsePublicKey(a.Cfg.ManagerKey)
	if err != nil {
		return fmt.Errorf("DRAWBRIDGE_MANAGER_KEY: %w", err)
	}
	body, err := json.Marshal(agentauth.EnrollRequest{Token: a.Cfg.Token, PublicKey: a.key.Public().String(),
		Hostname: a.Cfg.Hostname, Platform: a.Cfg.Platform, Version: a.Cfg.Version})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Cfg.Manager+agentauth.PathEnroll, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var resp agentauth.EnrollResponse
	if err := a.send(req, &resp); err != nil {
		return fmt.Errorf("enrollment failed: %w", err)
	}
	got, err := agentauth.ParsePublicKey(resp.ManagerKey)
	if err != nil || !got.Equal(pin) {
		return errors.New("enrollment failed: the manager's key is not the one in DRAWBRIDGE_MANAGER_KEY; check the address and the key")
	}
	a.st = state{HostID: resp.HostID, Name: resp.Name, ManagerKeyID: resp.ManagerKeyID, ManagerKey: got.String(), KeyCreated: a.Now()}
	if err := a.save(); err != nil {
		return err
	}
	a.Log.Info("enrolled", "host", resp.Name)
	return nil
}

// ---- polling ----

func (a *Agent) pollLoop(ctx context.Context) {
	wait := a.RetryEvery
	for ctx.Err() == nil {
		var p agentauth.Poll
		err := a.do(ctx, http.MethodGet, fmt.Sprintf("%s?wait=%d", agentauth.PathCommands, int(agentauth.PollWait.Seconds())), nil, &p)
		pause := time.Duration(0)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errRefused):
			a.Log.Error("the manager refuses this agent; make a new enrollment token in Settings → Hosts if it was revoked", "err", err)
			pause = revokedWait
		case err != nil:
			a.Log.Warn("cannot reach the manager", "err", err)
			pause, wait = wait, min(2*wait, time.Minute)
		default:
			wait = a.RetryEvery
			a.heartbeat()
			for _, cmd := range p.Commands {
				go a.handle(ctx, cmd)
			}
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pause):
			}
		}
	}
}

// handle runs a command and sends its result.
func (a *Agent) handle(ctx context.Context, cmd agentauth.Command) {
	res := a.run(ctx, cmd)
	if res.Error != "" {
		a.Log.Warn("command failed", "type", cmd.Type, "workload", cmd.Workload, "err", res.Error)
	} else {
		a.Log.Info("command done", "type", cmd.Type, "workload", cmd.Workload)
	}
	if err := a.do(ctx, http.MethodPost, agentauth.PathResults, []agentauth.Result{res}, nil); err != nil {
		a.Log.Warn("cannot send a command's result", "err", err)
	}
}

// accept checks a command's signature, host, expiry and that it is new.
func (a *Agent) accept(cmd agentauth.Command) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	pin, err := agentauth.ParsePublicKey(a.st.ManagerKey)
	if err != nil {
		return err
	}
	now := a.Now()
	if err := cmd.Check(pin, a.st.HostID, now); err != nil {
		prev, perr := agentauth.ParsePublicKey(a.st.PreviousManagerKey)
		if perr != nil || now.After(a.st.PreviousUntil) || cmd.Check(prev, a.st.HostID, now) != nil {
			return err
		}
	}
	for id, t := range a.ran {
		if now.Sub(t) > rememberRun {
			delete(a.ran, id)
		}
	}
	if _, ok := a.ran[cmd.ID]; ok {
		return fmt.Errorf("command %s was already run", cmd.ID)
	}
	a.ran[cmd.ID] = now
	return nil
}

func (a *Agent) run(ctx context.Context, cmd agentauth.Command) agentauth.Result {
	res := agentauth.Result{ID: cmd.ID}
	if err := a.accept(cmd); err != nil {
		a.Log.Error("command refused", "err", err)
		res.Error = err.Error()
		return res
	}
	data, err := a.execute(ctx, cmd)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	if data != nil {
		res.Data, err = json.Marshal(data)
		if err != nil {
			res.OK, res.Error = false, err.Error()
		}
	}
	return res
}

func (a *Agent) execute(ctx context.Context, cmd agentauth.Command) (any, error) {
	switch cmd.Type {
	case agentauth.CmdStats:
		return a.stats(ctx)
	case agentauth.CmdRotate:
		return nil, a.rotate(ctx)
	case agentauth.CmdRekey:
		return a.rekey(cmd)
	}
	w, err := a.workload(ctx, cmd.Workload)
	if err != nil {
		return nil, err
	}
	switch cmd.Type {
	case agentauth.CmdInspect:
		running, health, err := a.Runtime.Inspect(ctx, w)
		return agentauth.InspectData{Running: running, Health: health}, err
	case agentauth.CmdStop:
		var args agentauth.StopArgs
		if err := json.Unmarshal(cmd.Args, &args); err != nil {
			return nil, err
		}
		restore, err := a.Runtime.Stop(ctx, w, time.Duration(args.TimeoutSeconds)*time.Second)
		return agentauth.StopData{Restore: restore}, err
	case agentauth.CmdStart:
		var args agentauth.StartArgs
		if len(cmd.Args) > 0 {
			if err := json.Unmarshal(cmd.Args, &args); err != nil {
				return nil, err
			}
		}
		return nil, a.Runtime.Start(ctx, w, args.Restore)
	}
	return nil, fmt.Errorf("unknown command %q", cmd.Type)
}

// allowed reports whether the agent may list and act on w.
func (a *Agent) allowed(w hosts.Workload) bool {
	return len(a.Cfg.Allow) == 0 || slices.Contains(a.Cfg.Allow, w.Group)
}

// list returns the workloads the agent may act on.
func (a *Agent) list(ctx context.Context) ([]hosts.Workload, error) {
	ws, err := a.Runtime.List(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ws, func(w hosts.Workload) bool { return !a.allowed(w) }), nil
}

// workload finds a workload the agent may act on, by ID.
func (a *Agent) workload(ctx context.Context, id string) (hosts.Workload, error) {
	ws, err := a.Runtime.List(ctx)
	if err != nil {
		return hosts.Workload{}, err
	}
	for _, w := range ws {
		if w.ID == id {
			if !a.allowed(w) {
				return hosts.Workload{}, fmt.Errorf("%s is not in DRAWBRIDGE_AGENT_ALLOW", w.Name)
			}
			return w, nil
		}
	}
	return hosts.Workload{}, fmt.Errorf("no workload %s on this host", id)
}

func (a *Agent) stats(ctx context.Context) (agentauth.StatsData, error) {
	ws, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	out := agentauth.StatsData{}
	for _, w := range ws {
		if !w.Running {
			continue
		}
		if rx, tx, ok, err := a.Runtime.Stats(ctx, w); err == nil && ok {
			out[w.ID] = agentauth.Counters{RX: rx, TX: tx}
		}
	}
	return out, nil
}

// rekey pins the manager's new key, sent signed with the old one.
func (a *Agent) rekey(cmd agentauth.Command) (any, error) {
	var args agentauth.RekeyArgs
	if err := json.Unmarshal(cmd.Args, &args); err != nil {
		return nil, err
	}
	k, err := agentauth.ParsePublicKey(args.Key)
	if err != nil || args.KeyID == "" {
		return nil, errors.New("bad manager key")
	}
	a.mu.Lock()
	a.st.PreviousManagerKey, a.st.PreviousUntil = a.st.ManagerKey, a.Now().Add(previousKeyGrace)
	a.st.ManagerKey, a.st.ManagerKeyID = k.String(), args.KeyID
	err = a.save()
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	a.Log.Info("pinned the manager's new key", "key", args.KeyID)
	return agentauth.RekeyArgs{KeyID: args.KeyID}, nil
}

// ---- inventory ----

func (a *Agent) inventoryLoop(ctx context.Context) {
	changed := make(chan struct{}, 1)
	if a.Events != nil {
		go func() {
			for ctx.Err() == nil {
				err := a.Events.Events(ctx, func(action, _ string) {
					switch action {
					case "create", "destroy", "rename", "start", "die":
						select {
						case changed <- struct{}{}:
						default:
						}
					}
				})
				if ctx.Err() != nil {
					return
				}
				a.Log.Warn("docker event stream ended; reconnecting", "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}
	tick := time.NewTicker(a.InventoryEvery)
	defer tick.Stop()
	for {
		if err := a.pushInventory(ctx); err != nil && ctx.Err() == nil {
			a.Log.Warn("cannot send the inventory", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-changed:
			select { // let a burst of events settle
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (a *Agent) pushInventory(ctx context.Context) error {
	ws, err := a.list(ctx)
	if err != nil {
		return err
	}
	if ws == nil {
		ws = []hosts.Workload{}
	}
	return a.do(ctx, http.MethodPost, agentauth.PathInventory,
		agentauth.Inventory{Version: a.Cfg.Version, Platform: a.Cfg.Platform, Workloads: ws}, nil)
}

// ---- key rotation ----

func (a *Agent) rotateLoop(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		a.mu.Lock()
		due := a.Now().Sub(a.st.KeyCreated) >= RotateEvery
		a.mu.Unlock()
		if due {
			if err := a.rotate(ctx); err != nil && ctx.Err() == nil {
				a.Log.Warn("key rotation failed; trying again in an hour", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// rotate makes a new key and gives it to the manager. The new key is saved
// before it is sent, so a rotation cut short is finished at the next start.
func (a *Agent) rotate(ctx context.Context) error {
	a.rot.Lock()
	defer a.rot.Unlock()
	k, err := agentauth.NewKey()
	if err != nil {
		return err
	}
	if err := writeFile(a.path(nextKeyFile), []byte(k.String()+"\n")); err != nil {
		return err
	}
	return a.sendRotation(ctx, k)
}

func (a *Agent) sendRotation(ctx context.Context, k agentauth.Key) error {
	a.mu.Lock()
	host := a.st.HostID
	a.mu.Unlock()
	ts := a.Now().Unix()
	req := agentauth.RotateRequest{NewKey: k.Public().String(), TS: ts, Proof: k.Sign(agentauth.RotationProof(host, k.Public(), ts))}
	if err := a.do(ctx, http.MethodPost, agentauth.PathRotate, req, nil); err != nil {
		return err
	}
	if err := os.Rename(a.path(nextKeyFile), a.path(keyFile)); err != nil {
		return err
	}
	a.mu.Lock()
	a.key, a.st.KeyCreated = k, a.Now()
	err := a.save()
	a.mu.Unlock()
	if err == nil {
		a.Log.Info("key rotated", "key", k.Public().Fingerprint())
	}
	return err
}

// finishRotation sends a new key left from a rotation cut short.
func (a *Agent) finishRotation(ctx context.Context) error {
	b, err := os.ReadFile(a.path(nextKeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	k, err := agentauth.ParseKey(string(b))
	if err != nil {
		return os.Remove(a.path(nextKeyFile))
	}
	return a.sendRotation(ctx, k)
}
