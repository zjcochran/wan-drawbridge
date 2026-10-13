// SPDX-License-Identifier: AGPL-3.0-or-later

// Package probe finds the public IPv4 address by asking several providers in
// parallel and accepting an answer only when enough of them agree.
package probe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DefaultProviders are used when none are configured.
var DefaultProviders = []string{
	"https://1.1.1.1/cdn-cgi/trace",
	"https://api.ipify.org",
	"https://icanhazip.com",
}

// ProviderResult is one provider's answer, kept for the check record.
type ProviderResult struct {
	URL       string `json:"url"`
	IP        string `json:"ip,omitempty"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

// Result is the outcome of one probe.
type Result struct {
	IP         netip.Addr // valid only when Conclusive
	Conclusive bool
	// Clean is Conclusive with no provider reporting a different address.
	// Providers that failed outright do not spoil it, so one provider being
	// down does not block failback forever.
	Clean     bool
	Providers []ProviderResult
}

// Prober queries IP providers over IPv4 only, so providers cannot disagree
// just because one answered over IPv6.
type Prober struct {
	Providers []string
	Quorum    int
	client    *http.Client
}

// New returns a Prober that asks the providers over IPv4, on a fresh
// connection each time, and needs quorum of them to agree.
func New(providers []string, quorum int, timeout time.Duration) *Prober {
	dialer := &net.Dialer{Timeout: timeout}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		// Never reuse connections: a WAN switch must not ride an old socket.
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
	}
	if len(providers) == 0 {
		providers = DefaultProviders
	}
	return &Prober{
		Providers: providers,
		Quorum:    quorum,
		client:    &http.Client{Transport: tr, Timeout: timeout},
	}
}

// Probe queries every provider in parallel and applies the quorum.
func (p *Prober) Probe(ctx context.Context) Result {
	results := make([]ProviderResult, len(p.Providers))
	var wg sync.WaitGroup
	for i, url := range p.Providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.query(ctx, url)
		}()
	}
	wg.Wait()
	ip, ok := Agree(results, p.Quorum)
	clean := ok
	for _, r := range results {
		if r.IP != "" && r.IP != ip.String() {
			clean = false
		}
	}
	return Result{IP: ip, Conclusive: ok, Clean: clean, Providers: results}
}

func (p *Prober) query(ctx context.Context, url string) ProviderResult {
	start := time.Now()
	res := ProviderResult{URL: url}
	ip, err := p.fetch(ctx, url)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
	} else {
		res.IP = ip.String()
	}
	return res
}

func (p *Prober) fetch(ctx context.Context, url string) (netip.Addr, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	req.Header.Set("User-Agent", "wan-drawbridge")
	resp, err := p.client.Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return netip.Addr{}, err
	}
	return ParseBody(string(body))
}

// ParseBody accepts either a bare IP or a Cloudflare trace body ("ip=...").
func ParseBody(body string) (netip.Addr, error) {
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "ip="); ok {
			return parseIPv4(v)
		}
	}
	return parseIPv4(strings.TrimSpace(body))
}

func parseIPv4(s string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("not an IP: %q", truncate(s, 64))
	}
	ip = ip.Unmap()
	if !ip.Is4() {
		return netip.Addr{}, fmt.Errorf("not IPv4: %s", ip)
	}
	return ip, nil
}

// Agree returns the IP reported by at least quorum providers.
func Agree(results []ProviderResult, quorum int) (netip.Addr, bool) {
	counts := map[string]int{}
	for _, r := range results {
		if r.IP != "" {
			counts[r.IP]++
		}
	}
	for s, n := range counts {
		if n >= quorum {
			return netip.MustParseAddr(s), true
		}
	}
	return netip.Addr{}, false
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
