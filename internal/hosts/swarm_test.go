// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/docker"
)

// fakeSwarm is a swarm manager with services whose specs it keeps as JSON.
type fakeSwarm struct {
	fakeDocker
	services map[string]docker.Service
	specs    map[string]map[string]any
	versions map[string]uint64
	conflict int // updates refused as out of sequence before one is accepted
	err      error
}

func (f *fakeSwarm) Swarm(context.Context) (docker.SwarmInfo, error) {
	return docker.SwarmInfo{State: "active", Manager: true}, nil
}

func (f *fakeSwarm) Services(context.Context) ([]docker.Service, error) {
	var out []docker.Service
	for _, id := range []string{"s-db", "s-web", "s-mon", "s-agent", "s-alone"} {
		if s, ok := f.services[id]; ok {
			out = append(out, s)
		}
	}
	return out, f.err
}

func (f *fakeSwarm) ServiceSpec(_ context.Context, id string) (uint64, map[string]any, error) {
	spec, ok := f.specs[id]
	if !ok {
		return 0, nil, errors.New("no such service")
	}
	// A copy through JSON, as Docker would send it.
	b, _ := json.Marshal(spec)
	var out map[string]any
	json.Unmarshal(b, &out) //nolint:errcheck // test
	return f.versions[id], out, nil
}

func (f *fakeSwarm) UpdateService(_ context.Context, id string, version uint64, spec map[string]any) error {
	if f.conflict > 0 {
		f.conflict--
		f.versions[id]++
		return errors.New("docker: 500: update out of sequence")
	}
	if version != f.versions[id] {
		return errors.New("docker: 500: update out of sequence")
	}
	b, _ := json.Marshal(spec)
	var stored map[string]any
	json.Unmarshal(b, &stored) //nolint:errcheck // test
	f.specs[id], f.versions[id] = stored, version+1
	return nil
}

func newSwarm() *fakeSwarm {
	spec := func(js string) map[string]any {
		var m map[string]any
		json.Unmarshal([]byte(js), &m) //nolint:errcheck // test
		return m
	}
	return &fakeSwarm{
		fakeDocker: fakeDocker{cs: []docker.Container{
			{ID: "0123456789abcdef", Name: "drawbridge-agent.1", Running: true, Labels: map[string]string{docker.LabelSwarmTask: "s-agent"}},
			{ID: "task1", Name: "media_db.1.x", Running: true, Labels: map[string]string{docker.LabelSwarmTask: "s-db"}},
			{ID: "c-dns", Name: "dns", Running: true},
		}},
		services: map[string]docker.Service{
			"s-db": {ID: "s-db", Name: "media_db", Labels: map[string]string{docker.LabelStack: "media", docker.LabelOnFailover: "stop"},
				Replicas: 2, Running: 2, Desired: 2},
			"s-web":   {ID: "s-web", Name: "media_web", Labels: map[string]string{docker.LabelStack: "media"}, Replicas: 1, Running: 0, Desired: 1},
			"s-mon":   {ID: "s-mon", Name: "monitoring_node", Labels: map[string]string{docker.LabelStack: "monitoring"}, Global: true, Running: 3, Desired: 3},
			"s-agent": {ID: "s-agent", Name: "drawbridge-agent", Replicas: 1, Running: 1, Desired: 1},
			"s-alone": {ID: "s-alone", Name: "alone", Replicas: 1, Running: 1, Desired: 1},
		},
		specs: map[string]map[string]any{
			"s-db":  spec(`{"Name":"media_db","Labels":{"com.docker.stack.namespace":"media"},"Mode":{"Replicated":{"Replicas":2}},"TaskTemplate":{"ContainerSpec":{"Image":"db"}}}`),
			"s-mon": spec(`{"Name":"monitoring_node","Mode":{"Global":{}},"TaskTemplate":{"Placement":{"Constraints":["node.role==worker"]}}}`),
			"s-new": spec(`{"Name":"bare","Mode":{"Replicated":{}}}`),
		},
		versions: map[string]uint64{"s-db": 10, "s-mon": 20, "s-new": 1},
	}
}

func TestSwarmList(t *testing.T) {
	s := Swarm{Docker: newSwarm(), SelfID: "0123456789ab"}
	ws, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, w := range ws {
		keys = append(keys, w.Kind+":"+w.Key())
	}
	want := []string{"container:dns", "service:media/db", "service:media/web", "service:monitoring/node", "service:alone"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("listed %v, want %v (no tasks, no agent)", keys, want)
	}
	if ws[1].Label != "disable" || !ws[1].Running || ws[2].Running {
		t.Errorf("services = %+v", ws[1:3])
	}
	f := newSwarm()
	f.err = errors.New("this node is not a swarm manager")
	if _, err := (Swarm{Docker: f}).List(context.Background()); err == nil {
		t.Error("a services error was lost")
	}
	f.fakeDocker.err = errors.New("no socket")
	if _, err := (Swarm{Docker: f}).List(context.Background()); err == nil {
		t.Error("a containers error was lost")
	}
}

func TestSwarmReplicated(t *testing.T) {
	ctx := context.Background()
	f := newSwarm()
	f.conflict = 1 // the first update races another change
	s := Swarm{Docker: f}
	w := Workload{ID: "s-db", Kind: KindService}
	restore, err := s.Stop(ctx, w, 10*time.Second)
	if err != nil || string(restore) != `{"replicas":2}` {
		t.Fatalf("stop = %s, %v", restore, err)
	}
	mode := f.specs["s-db"]["Mode"].(map[string]any)["Replicated"].(map[string]any)
	labels := f.specs["s-db"]["Labels"].(map[string]any)
	if mode["Replicas"] != float64(0) || labels[LabelReplicas] != "2" || f.specs["s-db"]["TaskTemplate"] == nil {
		t.Fatalf("stopped spec = %v", f.specs["s-db"])
	}
	if err := s.Start(ctx, w, restore); err != nil {
		t.Fatal(err)
	}
	mode = f.specs["s-db"]["Mode"].(map[string]any)["Replicated"].(map[string]any)
	if mode["Replicas"] != float64(2) || f.specs["s-db"]["Labels"].(map[string]any)[LabelReplicas] != nil {
		t.Fatalf("started spec = %v", f.specs["s-db"])
	}

	// Without restore data, the label decides; without a label, 1.
	s.Stop(ctx, w, 0) //nolint:errcheck // checked above
	if err := s.Start(ctx, w, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.specs["s-db"]["Mode"].(map[string]any)["Replicated"].(map[string]any)["Replicas"]; n != float64(2) {
		t.Errorf("replicas from the label = %v", n)
	}
	if err := s.Start(ctx, Workload{ID: "s-new", Kind: KindService}, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.specs["s-new"]["Mode"].(map[string]any)["Replicated"].(map[string]any)["Replicas"]; n != float64(1) {
		t.Errorf("replicas without anything to go by = %v", n)
	}
	if err := s.Start(ctx, w, []byte("{")); err == nil {
		t.Error("bad restore data was accepted")
	}
	if _, err := s.Stop(ctx, Workload{ID: "gone", Kind: KindService}, 0); err == nil {
		t.Error("stopped an unknown service")
	}
}

func TestSwarmGlobal(t *testing.T) {
	ctx := context.Background()
	f := newSwarm()
	s := Swarm{Docker: f}
	w := Workload{ID: "s-mon", Kind: KindService}
	restore, err := s.Stop(ctx, w, 0)
	if err != nil || string(restore) != `{"global":true}` {
		t.Fatalf("stop = %s, %v", restore, err)
	}
	constraints := func() []any {
		return f.specs["s-mon"]["TaskTemplate"].(map[string]any)["Placement"].(map[string]any)["Constraints"].([]any)
	}
	if c := constraints(); len(c) != 2 || c[1] != StoppedConstraint {
		t.Fatalf("constraints = %v", c)
	}
	s.Stop(ctx, w, 0) //nolint:errcheck // a second stop adds nothing
	if c := constraints(); len(c) != 2 {
		t.Errorf("constraints after a second stop = %v", c)
	}
	if err := s.Start(ctx, w, restore); err != nil {
		t.Fatal(err)
	}
	if c := constraints(); len(c) != 1 || c[0] != "node.role==worker" {
		t.Errorf("constraints after start = %v", c)
	}
}

func TestSwarmInspectAndContainers(t *testing.T) {
	ctx := context.Background()
	f := newSwarm()
	s := Swarm{Docker: f}
	if running, health, err := s.Inspect(ctx, Workload{ID: "s-db", Kind: KindService}); err != nil || !running || health != "healthy" {
		t.Errorf("inspect a running service = %v %q %v", running, health, err)
	}
	if running, health, _ := s.Inspect(ctx, Workload{ID: "s-web", Kind: KindService}); running || health != "starting" {
		t.Errorf("inspect a starting service = %v %q", running, health)
	}
	if _, _, err := s.Inspect(ctx, Workload{ID: "gone", Kind: KindService}); err == nil || !strings.Contains(err.Error(), "no service") {
		t.Errorf("inspect an unknown service: %v", err)
	}
	if _, _, ok, _ := s.Stats(ctx, Workload{ID: "s-db", Kind: KindService}); ok {
		t.Error("a service has counters")
	}
	// Standalone containers behave as on any Docker host.
	c := Workload{ID: "c-dns", Kind: KindContainer}
	s.Inspect(ctx, c)           //nolint:errcheck // recorded
	s.Stop(ctx, c, time.Second) //nolint:errcheck // recorded
	s.Start(ctx, c, nil)        //nolint:errcheck // recorded
	s.Stats(ctx, c)             //nolint:errcheck // recorded
	if !reflect.DeepEqual(f.calls, []string{"inspect c-dns", "stop c-dns 1s", "start c-dns", "stats c-dns"}) {
		t.Errorf("container calls = %v", f.calls)
	}
}
