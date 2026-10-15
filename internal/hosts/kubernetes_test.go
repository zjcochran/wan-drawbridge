// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zjcochran/wan-drawbridge/internal/kube"
)

// fakeKube is a cluster with a few workloads in two namespaces.
type fakeKube struct {
	objs    map[string][]kube.Object // by resource kind
	hpas    []kube.Object
	scale   int64
	patches []string
	listed  []string // namespaces listed
	err     error
}

func obj(ns, name string, set func(*kube.Object)) kube.Object {
	var o kube.Object
	o.Metadata.Namespace, o.Metadata.Name = ns, name
	if set != nil {
		set(&o)
	}
	return o
}

func i64(n int64) *int64 { return &n }
func boolp(b bool) *bool { return &b }

func newKube() *fakeKube {
	return &fakeKube{
		objs: map[string][]kube.Object{
			"deployment": {
				obj("media", "web", func(o *kube.Object) {
					o.Spec.Replicas = i64(3)
					o.Status.ReadyReplicas = 3
					o.Metadata.Labels = map[string]string{"drawbridge.on-failover": "stop", "argocd.argoproj.io/instance": "media"}
				}),
				obj("media", "idle", func(o *kube.Object) { o.Spec.Replicas = i64(0) }),
				obj("drawbridge", "agent", func(o *kube.Object) { o.Metadata.Labels = map[string]string{LabelKubeSelf: "true"} }),
			},
			"statefulset": {obj("media", "db", func(o *kube.Object) {
				o.Metadata.Annotations = map[string]string{"drawbridge.on-failover": "keep"}
				o.Metadata.Labels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps"}
			})},
			"daemonset": {obj("media", "logs", func(o *kube.Object) {
				o.Status.DesiredNumberScheduled, o.Status.NumberReady = 3, 2
			})},
			"cronjob": {obj("media", "backup", func(o *kube.Object) { o.Spec.Suspend = boolp(false) })},
		},
		hpas: []kube.Object{obj("media", "web-hpa", func(o *kube.Object) {
			o.Spec.ScaleTargetRef.Kind, o.Spec.ScaleTargetRef.Name = "Deployment", "web"
		})},
		scale: 3,
	}
}

func (f *fakeKube) List(_ context.Context, r kube.Resource, ns string) ([]kube.Object, error) {
	if f.err != nil {
		return nil, f.err
	}
	if r == kube.HPAs {
		f.listed = append(f.listed, ns)
		return f.hpas, nil
	}
	var out []kube.Object
	for _, o := range f.objs[r.Kind] {
		if ns == "" || o.Metadata.Namespace == ns {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeKube) Get(_ context.Context, r kube.Resource, ns, name string) (kube.Object, error) {
	for _, o := range f.objs[r.Kind] {
		if o.Metadata.Namespace == ns && o.Metadata.Name == name {
			return o, nil
		}
	}
	return kube.Object{}, errors.New("404 not found")
}

func (f *fakeKube) Patch(_ context.Context, r kube.Resource, ns, name, sub string, patch any) error {
	b, _ := json.Marshal(patch)
	f.patches = append(f.patches, strings.TrimSuffix(r.Kind+"/"+ns+"/"+name+"/"+sub, "/")+" "+string(b))
	return nil
}

func (f *fakeKube) Scale(context.Context, kube.Resource, string, string) (int64, error) {
	return f.scale, nil
}

func TestKubernetesList(t *testing.T) {
	f := newKube()
	ws, err := Kubernetes{API: f}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Workload{}
	for _, w := range ws {
		got[w.Key()] = w
	}
	if len(ws) != 5 || got["drawbridge/deployment/agent"].ID != "" {
		t.Fatalf("listed %v (the agent must be left out)", got)
	}
	web := got["media/deployment/web"]
	if web.ID != "deployment/media/web" || web.Kind != "deployment" || !web.Running || web.Label != "disable" ||
		!reflect.DeepEqual(web.Warnings, []string{"managed by Argo CD, which undoes the change unless the application ignores it",
			"the HorizontalPodAutoscaler web-hpa scales it back up"}) {
		t.Errorf("web = %+v", web)
	}
	if db := got["media/statefulset/db"]; !db.Running || db.Label != "keep" || len(db.Warnings) != 1 || !strings.Contains(db.Warnings[0], "Flux") {
		t.Errorf("db (no replicas set: 1) = %+v", db)
	}
	if got["media/deployment/idle"].Running || !got["media/daemonset/logs"].Running || !got["media/cronjob/backup"].Running {
		t.Errorf("running = %+v", got)
	}
	// With namespaces given, each is listed on its own.
	if _, err := (Kubernetes{API: f, Namespaces: []string{"media", "tools"}}).List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.listed, []string{"", "media", "tools"}) {
		t.Errorf("namespaces listed: %v", f.listed)
	}
	f.err = errors.New("403 forbidden")
	if _, err := (Kubernetes{API: f}).List(context.Background()); err == nil {
		t.Error("an API error was lost")
	}
}

func TestKubernetesStopAndStart(t *testing.T) {
	ctx := context.Background()
	f := newKube()
	k := Kubernetes{API: f}
	dep := Workload{ID: "deployment/media/web"}
	restore, err := k.Stop(ctx, dep, 0)
	if err != nil || string(restore) != `{"replicas":3}` {
		t.Fatalf("stop = %s, %v", restore, err)
	}
	if err := k.Start(ctx, dep, restore); err != nil {
		t.Fatal(err)
	}
	if err := k.Start(ctx, Workload{ID: "statefulset/media/db"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"daemonset/media/logs", "cronjob/media/backup"} {
		if _, err := k.Stop(ctx, Workload{ID: id}, 0); err != nil {
			t.Fatal(err)
		}
		if err := k.Start(ctx, Workload{ID: id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`deployment/media/web/scale {"spec":{"replicas":0}}`,
		`deployment/media/web/scale {"spec":{"replicas":3}}`,
		`statefulset/media/db/scale {"spec":{"replicas":1}}`,
		`daemonset/media/logs {"spec":{"template":{"spec":{"nodeSelector":{"drawbridge.io/stopped":"true"}}}}}`,
		`daemonset/media/logs {"spec":{"template":{"spec":{"nodeSelector":{"drawbridge.io/stopped":null}}}}}`,
		`cronjob/media/backup {"spec":{"suspend":true}}`,
		`cronjob/media/backup {"spec":{"suspend":false}}`,
	}
	if !reflect.DeepEqual(f.patches, want) {
		t.Errorf("patches\n got %v\nwant %v", f.patches, want)
	}
	for _, bad := range []string{"pod/media/x", "deployment/media", "deployment//x", "x"} {
		if _, err := k.Stop(ctx, Workload{ID: bad}, 0); err == nil {
			t.Errorf("stopped %q", bad)
		}
		if err := k.Start(ctx, Workload{ID: bad}, nil); err == nil {
			t.Errorf("started %q", bad)
		}
		if _, _, err := k.Inspect(ctx, Workload{ID: bad}); err == nil {
			t.Errorf("inspected %q", bad)
		}
	}
	if err := k.Start(ctx, dep, []byte("{")); err == nil {
		t.Error("bad restore data was accepted")
	}
}

func TestKubernetesInspect(t *testing.T) {
	ctx := context.Background()
	k := Kubernetes{API: newKube()}
	for id, want := range map[string]string{
		"deployment/media/web":  "true healthy",
		"statefulset/media/db":  "true starting", // 1 replica wanted, 0 ready
		"daemonset/media/logs":  "true starting", // 2 of 3 pods ready
		"cronjob/media/backup":  "true ",
		"deployment/media/idle": "false healthy",
	} {
		running, health, err := k.Inspect(ctx, Workload{ID: id})
		if got := strings.TrimSpace(strings.Join([]string{boolStr(running), health}, " ")); err != nil || got != strings.TrimSpace(want) {
			t.Errorf("%s = %q, %v; want %q", id, got, err, want)
		}
	}
	if _, _, err := k.Inspect(ctx, Workload{ID: "deployment/media/gone"}); err == nil {
		t.Error("inspected a missing deployment")
	}
	if _, _, ok, _ := k.Stats(ctx, Workload{ID: "deployment/media/web"}); ok {
		t.Error("Kubernetes workloads have no counters")
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
