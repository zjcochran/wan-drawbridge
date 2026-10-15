// SPDX-License-Identifier: AGPL-3.0-or-later

package kube

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAPI(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","message":"Unauthorized"}`)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c, err := New(srv.URL, ca, func() (string, error) { return "sa-token", nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// workloadAPI serves a few workloads; patches are recorded in patched.
func workloadAPI(t *testing.T, patched *[]string) *Client {
	t.Helper()
	return newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1/deployments":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"web","namespace":"media","labels":{"a":"b"}},"spec":{"replicas":2},"status":{"readyReplicas":1}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/batch/v1/namespaces/media/cronjobs":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"backup","namespace":"media"},"spec":{"suspend":true}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1/namespaces/media/deployments/web":
			fmt.Fprint(w, `{"metadata":{"name":"web","namespace":"media"},"spec":{"replicas":2}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1/namespaces/media/deployments/web/scale":
			fmt.Fprint(w, `{"spec":{"replicas":2}}`)
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			*patched = append(*patched, r.URL.Path+" "+r.Header.Get("Content-Type")+" "+string(b))
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"kind":"Status","message":"deployments.apps is forbidden"}`)
		}
	})
}

func TestListAndGet(t *testing.T) {
	ctx := context.Background()
	c := workloadAPI(t, &[]string{})
	ds, err := c.List(ctx, Deployments, "")
	if err != nil || len(ds) != 1 || ds[0].Metadata.Name != "web" || *ds[0].Spec.Replicas != 2 || ds[0].Status.ReadyReplicas != 1 {
		t.Fatalf("List = %+v, %v", ds, err)
	}
	cj, err := c.List(ctx, CronJobs, "media")
	if err != nil || len(cj) != 1 || !*cj[0].Spec.Suspend {
		t.Fatalf("cronjobs = %+v, %v", cj, err)
	}
	if o, err := c.Get(ctx, Deployments, "media", "web"); err != nil || o.Metadata.Namespace != "media" {
		t.Fatalf("Get = %+v, %v", o, err)
	}
	if n, err := c.Scale(ctx, Deployments, "media", "web"); err != nil || n != 2 {
		t.Fatalf("Scale = %d, %v", n, err)
	}
	if _, err := c.List(ctx, StatefulSets, "other"); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("a forbidden list: %v", err)
	}
}

func TestPatch(t *testing.T) {
	ctx := context.Background()
	var patched []string
	c := workloadAPI(t, &patched)
	if err := c.Patch(ctx, Deployments, "media", "web", "scale", map[string]any{"spec": map[string]any{"replicas": 0}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(ctx, CronJobs, "media", "backup", "", map[string]any{"spec": map[string]any{"suspend": true}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`/apis/apps/v1/namespaces/media/deployments/web/scale application/merge-patch+json {"spec":{"replicas":0}}`,
		`/apis/batch/v1/namespaces/media/cronjobs/backup application/merge-patch+json {"spec":{"suspend":true}}`,
	}
	if strings.Join(patched, "\n") != strings.Join(want, "\n") {
		t.Errorf("patches\n got %v\nwant %v", patched, want)
	}
	if err := c.Patch(ctx, Deployments, "media", "web", "", make(chan int)); err == nil {
		t.Error("a patch that cannot be encoded was sent")
	}
}

func TestClientSetup(t *testing.T) {
	if _, err := New("https://x", []byte("not a certificate"), nil); err == nil {
		t.Error("a bad CA was accepted")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := InCluster(); err == nil || !strings.Contains(err.Error(), "not running in a Kubernetes pod") {
		t.Errorf("outside a pod: %v", err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "192.0.2.10")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if _, err := InCluster(); err == nil {
		t.Error("in-cluster without the service account files")
	}
	c := newAPI(t, func(http.ResponseWriter, *http.Request) {})
	c.token = func() (string, error) { return "", errors.New("no token file") }
	if _, err := c.List(context.Background(), Deployments, ""); err == nil || !strings.Contains(err.Error(), "service account token") {
		t.Errorf("without a token: %v", err)
	}
	c.token = func() (string, error) { return "wrong", nil }
	if _, err := c.List(context.Background(), Deployments, ""); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("with a wrong token: %v", err)
	}
}
