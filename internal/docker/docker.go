// SPDX-License-Identifier: AGPL-3.0-or-later

// Package docker is a minimal Docker Engine API client: the five calls
// Drawbridge needs, over the mounted socket.
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	LabelProject   = "com.docker.compose.project"
	LabelService   = "com.docker.compose.service"
	LabelDependsOn = "com.docker.compose.depends_on"
	LabelSelf      = "drawbridge.self"
)

// Container is the part of a container Drawbridge cares about.
type Container struct {
	ID      string
	Name    string
	Project string // Compose project, "" for standalone containers
	Service string // Compose service, "" for standalone containers
	Running bool
	Labels  map[string]string
}

// Key identifies a container across re-creation: project/service for Compose
// containers, the container name otherwise.
func (c Container) Key() string {
	if c.Project != "" && c.Service != "" {
		return c.Project + "/" + c.Service
	}
	return c.Name
}

// DependsOn returns the Compose services this container depends on, read from
// "db:service_started:false,redis:service_healthy:true".
func (c Container) DependsOn() []string {
	var out []string
	for _, part := range strings.Split(c.Labels[LabelDependsOn], ",") {
		if svc, _, _ := strings.Cut(strings.TrimSpace(part), ":"); svc != "" {
			out = append(out, svc)
		}
	}
	return out
}

// Health is a container's healthcheck status: "" when it has no healthcheck,
// otherwise "starting", "healthy" or "unhealthy".
type Health string

type Client struct {
	http *http.Client
	base string
}

// New connects to host: "unix:///var/run/docker.sock" or "tcp://host:port".
func New(host string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host %q: %w", host, err)
	}
	tr := &http.Transport{}
	base := "http://docker"
	switch u.Scheme {
	case "unix":
		path := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
	case "tcp", "http":
		base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("docker host %q: unsupported scheme", host)
	}
	return &Client{http: &http.Client{Transport: tr}, base: base}, nil
}

// Ping checks the connection and returns the engine version.
func (c *Client) Ping(ctx context.Context) (string, error) {
	var v struct{ Version string }
	return v.Version, c.getJSON(ctx, "/version", &v)
}

// List returns all containers, running or not.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	var raw []struct {
		ID     string `json:"Id"`
		Names  []string
		State  string
		Labels map[string]string
	}
	if err := c.getJSON(ctx, "/containers/json?all=1", &raw); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		name := r.ID[:min(12, len(r.ID))]
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		out = append(out, Container{
			ID:      r.ID,
			Name:    name,
			Project: r.Labels[LabelProject],
			Service: r.Labels[LabelService],
			Running: r.State == "running" || r.State == "restarting",
			Labels:  r.Labels,
		})
	}
	return out, nil
}

// Inspect returns whether the container is running and its health.
func (c *Client) Inspect(ctx context.Context, id string) (running bool, health Health, err error) {
	var raw struct {
		State struct {
			Running bool
			Health  *struct{ Status string }
		}
	}
	if err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", &raw); err != nil {
		return false, "", err
	}
	if raw.State.Health != nil {
		health = Health(raw.State.Health.Status)
	}
	return raw.State.Running, health, nil
}

// Stop stops a container, waiting up to timeout before Docker kills it.
// Stopping an already stopped container is not an error.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	q := "?t=" + strconv.Itoa(int(timeout.Seconds()))
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/stop"+q)
}

// Start starts a container. Starting a running container is not an error.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/start")
}

// Events calls fn for every container event until ctx is done or the stream breaks.
func (c *Client) Events(ctx context.Context, fn func(action, id string)) error {
	filters := url.QueryEscape(`{"type":["container"]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/events?filters="+filters, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev struct {
			Action string
			Actor  struct{ ID string }
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			fn(ev.Action, ev.Actor.ID)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) post(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified: // done, or already in that state
		return nil
	}
	return apiError(resp)
}

func apiError(resp *http.Response) error {
	var body struct{ Message string }
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(b, &body) != nil || body.Message == "" {
		body.Message = strings.TrimSpace(string(b))
	}
	return fmt.Errorf("docker: %s: %s", resp.Status, body.Message)
}
