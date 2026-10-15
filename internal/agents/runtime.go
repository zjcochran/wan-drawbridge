// SPDX-License-Identifier: AGPL-3.0-or-later

package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agentauth"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
)

// runtime is the hosts.Runtime of one agent: listing reads the inventory the
// agent last pushed, and the rest are commands it picks up when it polls.
type runtime struct {
	m  *Manager
	id int64
}

// ErrOffline is returned for a host whose agent has not polled within
// OfflineAfter.
var ErrOffline = errors.New("the agent has not been in touch")

func (r runtime) conn() (*conn, error) {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	c := r.m.conns[r.id]
	if c == nil {
		return nil, errors.New("the host is not connected")
	}
	if since := r.m.Now().Sub(c.lastPoll); since > OfflineAfter {
		return nil, fmt.Errorf("%w for %s", ErrOffline, since.Round(time.Second))
	}
	return c, nil
}

// List returns the workloads the agent last pushed.
func (r runtime) List(context.Context) ([]hosts.Workload, error) {
	c, err := r.conn()
	if err != nil {
		return nil, err
	}
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return slices.Clone(c.workloads), nil
}

// Inspect asks the agent for a workload's state.
func (r runtime) Inspect(ctx context.Context, w hosts.Workload) (bool, string, error) {
	var d agentauth.InspectData
	err := r.m.call(ctx, r.id, agentauth.CmdInspect, w.ID, nil, callTimeout, &d)
	return d.Running, d.Health, err
}

// Stop asks the agent to stop a workload, waiting up to the stop timeout
// plus 30 seconds for its answer.
func (r runtime) Stop(ctx context.Context, w hosts.Workload, timeout time.Duration) ([]byte, error) {
	var d agentauth.StopData
	err := r.m.call(ctx, r.id, agentauth.CmdStop, w.ID,
		agentauth.StopArgs{TimeoutSeconds: int(timeout.Seconds())}, timeout+30*time.Second, &d)
	return d.Restore, err
}

// Start asks the agent to start a workload.
func (r runtime) Start(ctx context.Context, w hosts.Workload, restore []byte) error {
	return r.m.call(ctx, r.id, agentauth.CmdStart, w.ID, agentauth.StartArgs{Restore: restore}, callTimeout, nil)
}

// Stats reads a workload's counters. One answer from the agent, with every
// workload's counters, serves for statsFor.
func (r runtime) Stats(ctx context.Context, w hosts.Workload) (int64, int64, bool, error) {
	r.m.mu.Lock()
	c := r.m.conns[r.id]
	fresh := c != nil && c.stats != nil && r.m.Now().Sub(c.statsAt) < statsFor
	var all agentauth.StatsData
	if fresh {
		all = c.stats
	}
	r.m.mu.Unlock()
	if !fresh {
		if err := r.m.call(ctx, r.id, agentauth.CmdStats, "", nil, callTimeout, &all); err != nil {
			return 0, 0, false, err
		}
		r.m.mu.Lock()
		if c := r.m.conns[r.id]; c != nil {
			c.stats, c.statsAt = all, r.m.Now()
		}
		r.m.mu.Unlock()
	}
	cn, ok := all[w.ID]
	return cn.RX, cn.TX, ok, nil
}

// send queues a signed command for an agent and wakes its poll. It returns
// the command's ID and the channel its result arrives on.
func (m *Manager) send(ctx context.Context, id int64, typ, workload string, args any, expires time.Time) (string, chan agentauth.Result, error) {
	m.mu.Lock()
	c := m.conns[id]
	keyID := ""
	if c != nil {
		keyID = c.keyID
	}
	m.mu.Unlock()
	if c == nil {
		return "", nil, errors.New("the host is not connected")
	}
	k, err := m.loadKeys(ctx)
	if err != nil {
		return "", nil, err
	}
	key, err := m.keyByID(k, keyID)
	if err != nil {
		return "", nil, err
	}
	cmd, err := agentauth.NewCommand(key, keyID, id, typ, workload, args, expires)
	if err != nil {
		return "", nil, err
	}
	ch := make(chan agentauth.Result, 1)
	m.mu.Lock()
	c.queue = append(c.queue, cmd)
	c.waiting[cmd.ID] = ch
	m.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return cmd.ID, ch, nil
}

// forget drops a command no one waits for any more.
func (m *Manager) forget(id int64, cmdID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.conns[id]; c != nil {
		delete(c.waiting, cmdID)
		c.queue = slices.DeleteFunc(c.queue, func(q agentauth.Command) bool { return q.ID == cmdID })
	}
}

// call sends a command and waits for its result, decoding its data into out.
func (m *Manager) call(ctx context.Context, id int64, typ, workload string, args any, timeout time.Duration, out any) error {
	if _, err := (runtime{m: m, id: id}).conn(); err != nil {
		return err
	}
	cmdID, ch, err := m.send(ctx, id, typ, workload, args, m.Now().Add(timeout))
	if err != nil {
		return err
	}
	defer m.forget(id, cmdID)
	t := time.NewTimer(timeout)
	defer t.Stop()
	var res agentauth.Result
	select {
	case res = <-ch:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return fmt.Errorf("the agent did not answer %s within %s", typ, timeout)
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	if out != nil && len(res.Data) > 0 {
		return json.Unmarshal(res.Data, out)
	}
	return nil
}
