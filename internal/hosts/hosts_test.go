// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/docker"
)

type fakeDocker struct {
	cs    []docker.Container
	err   error
	block bool // List waits for its context
	calls []string
}

func (f *fakeDocker) List(ctx context.Context) ([]docker.Container, error) {
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.cs, f.err
}

func (f *fakeDocker) Inspect(_ context.Context, id string) (bool, docker.Health, error) {
	f.calls = append(f.calls, "inspect "+id)
	return true, "healthy", nil
}

func (f *fakeDocker) Stop(_ context.Context, id string, timeout time.Duration) error {
	f.calls = append(f.calls, "stop "+id+" "+timeout.String())
	return nil
}

func (f *fakeDocker) Start(_ context.Context, id string) error {
	f.calls = append(f.calls, "start "+id)
	return nil
}

func (f *fakeDocker) Stats(_ context.Context, id string) (int64, int64, bool, error) {
	f.calls = append(f.calls, "stats "+id)
	return 5, 6, true, nil
}

func TestLocal(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{cs: []docker.Container{
		{ID: "0123456789abcdef", Name: "drawbridge"}, // its own container, by ID
		{ID: "b", Name: "labelled", Labels: map[string]string{docker.LabelSelf: "true"}},
		{ID: "c", Name: "app-web-1", Project: "app", Service: "web", Running: true, Labels: map[string]string{
			docker.LabelOnFailover: "stop", docker.LabelDependsOn: "db:service_started:false",
		}},
		{ID: "d", Name: "dns"},
	}}
	l := Local{Docker: f, SelfID: "0123456789ab"}
	ws, err := l.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Workload{
		{ID: "c", Name: "app-web-1", Kind: KindContainer, Group: "app", Member: "web", Running: true, Label: "disable", DependsOn: []string{"db"}},
		{ID: "d", Name: "dns", Kind: KindContainer},
	}
	if !reflect.DeepEqual(ws, want) {
		t.Fatalf("List\n got %+v\nwant %+v", ws, want)
	}
	if ws[0].Key() != "app/web" || ws[1].Key() != "dns" {
		t.Errorf("keys %q %q", ws[0].Key(), ws[1].Key())
	}

	running, health, _ := l.Inspect(ctx, ws[0])
	restore, _ := l.Stop(ctx, ws[0], 10*time.Second)
	_ = l.Start(ctx, ws[0], nil)
	rx, tx, ok, _ := l.Stats(ctx, ws[0])
	if !running || health != "healthy" || restore != nil || rx != 5 || tx != 6 || !ok {
		t.Errorf("results: %v %q %v %d %d %v", running, health, restore, rx, tx, ok)
	}
	if !reflect.DeepEqual(f.calls, []string{"inspect c", "stop c 10s", "start c", "stats c"}) {
		t.Errorf("calls = %v", f.calls)
	}

	f.err = errors.New("no socket")
	if _, err := l.List(ctx); err == nil {
		t.Error("a List error was lost")
	}
}

func TestRegistry(t *testing.T) {
	ok := &fakeDocker{cs: []docker.Container{{ID: "a", Name: "a"}}}
	r := NewRegistry(
		Host{ID: 3, Name: "slow", Runtime: Local{Docker: &fakeDocker{block: true}}},
		Host{ID: 1, Name: "local", Runtime: Local{Docker: ok}},
	)
	r.Set(Host{ID: 2, Name: "broken", Runtime: Local{Docker: &fakeDocker{err: errors.New("refused")}}})
	if all := r.All(); len(all) != 3 || all[0].ID != 1 || all[1].ID != 2 || all[2].ID != 3 {
		t.Fatalf("All = %+v", all)
	}
	ls := r.List(context.Background(), 20*time.Millisecond)
	if len(ls) != 3 || ls[0].Err != nil || len(ls[0].Workloads) != 1 || ls[0].Workloads[0].Host != 1 ||
		ls[1].Err == nil || !errors.Is(ls[2].Err, context.DeadlineExceeded) {
		t.Fatalf("List = %+v", ls)
	}
	if h, found := r.Get(2); !found || h.Name != "broken" {
		t.Errorf("Get = %+v %v", h, found)
	}
	r.Remove(2)
	if _, found := r.Get(2); found {
		t.Error("removed host still there")
	}
}
