// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/proxmox"
)

type fakePVE struct {
	guests []proxmox.Guest
	order  map[int]int
	ha     map[string]bool
	calls  []string
	err    error
}

func (f *fakePVE) Guests(context.Context) ([]proxmox.Guest, error) {
	return slices.Clone(f.guests), f.err
}
func (f *fakePVE) StartupOrder(_ context.Context, g proxmox.Guest) (int, error) {
	return f.order[g.VMID], nil
}
func (f *fakePVE) HAManaged(context.Context) (map[string]bool, error) {
	if f.ha == nil {
		return nil, errors.New("403 permission check failed (/, Sys.Audit)")
	}
	return f.ha, nil
}
func (f *fakePVE) Shutdown(_ context.Context, g proxmox.Guest, timeout time.Duration) error {
	f.calls = append(f.calls, "shutdown "+g.ID+" "+timeout.String())
	return nil
}
func (f *fakePVE) Start(_ context.Context, g proxmox.Guest) error {
	f.calls = append(f.calls, "start "+g.ID)
	return nil
}
func (f *fakePVE) SetHAState(_ context.Context, g proxmox.Guest, state string, wait time.Duration) error {
	f.calls = append(f.calls, "ha "+g.ID+" "+state+" "+wait.String())
	return nil
}

func newPVE() *fakePVE {
	return &fakePVE{
		guests: []proxmox.Guest{
			{ID: "lxc/101", VMID: 101, Name: "db", Status: "running", Pool: "media", NetIn: 500, NetOut: 50},
			{ID: "lxc/102", VMID: 102, Name: "app", Status: "running", Pool: "media", Tags: "drawbridge-stop"},
			{ID: "lxc/103", VMID: 103, Name: "web", Status: "stopped", Pool: "media", Tags: "drawbridge-keep"},
			{ID: "lxc/104", VMID: 104, Status: "running"},
			{ID: "lxc/105", VMID: 105, Name: "drawbridge-agent", Status: "running", Pool: "media"},
		},
		order: map[int]int{101: 1, 102: 2},
		ha:    map[string]bool{"ct:102": true},
	}
}

func TestProxmoxList(t *testing.T) {
	p := Proxmox{API: newPVE(), Self: "drawbridge-agent"}
	ws, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Workload{
		{ID: "lxc/101", Name: "db", Kind: KindLXC, Group: "media", Member: "101", Running: true},
		{ID: "lxc/102", Name: "app", Kind: KindLXC, Group: "media", Member: "102", Running: true, Label: "disable", DependsOn: []string{"101"}},
		{ID: "lxc/103", Name: "web", Kind: KindLXC, Group: "media", Member: "103", Label: "keep", DependsOn: []string{"101", "102"}},
		{ID: "lxc/104", Name: "CT 104", Kind: KindLXC, Running: true},
	}
	if !reflect.DeepEqual(ws, want) {
		t.Fatalf("List\n got %+v\nwant %+v", ws, want)
	}
	if ws[0].Key() != "media/101" || ws[3].Key() != "CT 104" {
		t.Errorf("keys %q %q", ws[0].Key(), ws[3].Key())
	}
	f := newPVE()
	f.err = errors.New("401 invalid token")
	if _, err := (Proxmox{API: f}).List(context.Background()); err == nil {
		t.Error("an API error was lost")
	}
}

func TestProxmoxStopAndStart(t *testing.T) {
	ctx := context.Background()
	f := newPVE()
	p := Proxmox{API: f}
	plain, ha := Workload{ID: "lxc/101"}, Workload{ID: "lxc/102"}

	if restore, err := p.Stop(ctx, plain, 30*time.Second); err != nil || restore != nil {
		t.Fatalf("stop = %s, %v", restore, err)
	}
	if err := p.Start(ctx, plain, nil); err != nil {
		t.Fatal(err)
	}
	restore, err := p.Stop(ctx, ha, 30*time.Second)
	if err != nil || string(restore) != `{"ha":true}` {
		t.Fatalf("HA stop = %s, %v", restore, err)
	}
	if err := p.Start(ctx, ha, restore); err != nil {
		t.Fatal(err)
	}
	// HA-managed now, without restore data: still through HA.
	if err := p.Start(ctx, ha, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"shutdown lxc/101 30s", "start lxc/101", "ha lxc/102 stopped 1m0s", "ha lxc/102 started 2m0s", "ha lxc/102 started 2m0s"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls\n got %v\nwant %v", f.calls, want)
	}
	// A token that cannot read HA stops containers directly.
	f.ha, f.calls = nil, nil
	if _, err := p.Stop(ctx, ha, time.Second); err != nil || len(f.calls) != 1 || f.calls[0] != "shutdown lxc/102 1s" {
		t.Errorf("without HA access: %v %v", err, f.calls)
	}
	if err := p.Start(ctx, plain, []byte("{")); err == nil {
		t.Error("bad restore data was accepted")
	}
	for name, err := range map[string]error{
		"stop":  func() error { _, err := p.Stop(ctx, Workload{ID: "lxc/9"}, 0); return err }(),
		"start": p.Start(ctx, Workload{ID: "lxc/9"}, nil),
	} {
		if err == nil {
			t.Errorf("%s of an unknown container worked", name)
		}
	}
}

func TestProxmoxInspectAndStats(t *testing.T) {
	ctx := context.Background()
	p := Proxmox{API: newPVE()}
	if running, health, err := p.Inspect(ctx, Workload{ID: "lxc/101"}); err != nil || !running || health != "" {
		t.Errorf("inspect = %v %q %v", running, health, err)
	}
	if rx, tx, ok, err := p.Stats(ctx, Workload{ID: "lxc/101"}); err != nil || !ok || rx != 500 || tx != 50 {
		t.Errorf("stats = %d %d %v %v", rx, tx, ok, err)
	}
	if _, _, ok, _ := p.Stats(ctx, Workload{ID: "lxc/103"}); ok {
		t.Error("a stopped container has counters")
	}
	if _, _, _, err := p.Stats(ctx, Workload{ID: "lxc/9"}); err == nil {
		t.Error("stats of an unknown container")
	}
	if _, _, err := p.Inspect(ctx, Workload{ID: "lxc/9"}); err == nil {
		t.Error("inspect of an unknown container")
	}
}
