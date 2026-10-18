// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agents"
	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// cmdEnv is what a subcommand works with.
type cmdEnv struct {
	ctx    context.Context
	st     *store.Store
	box    func() (*secrets.Box, error) // the instance key, loaded when a command needs it
	base   string                       // DRAWBRIDGE_BASE_URL
	w      *tabwriter.Writer            // aligned output, flushed after the command
	stdin  io.Reader
	stderr io.Writer
	now    time.Time
}

// subcommands maps each name to its number of arguments and its function.
var subcommands = map[string]struct {
	args int
	run  func(c cmdEnv, args []string) error
}{
	"status":          {0, (cmdEnv).status},
	"stacks":          {0, (cmdEnv).stacks},
	"containers":      {0, (cmdEnv).containers},
	"set-stack":       {2, (cmdEnv).setStack},
	"set-container":   {2, (cmdEnv).setContainer},
	"isps":            {0, (cmdEnv).isps},
	"set-isp":         {2, (cmdEnv).setISP},
	"set-data-limits": {3, (cmdEnv).setDataLimits},
	"move-asn":        {2, (cmdEnv).moveASN},
	"reset-password":  {1, (cmdEnv).resetPassword},
	"reset-2fa":       {1, (cmdEnv).resetTwoFactor},
	"hosts":           {0, (cmdEnv).hosts},
	"revoke-host":     {1, (cmdEnv).revokeHost},
	"enroll-token":    {1, (cmdEnv).enrollToken},
}

var errUsage = errors.New(usage)

func command(ctx context.Context, args []string, st *store.Store, ex *executor.Executor, box func() (*secrets.Box, error), base string,
	stdin io.Reader, stdout, stderr io.Writer) error {
	sub, ok := subcommands[args[0]]
	if !ok || len(args)-1 != sub.args {
		return errUsage
	}
	// Refresh the inventory so new containers show up; the stored copy is
	// still useful when Docker cannot be reached.
	if err := ex.Sync(ctx); err != nil {
		fmt.Fprintln(stderr, "warning: could not read Docker, showing stored inventory:", err)
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	return sub.run(cmdEnv{ctx: ctx, st: st, box: box, base: base, w: w, stdin: stdin, stderr: stderr, now: time.Now()}, args[1:])
}

func (c cmdEnv) status([]string) error {
	state, err := c.st.LoadState(c.ctx, c.now)
	if err != nil {
		return err
	}
	primary, pinned, err := c.st.Primary(c.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.w, "state\t%s (since %s)\n", state.Machine.State, state.Since.Format(time.DateTime))
	fmt.Fprintf(c.w, "current ASN\t%d\n", state.CurrentASN)
	fmt.Fprintf(c.w, "primary ISP\t%d (pinned: %v)\n", primary, pinned)
	run, err := c.st.OpenRun(c.ctx)
	if err != nil || run == nil {
		fmt.Fprintln(c.w, "open run\tnone")
		return err
	}
	actions, err := c.st.RunActions(c.ctx, run.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.w, "open run\t#%d since %s\n", run.ID, run.StartedAt.Format(time.DateTime))
	for _, a := range actions {
		fmt.Fprintf(c.w, "  %s\ton %s, was running: %v, stopped: %v %s\n", a.Name, a.Host, a.WasRunning, a.Stopped, a.Error)
	}
	return nil
}

// onHost splits "<host>:<name>" into the host's ID and the name. Without a
// known host name in front, the name is on the local host.
func (c cmdEnv) onHost(arg string) (int64, string, error) {
	name, rest, ok := strings.Cut(arg, ":")
	if !ok {
		return store.LocalHost, arg, nil
	}
	hs, err := c.st.Hosts(c.ctx)
	if err != nil {
		return 0, "", err
	}
	for _, h := range hs {
		if h.Name == name {
			return h.ID, rest, nil
		}
	}
	return 0, "", fmt.Errorf("no host named %q", name)
}

func (c cmdEnv) stacks([]string) error {
	rows, err := c.st.Stacks(c.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.w, "HOST\tSTACK\tSTATUS\tDISABLING\tCONTAINERS")
	for _, r := range rows {
		fmt.Fprintf(c.w, "%s\t%s\t%s\t%d\t%d\n", r.Host, r.Project, r.Status(), r.Disabling, r.Containers)
	}
	return nil
}

func (c cmdEnv) containers([]string) error {
	rows, err := c.st.Containers(c.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.w, "HOST\tKEY\tSTACK\tPOLICY\tEFFECTIVE\tNOTE")
	for _, r := range rows {
		note := ""
		switch {
		case r.NeedsReview:
			note = "new, not configured"
		case r.Policy == store.Inherit && r.Label != "":
			note = "from label"
		case r.Policy == store.Inherit && r.StackDisabled:
			note = "from stack"
		}
		fmt.Fprintf(c.w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Host, r.Key, r.Project, r.Policy, r.Effective, note)
	}
	return nil
}

func (c cmdEnv) setStack(args []string) error {
	host, project, err := c.onHost(args[0])
	if err != nil {
		return err
	}
	onOff := args[1]
	if onOff != "on" && onOff != "off" {
		return errUsage
	}
	if err := c.st.SetStackPolicy(c.ctx, host, project, onOff == "on", "cli", c.now); err != nil {
		return err
	}
	fmt.Fprintf(c.w, "stack %s: disable on failover %s; its containers now follow the stack\n", project, onOff)
	return nil
}

func (c cmdEnv) setContainer(args []string) error {
	host, key, err := c.onHost(args[0])
	if err != nil {
		return err
	}
	policy := args[1]
	converted, err := c.st.SetContainerPolicy(c.ctx, host, key, policy, "cli", c.now)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.w, "container %s: %s\n", key, policy)
	if converted {
		fmt.Fprintln(c.w, "the stack-wide setting was removed; the other containers in the stack stay disabled individually")
	}
	return nil
}

func (c cmdEnv) isps([]string) error {
	rows, err := c.st.ISPs(c.ctx, c.now)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.w, "ID\tISP\tROLE\tDATA LIMITS (OUTAGE / MONTH)\tLAST 7 DAYS\tASNS")
	for _, r := range rows {
		role := r.Role
		if r.Pinned {
			role += " (pinned)"
		}
		if (r.Role == store.RoleBackup || r.Role == store.RolePrimary) && !r.Metered {
			role += " (unmetered)"
		}
		limits := "-"
		if r.Limits.OutageBytes > 0 || r.Limits.MonthBytes > 0 {
			limits = gbOrOff(r.Limits.OutageBytes) + " / " + gbOrOff(r.Limits.MonthBytes)
		}
		var asns []string
		for _, a := range r.ASNs {
			label := fmt.Sprintf("AS%d %s", a.ASN, a.Name)
			if a.Manual {
				label += " (manual)"
			}
			asns = append(asns, label)
		}
		fmt.Fprintf(c.w, "%d\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, role, limits,
			(time.Duration(r.WeekSeconds) * time.Second).String(), strings.Join(asns, ", "))
	}
	return nil
}

func (c cmdEnv) setISP(args []string) error {
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return errUsage
	}
	switch args[1] {
	case "metered", "unmetered":
		err = c.st.SetMetered(c.ctx, id, args[1] == "metered")
	default:
		err = c.st.SetRole(c.ctx, id, args[1], true)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.w, "ISP %d is now %s\n", id, args[1])
	return nil
}

// gbOrOff shows a data limit in GB, or "off".
func gbOrOff(b int64) string {
	if b <= 0 {
		return "off"
	}
	return strconv.FormatFloat(float64(b)/1e9, 'f', -1, 64) + " GB"
}

// parseLimit reads a data limit in GB for set-data-limits; 0 is off.
func parseLimit(v string) (int64, error) {
	gb, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(gb) || gb < 0 || gb > 1e6 {
		return 0, fmt.Errorf("%q: a data limit is a number of GB from 0 to 1000000", v)
	}
	return int64(math.Round(gb * 1e9)), nil
}

func (c cmdEnv) setDataLimits(args []string) error {
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return errUsage
	}
	var l store.DataLimits
	if l.OutageBytes, err = parseLimit(args[1]); err != nil {
		return err
	}
	if l.MonthBytes, err = parseLimit(args[2]); err != nil {
		return err
	}
	if err := c.st.SetDataLimits(c.ctx, id, l); err != nil {
		return err
	}
	fmt.Fprintf(c.w, "ISP %d data limits: %s per outage, %s per month\n", id, gbOrOff(l.OutageBytes), gbOrOff(l.MonthBytes))
	return nil
}

func (c cmdEnv) moveASN(args []string) error {
	asn, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(args[0]), "AS"))
	if err != nil {
		return errUsage
	}
	to := 0 // a new ISP of its own
	if args[1] != "new" {
		if to, err = strconv.Atoi(args[1]); err != nil {
			return errUsage
		}
	}
	id, err := c.st.MoveASN(c.ctx, asn, to, c.now)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.w, "AS%d is now in ISP %d\n", asn, id)
	return nil
}

func (c cmdEnv) resetPassword(args []string) error {
	users, err := c.st.Users(c.ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if !strings.EqualFold(u.Username, args[0]) {
			continue
		}
		fmt.Fprintln(c.stderr, "new password (at least 12 characters), then Enter:")
		line, err := bufio.NewReader(c.stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		if err := c.st.SetPassword(c.ctx, u.ID, strings.TrimRight(line, "\r\n")); err != nil {
			return err
		}
		if u.Disabled {
			if err := c.st.SetUserDisabled(c.ctx, u.ID, false); err != nil {
				return err
			}
		}
		fmt.Fprintf(c.w, "password for %s changed; their sessions were signed out\n", u.Username)
		return nil
	}
	return fmt.Errorf("no user %q", args[0])
}

// resetTwoFactor turns off a user's two-factor sign-in, for a lost phone
// when no admin can sign in to do it.
func (c cmdEnv) resetTwoFactor(args []string) error {
	users, err := c.st.Users(c.ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if !strings.EqualFold(u.Username, args[0]) {
			continue
		}
		on, err := c.st.DisableTwoFactor(c.ctx, u.ID)
		if err != nil {
			return err
		}
		if !on {
			fmt.Fprintf(c.w, "%s does not use two-factor sign-in\n", u.Username)
			return nil
		}
		fmt.Fprintf(c.w, "two-factor sign-in for %s is off; their recovery codes were deleted\n", u.Username)
		return nil
	}
	return fmt.Errorf("no user %q", args[0])
}

func (c cmdEnv) hosts([]string) error {
	rows, err := c.st.Hosts(c.ctx)
	if err != nil {
		return err
	}
	as, err := c.st.AgentHosts(c.ctx)
	if err != nil {
		return err
	}
	version := map[int64]string{}
	for _, a := range as {
		version[a.ID] = a.Version
	}
	fmt.Fprintln(c.w, "ID\tHOST\tKIND\tSTATUS\tLAST SEEN\tVERSION")
	for _, h := range rows {
		seen := "never"
		if !h.LastSeen.IsZero() {
			seen = h.LastSeen.Format(time.DateTime)
		}
		fmt.Fprintf(c.w, "%d\t%s\t%s\t%s\t%s\t%s\n", h.ID, h.Name, h.Kind, h.Status, seen, version[h.ID])
	}
	return nil
}

// agentByName finds an agent host by name.
func (c cmdEnv) agentByName(name string) (store.AgentHost, bool, error) {
	as, err := c.st.AgentHosts(c.ctx)
	for _, a := range as {
		if a.Name == name {
			return a, true, err
		}
	}
	return store.AgentHost{}, false, err
}

func (c cmdEnv) revokeHost(args []string) error {
	a, ok, err := c.agentByName(args[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no agent host named %q", args[0])
	}
	if err := c.st.RevokeHost(c.ctx, a.ID); err != nil {
		return err
	}
	fmt.Fprintf(c.w, "host %s revoked: its agent is refused from now on\n", a.Name)
	return nil
}

// enrollToken prints a one-time enrollment token for an agent host, adding
// the host if it does not exist yet.
func (c cmdEnv) enrollToken(args []string) error {
	box, err := c.box()
	if err != nil {
		return err
	}
	mgr := agents.New(c.st, box, nil, hosts.Host{}, slog.New(slog.DiscardHandler))
	key, err := mgr.ManagerKey(c.ctx)
	if err != nil {
		return err
	}
	a, ok, err := c.agentByName(args[0])
	if err != nil {
		return err
	}
	var token string
	if ok {
		token, err = c.st.NewEnrollToken(c.ctx, a.ID, c.now)
	} else {
		_, token, err = c.st.AddAgentHost(c.ctx, args[0], "cli", c.now)
	}
	if err != nil {
		return err
	}
	manager := c.base
	if manager == "" {
		manager = "<the address of Drawbridge's web UI>"
	}
	fmt.Fprintf(c.w, "DRAWBRIDGE_MANAGER=%s\nDRAWBRIDGE_ENROLL_TOKEN=%s\nDRAWBRIDGE_MANAGER_KEY=%s\n", manager, token, key)
	fmt.Fprintln(c.w, "# the token works once, within an hour")
	return nil
}
