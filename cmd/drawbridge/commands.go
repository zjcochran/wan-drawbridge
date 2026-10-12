// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// cmdEnv is what a subcommand works with.
type cmdEnv struct {
	ctx    context.Context
	st     *store.Store
	w      *tabwriter.Writer // aligned output, flushed after the command
	stdin  io.Reader
	stderr io.Writer
	now    time.Time
}

// subcommands maps each name to its number of arguments and its function.
var subcommands = map[string]struct {
	args int
	run  func(c cmdEnv, args []string) error
}{
	"status":         {0, (cmdEnv).status},
	"stacks":         {0, (cmdEnv).stacks},
	"containers":     {0, (cmdEnv).containers},
	"set-stack":      {2, (cmdEnv).setStack},
	"set-container":  {2, (cmdEnv).setContainer},
	"isps":           {0, (cmdEnv).isps},
	"set-isp":        {2, (cmdEnv).setISP},
	"move-asn":       {2, (cmdEnv).moveASN},
	"reset-password": {1, (cmdEnv).resetPassword},
}

var errUsage = errors.New(usage)

func command(ctx context.Context, args []string, st *store.Store, ex *executor.Executor,
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
	return sub.run(cmdEnv{ctx: ctx, st: st, w: w, stdin: stdin, stderr: stderr, now: time.Now()}, args[1:])
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
		fmt.Fprintf(c.w, "  %s\twas running: %v, stopped: %v %s\n", a.Name, a.WasRunning, a.Stopped, a.Error)
	}
	return nil
}

func (c cmdEnv) stacks([]string) error {
	rows, err := c.st.Stacks(c.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.w, "STACK\tSTATUS\tDISABLING\tCONTAINERS")
	for _, r := range rows {
		fmt.Fprintf(c.w, "%s\t%s\t%d\t%d\n", r.Project, r.Status(), r.Disabling, r.Containers)
	}
	return nil
}

func (c cmdEnv) containers([]string) error {
	rows, err := c.st.Containers(c.ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.w, "KEY\tSTACK\tPOLICY\tEFFECTIVE\tNOTE")
	for _, r := range rows {
		note := ""
		switch {
		case r.NeedsReview:
			note = "new, not configured"
		case r.Policy == store.Inherit && r.StackDisabled:
			note = "from stack"
		}
		fmt.Fprintf(c.w, "%s\t%s\t%s\t%s\t%s\n", r.Key, r.Project, r.Policy, r.Effective, note)
	}
	return nil
}

func (c cmdEnv) setStack(args []string) error {
	project, onOff := args[0], args[1]
	if onOff != "on" && onOff != "off" {
		return errUsage
	}
	if err := c.st.SetStackPolicy(c.ctx, project, onOff == "on", "cli", c.now); err != nil {
		return err
	}
	fmt.Fprintf(c.w, "stack %s: disable on failover %s; its containers now follow the stack\n", project, onOff)
	return nil
}

func (c cmdEnv) setContainer(args []string) error {
	key, policy := args[0], args[1]
	converted, err := c.st.SetContainerPolicy(c.ctx, key, policy, "cli", c.now)
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
	fmt.Fprintln(c.w, "ID\tISP\tROLE\tLAST 7 DAYS\tASNS")
	for _, r := range rows {
		role := r.Role
		if r.Pinned {
			role += " (pinned)"
		}
		var asns []string
		for _, a := range r.ASNs {
			label := fmt.Sprintf("AS%d %s", a.ASN, a.Name)
			if a.Manual {
				label += " (manual)"
			}
			asns = append(asns, label)
		}
		fmt.Fprintf(c.w, "%d\t%s\t%s\t%s\t%s\n", r.ID, r.Name, role,
			(time.Duration(r.WeekSeconds) * time.Second).String(), strings.Join(asns, ", "))
	}
	return nil
}

func (c cmdEnv) setISP(args []string) error {
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return errUsage
	}
	if err := c.st.SetRole(c.ctx, id, args[1], true); err != nil {
		return err
	}
	fmt.Fprintf(c.w, "ISP %d is now %s\n", id, args[1])
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
