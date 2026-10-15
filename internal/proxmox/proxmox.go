// SPDX-License-Identifier: AGPL-3.0-or-later

// Package proxmox is a minimal Proxmox VE API client: list guests, stop and
// start LXC containers (directly or through HA), and wait for their tasks.
// It authenticates with an API token and pins the server's certificate by
// its SHA-256 fingerprint, since Proxmox certificates are usually
// self-signed.
package proxmox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Proxmox VE cluster.
type Client struct {
	base  string // https://host:8006/api2/json
	token string // the Authorization header value
	http  *http.Client
	// PollEvery is how often tasks and HA changes are checked.
	PollEvery time.Duration
}

// Fingerprint formats a certificate's SHA-256 the way Proxmox shows it:
// upper-case hex pairs separated by colons.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(pairs, ":")
}

// normalize makes fingerprints comparable however they were written.
func normalize(fp string) string {
	return strings.ToUpper(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(fp)))
}

// ErrNoFingerprint is returned by New without a fingerprint; its message
// carries the fingerprint the server presented.
var ErrNoFingerprint = errors.New("PROXMOX_FINGERPRINT is not set")

// New returns a client for the API at rawURL (for example
// https://pve.example.com:8006). tokenID is "user@realm!name". The server's
// certificate must have the SHA-256 fingerprint fp.
func New(rawURL, tokenID, secret, fp string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("PROXMOX_URL must be https://<host>:8006")
	}
	if !strings.Contains(tokenID, "!") || !strings.Contains(tokenID, "@") || secret == "" {
		return nil, errors.New("PROXMOX_TOKEN_ID must be user@realm!name, with PROXMOX_TOKEN_SECRET")
	}
	want := normalize(fp)
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The certificate is checked by its fingerprint below instead of a CA,
		// on every connection, resumed ones included.
		InsecureSkipVerify: true, //nolint:gosec // G402: pinned by fingerprint in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("proxmox: no certificate")
			}
			got := Fingerprint(cs.PeerCertificates[0].Raw)
			if want == "" {
				return fmt.Errorf("%w; the server's certificate has %s: check it in the Proxmox web UI (node → System → Certificates) and set it", ErrNoFingerprint, got)
			}
			if normalize(got) != want {
				return fmt.Errorf("proxmox: the server's certificate fingerprint is %s, not PROXMOX_FINGERPRINT", got)
			}
			return nil
		},
	}}
	return &Client{
		base: u.String() + "/api2/json", token: "PVEAPIToken=" + tokenID + "=" + secret,
		http: &http.Client{Transport: tr, Timeout: 30 * time.Second}, PollEvery: 2 * time.Second,
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var nf *url.Error
		if errors.As(err, &nf) && errors.Is(nf.Err, ErrNoFingerprint) {
			return nf.Err
		}
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(resp.Status)
		var e struct{ Message string }
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			msg += ": " + strings.TrimSpace(e.Message)
		}
		return fmt.Errorf("proxmox: %s %s: %s", method, path, msg)
	}
	var env struct{ Data json.RawMessage }
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&env); err != nil {
		return fmt.Errorf("proxmox: %s %s: %w", method, path, err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// Guest is an LXC container or a VM of the cluster.
type Guest struct {
	ID     string `json:"id"` // "lxc/101"
	Type   string `json:"type"`
	Node   string `json:"node"`
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"` // "running", "stopped"
	Pool   string `json:"pool"`
	Tags   string `json:"tags"` // separated by ";"
	NetIn  int64  `json:"netin"`
	NetOut int64  `json:"netout"`
}

// TagList splits the guest's tags.
func (g Guest) TagList() []string {
	var out []string
	for _, t := range strings.FieldsFunc(g.Tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		out = append(out, strings.ToLower(t))
	}
	return out
}

// Guests lists the cluster's LXC containers.
func (c *Client) Guests(ctx context.Context) ([]Guest, error) {
	var all []Guest
	if err := c.do(ctx, http.MethodGet, "/cluster/resources?type=vm", nil, &all); err != nil {
		return nil, err
	}
	var out []Guest
	for _, g := range all {
		if g.Type == "lxc" {
			out = append(out, g)
		}
	}
	return out, nil
}

// StartupOrder reads a guest's start order from its "startup" option
// ("order=2,up=30"); 0 when none is set.
func (c *Client) StartupOrder(ctx context.Context, g Guest) (int, error) {
	var cfg struct {
		Startup string `json:"startup"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/lxc/%d/config", url.PathEscape(g.Node), g.VMID), nil, &cfg); err != nil {
		return 0, err
	}
	for _, part := range strings.Split(cfg.Startup, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "order="); ok {
			return strconv.Atoi(v)
		}
	}
	return 0, nil
}

// HAManaged lists the guests under HA, by "ct:<vmid>".
func (c *Client) HAManaged(ctx context.Context) (map[string]bool, error) {
	var rs []struct {
		SID string `json:"sid"`
	}
	if err := c.do(ctx, http.MethodGet, "/cluster/ha/resources", nil, &rs); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rs {
		out[r.SID] = true
	}
	return out, nil
}

// Shutdown asks a container to shut down, stopping it after timeout, and
// waits for the task.
func (c *Client) Shutdown(ctx context.Context, g Guest, timeout time.Duration) error {
	var upid string
	form := url.Values{"timeout": {strconv.Itoa(int(timeout.Seconds()))}, "forceStop": {"1"}}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/lxc/%d/status/shutdown", url.PathEscape(g.Node), g.VMID), form, &upid); err != nil {
		return err
	}
	return c.wait(ctx, g.Node, upid)
}

// Start starts a container and waits for the task.
func (c *Client) Start(ctx context.Context, g Guest) error {
	var upid string
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/lxc/%d/status/start", url.PathEscape(g.Node), g.VMID), url.Values{}, &upid); err != nil {
		return err
	}
	return c.wait(ctx, g.Node, upid)
}

// wait polls a task until it ends; it fails if the task did.
func (c *Client) wait(ctx context.Context, node, upid string) error {
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid)), nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("proxmox task %s: %s", upid, st.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollEvery):
		}
	}
}

// SetHAState asks HA to have a container "stopped" or "started", then waits
// until its status matches, for up to wait.
func (c *Client) SetHAState(ctx context.Context, g Guest, state string, wait time.Duration) error {
	if err := c.do(ctx, http.MethodPut, "/cluster/ha/resources/"+url.PathEscape(fmt.Sprintf("ct:%d", g.VMID)),
		url.Values{"state": {state}}, nil); err != nil {
		return err
	}
	want := map[string]string{"stopped": "stopped", "started": "running"}[state]
	deadline := time.Now().Add(wait)
	for {
		gs, err := c.Guests(ctx)
		if err != nil {
			return err
		}
		for _, x := range gs {
			if x.VMID == g.VMID && x.Status == want {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("HA did not make container %d %s within %s", g.VMID, want, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.PollEvery):
		}
	}
}
