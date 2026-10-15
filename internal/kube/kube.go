// SPDX-License-Identifier: AGPL-3.0-or-later

// Package kube is a minimal Kubernetes API client for the Drawbridge agent:
// list and patch Deployments, StatefulSets, DaemonSets and CronJobs, scale
// through the scale subresource, and list HorizontalPodAutoscalers. It uses
// the in-cluster ServiceAccount.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// In-cluster ServiceAccount files.
const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Client talks to one cluster's API server.
type Client struct {
	base  string
	token func() (string, error) // read for every request: the kubelet rotates it
	http  *http.Client
}

// New returns a client for the API at base, trusting caPEM.
func New(base string, caPEM []byte, token func() (string, error)) (*Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kube: no CA certificate")
	}
	return &Client{
		base: strings.TrimRight(base, "/"), token: token,
		http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}},
	}, nil
}

// InCluster returns a client for the cluster the agent's pod runs in.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST is not set)")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("kube: %w", err)
	}
	return New("https://"+net.JoinHostPort(host, port), ca, func() (string, error) {
		b, err := os.ReadFile(saDir + "/token")
		return strings.TrimSpace(string(b)), err
	})
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	token, err := c.token()
	if err != nil {
		return fmt.Errorf("kube: service account token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		var st struct{ Message string }
		msg := resp.Status
		if json.Unmarshal(raw, &st) == nil && st.Message != "" {
			msg += ": " + st.Message
		}
		return fmt.Errorf("kube: %s %s: %s", method, path, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Resource names a kind of workload in the API.
type Resource struct {
	Kind   string // "deployment", ...
	Group  string // API group and version, e.g. "apps/v1"
	Plural string
}

// Resources Drawbridge controls.
var (
	Deployments  = Resource{"deployment", "apps/v1", "deployments"}
	StatefulSets = Resource{"statefulset", "apps/v1", "statefulsets"}
	DaemonSets   = Resource{"daemonset", "apps/v1", "daemonsets"}
	CronJobs     = Resource{"cronjob", "batch/v1", "cronjobs"}
	HPAs         = Resource{"horizontalpodautoscaler", "autoscaling/v2", "horizontalpodautoscalers"}
)

// path is the collection's path, in one namespace or ("" ) all of them.
func (r Resource) path(ns string) string {
	p := "/apis/" + r.Group
	if ns != "" {
		p += "/namespaces/" + url.PathEscape(ns)
	}
	return p + "/" + r.Plural
}

// Object is the part of a workload Drawbridge reads.
type Object struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		UID         string            `json:"uid"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int64 `json:"replicas"`
		Suspend  *bool  `json:"suspend"` // CronJobs
		Template struct {
			Spec struct {
				NodeSelector map[string]string `json:"nodeSelector"`
			} `json:"spec"`
		} `json:"template"`
		ScaleTargetRef struct { // HPAs
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"scaleTargetRef"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas          int64 `json:"readyReplicas"`
		DesiredNumberScheduled int64 `json:"desiredNumberScheduled"`
		NumberReady            int64 `json:"numberReady"`
	} `json:"status"`
}

// List lists a resource in a namespace, or in all namespaces when ns is "".
func (c *Client) List(ctx context.Context, r Resource, ns string) ([]Object, error) {
	var list struct{ Items []Object }
	err := c.do(ctx, http.MethodGet, r.path(ns), "", nil, &list)
	return list.Items, err
}

// Get reads one object.
func (c *Client) Get(ctx context.Context, r Resource, ns, name string) (Object, error) {
	var o Object
	err := c.do(ctx, http.MethodGet, r.path(ns)+"/"+url.PathEscape(name), "", nil, &o)
	return o, err
}

// Patch applies a JSON merge patch to an object, or to its subresource
// ("scale") when sub is set.
func (c *Client) Patch(ctx context.Context, r Resource, ns, name, sub string, patch any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	p := r.path(ns) + "/" + url.PathEscape(name)
	if sub != "" {
		p += "/" + sub
	}
	return c.do(ctx, http.MethodPatch, p, "application/merge-patch+json", body, nil)
}

// Scale reads the replica count from an object's scale subresource.
func (c *Client) Scale(ctx context.Context, r Resource, ns, name string) (int64, error) {
	var s struct {
		Spec struct {
			Replicas int64 `json:"replicas"`
		} `json:"spec"`
	}
	err := c.do(ctx, http.MethodGet, r.path(ns)+"/"+url.PathEscape(name)+"/scale", "", nil, &s)
	return s.Spec.Replicas, err
}
