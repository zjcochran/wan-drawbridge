// SPDX-License-Identifier: AGPL-3.0-or-later

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// swarmEngine is a fake engine for the swarm calls; updates are recorded in
// updated as "<version> <spec JSON>".
func swarmEngine(t *testing.T, updated *[]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/info":
			fmt.Fprint(w, `{"Swarm":{"NodeID":"node1","LocalNodeState":"active","ControlAvailable":true}}`)
		case r.URL.Path == "/services" && r.URL.Query().Get("status") == "true":
			fmt.Fprint(w, `[
				{"ID":"s1","Spec":{"Name":"media_db","Labels":{"com.docker.stack.namespace":"media"},"Mode":{"Replicated":{"Replicas":2}}},
				 "ServiceStatus":{"RunningTasks":2,"DesiredTasks":2}},
				{"ID":"s2","Spec":{"Name":"agent","Mode":{"Global":{}}},"ServiceStatus":{"RunningTasks":3,"DesiredTasks":3}}]`)
		case r.URL.Path == "/services/s1" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"Version":{"Index":42},"Spec":{"Name":"media_db","Mode":{"Replicated":{"Replicas":2}},"TaskTemplate":{"ContainerSpec":{"Image":"db"}}}}`)
		case r.URL.Path == "/services/s1/update" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			*updated = append(*updated, r.URL.Query().Get("version")+" "+string(b))
			fmt.Fprint(w, `{"Warnings":null}`)
		case r.URL.Path == "/services/s9/update":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"update out of sequence"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSwarmServices(t *testing.T) {
	c := swarmEngine(t, &[]string{})
	ctx := context.Background()
	info, err := c.Swarm(ctx)
	if err != nil || info != (SwarmInfo{NodeID: "node1", State: "active", Manager: true}) {
		t.Fatalf("Swarm = %+v, %v", info, err)
	}
	ss, err := c.Services(ctx)
	if err != nil || len(ss) != 2 {
		t.Fatalf("Services = %+v, %v", ss, err)
	}
	if s := ss[0]; s.ID != "s1" || s.Name != "media_db" || s.Labels[LabelStack] != "media" || s.Global || s.Replicas != 2 || s.Running != 2 || s.Desired != 2 {
		t.Errorf("replicated service = %+v", s)
	}
	if s := ss[1]; !s.Global || s.Replicas != 0 || s.Running != 3 {
		t.Errorf("global service = %+v", s)
	}
}

func TestServiceUpdate(t *testing.T) {
	var updated []string
	c := swarmEngine(t, &updated)
	ctx := context.Background()

	version, spec, err := c.ServiceSpec(ctx, "s1")
	if err != nil || version != 42 || spec["Name"] != "media_db" {
		t.Fatalf("ServiceSpec = %d %v %v", version, spec, err)
	}
	spec["Mode"] = map[string]any{"Replicated": map[string]any{"Replicas": 0}}
	if err := c.UpdateService(ctx, "s1", version, spec); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if len(updated) != 1 || !strings.HasPrefix(updated[0], "42 ") || json.Unmarshal([]byte(updated[0][3:]), &sent) != nil ||
		sent["TaskTemplate"] == nil {
		t.Errorf("update = %v", updated)
	}
	if err := c.UpdateService(ctx, "s9", 1, spec); err == nil || !strings.Contains(err.Error(), "out of sequence") {
		t.Errorf("a refused update: %v", err)
	}
	if err := c.UpdateService(ctx, "s1", 1, map[string]any{"bad": make(chan int)}); err == nil {
		t.Error("a spec that cannot be encoded was sent")
	}
	if _, _, err := c.ServiceSpec(ctx, "nope"); err == nil {
		t.Error("an unknown service was read")
	}
}
