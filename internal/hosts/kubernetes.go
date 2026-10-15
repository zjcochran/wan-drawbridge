// SPDX-License-Identifier: AGPL-3.0-or-later

package hosts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/kube"
)

// Kubernetes labels and selectors Drawbridge uses.
const (
	// LabelKubeSelf marks the agent's own workload, which is never listed.
	LabelKubeSelf = "drawbridge.io/self"
	// KubeStoppedSelector is the node selector that takes a DaemonSet's pods
	// off every node: no node has this label.
	KubeStoppedSelector = "drawbridge.io/stopped"
)

// kubeKinds are the workload kinds, in listing order.
var kubeKinds = []kube.Resource{kube.Deployments, kube.StatefulSets, kube.DaemonSets, kube.CronJobs}

// KubeAPI is what the Kubernetes runtime needs from the API.
type KubeAPI interface {
	List(ctx context.Context, r kube.Resource, ns string) ([]kube.Object, error)
	Get(ctx context.Context, r kube.Resource, ns, name string) (kube.Object, error)
	Patch(ctx context.Context, r kube.Resource, ns, name, sub string, patch any) error
	Scale(ctx context.Context, r kube.Resource, ns, name string) (int64, error)
}

// Kubernetes is the runtime of a cluster's workloads: Deployments,
// StatefulSets, DaemonSets and CronJobs, grouped by namespace. Deployments
// and StatefulSets are scaled to 0; a DaemonSet gets a node selector no node
// matches; a CronJob is suspended.
type Kubernetes struct {
	API        KubeAPI
	Namespaces []string // the namespaces to control; empty: all
}

func (k Kubernetes) namespaces() []string {
	if len(k.Namespaces) == 0 {
		return []string{""}
	}
	return k.Namespaces
}

// kubeID is a workload's ID: "<kind>/<namespace>/<name>".
func kubeID(r kube.Resource, o kube.Object) string {
	return r.Kind + "/" + o.Metadata.Namespace + "/" + o.Metadata.Name
}

// parseKubeID splits an ID into its resource, namespace and name.
func parseKubeID(id string) (kube.Resource, string, string, error) {
	kind, rest, _ := strings.Cut(id, "/")
	ns, name, ok := strings.Cut(rest, "/")
	for _, r := range kubeKinds {
		if r.Kind == kind && ok && ns != "" && name != "" {
			return r, ns, name, nil
		}
	}
	return kube.Resource{}, "", "", fmt.Errorf("not a Kubernetes workload ID: %q", id)
}

// List returns the workloads of the namespaces, with warnings for those an
// autoscaler or a GitOps tool would change back.
func (k Kubernetes) List(ctx context.Context) ([]Workload, error) {
	var out []Workload
	for _, ns := range k.namespaces() {
		hpas, err := k.API.List(ctx, kube.HPAs, ns)
		if err != nil {
			return nil, err
		}
		scaled := map[string]string{} // "<kind>/<namespace>/<name>" → HPA name
		for _, h := range hpas {
			t := h.Spec.ScaleTargetRef
			scaled[strings.ToLower(t.Kind)+"/"+h.Metadata.Namespace+"/"+t.Name] = h.Metadata.Name
		}
		for _, r := range kubeKinds {
			objs, err := k.API.List(ctx, r, ns)
			if err != nil {
				return nil, err
			}
			for _, o := range objs {
				if o.Metadata.Labels[LabelKubeSelf] == "true" {
					continue
				}
				w := fromKube(r, o)
				if hpa := scaled[w.ID]; hpa != "" {
					w.Warnings = append(w.Warnings, "the HorizontalPodAutoscaler "+hpa+" scales it back up")
				}
				out = append(out, w)
			}
		}
	}
	return out, nil
}

// replicas is a Deployment's or StatefulSet's wanted replicas; 1 when unset.
func replicas(o kube.Object) int64 {
	if o.Spec.Replicas == nil {
		return 1
	}
	return *o.Spec.Replicas
}

// kubeRunning reports whether a workload is wanted to run.
func kubeRunning(r kube.Resource, o kube.Object) bool {
	switch r {
	case kube.DaemonSets:
		return o.Spec.Template.Spec.NodeSelector[KubeStoppedSelector] != "true" && o.Status.DesiredNumberScheduled > 0
	case kube.CronJobs:
		return o.Spec.Suspend == nil || !*o.Spec.Suspend
	}
	return replicas(o) > 0
}

func fromKube(r kube.Resource, o kube.Object) Workload {
	labels := map[string]string{}
	for k, v := range o.Metadata.Annotations {
		labels[k] = v
	}
	for k, v := range o.Metadata.Labels {
		labels[k] = v
	}
	w := Workload{
		ID: kubeID(r, o), Name: o.Metadata.Name, Kind: r.Kind, Group: o.Metadata.Namespace,
		Member: r.Kind + "/" + o.Metadata.Name, Running: kubeRunning(r, o),
		Label: docker.Container{Labels: labels}.OnFailover(),
	}
	switch {
	case labels["argocd.argoproj.io/instance"] != "" || labels["argocd.argoproj.io/tracking-id"] != "":
		w.Warnings = append(w.Warnings, "managed by Argo CD, which undoes the change unless the application ignores it")
	case labels["kustomize.toolkit.fluxcd.io/name"] != "" || labels["helm.toolkit.fluxcd.io/name"] != "":
		w.Warnings = append(w.Warnings, "managed by Flux, which undoes the change unless its Kustomization or HelmRelease is suspended")
	}
	return w
}

// Inspect reports whether a workload runs: Deployments and StatefulSets are
// healthy once all replicas are ready, DaemonSets once every scheduled pod
// is; CronJobs have no health.
func (k Kubernetes) Inspect(ctx context.Context, w Workload) (bool, string, error) {
	r, ns, name, err := parseKubeID(w.ID)
	if err != nil {
		return false, "", err
	}
	o, err := k.API.Get(ctx, r, ns, name)
	if err != nil {
		return false, "", err
	}
	running := kubeRunning(r, o)
	ready := true
	switch r {
	case kube.Deployments, kube.StatefulSets:
		ready = o.Status.ReadyReplicas >= replicas(o)
	case kube.DaemonSets:
		ready = o.Status.NumberReady >= o.Status.DesiredNumberScheduled
	case kube.CronJobs:
		return running, "", nil
	}
	if !ready {
		return running, "starting", nil
	}
	return running, "healthy", nil
}

type kubeRestore struct {
	Replicas *int64 `json:"replicas,omitempty"`
}

// Stop scales a Deployment or StatefulSet to 0, keeps a DaemonSet's pods off
// every node, or suspends a CronJob.
func (k Kubernetes) Stop(ctx context.Context, w Workload, _ time.Duration) ([]byte, error) {
	r, ns, name, err := parseKubeID(w.ID)
	if err != nil {
		return nil, err
	}
	switch r {
	case kube.DaemonSets:
		return nil, k.API.Patch(ctx, r, ns, name, "", map[string]any{"spec": map[string]any{"template": map[string]any{
			"spec": map[string]any{"nodeSelector": map[string]any{KubeStoppedSelector: "true"}}}}})
	case kube.CronJobs:
		return nil, k.API.Patch(ctx, r, ns, name, "", map[string]any{"spec": map[string]any{"suspend": true}})
	}
	n, err := k.API.Scale(ctx, r, ns, name)
	if err != nil {
		return nil, err
	}
	if err := k.API.Patch(ctx, r, ns, name, "scale", map[string]any{"spec": map[string]any{"replicas": 0}}); err != nil {
		return nil, err
	}
	return json.Marshal(kubeRestore{Replicas: &n})
}

// Start undoes Stop. A Deployment or StatefulSet gets back the replicas from
// restore, or 1 without it.
func (k Kubernetes) Start(ctx context.Context, w Workload, restore []byte) error {
	r, ns, name, err := parseKubeID(w.ID)
	if err != nil {
		return err
	}
	switch r {
	case kube.DaemonSets:
		return k.API.Patch(ctx, r, ns, name, "", map[string]any{"spec": map[string]any{"template": map[string]any{
			"spec": map[string]any{"nodeSelector": map[string]any{KubeStoppedSelector: nil}}}}})
	case kube.CronJobs:
		return k.API.Patch(ctx, r, ns, name, "", map[string]any{"spec": map[string]any{"suspend": false}})
	}
	var kr kubeRestore
	if len(restore) > 0 {
		if err := json.Unmarshal(restore, &kr); err != nil {
			return fmt.Errorf("restore data: %w", err)
		}
	}
	n := int64(1)
	if kr.Replicas != nil {
		n = *kr.Replicas
	}
	return k.API.Patch(ctx, r, ns, name, "scale", map[string]any{"spec": map[string]any{"replicas": n}})
}

// Stats is not available: reading pods' network counters needs node proxy
// access, which is far more than Drawbridge should have.
func (k Kubernetes) Stats(context.Context, Workload) (int64, int64, bool, error) {
	return 0, 0, false, nil
}
