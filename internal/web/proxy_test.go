// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func proxyServer(t *testing.T, trusted ...string) *Server {
	t.Helper()
	p, err := ParseProxies(trusted)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{TrustedProxies: p}
}

func req(remote string, headers ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Add(headers[i], headers[i+1])
	}
	return r
}

func TestClientIP(t *testing.T) {
	s := proxyServer(t, "192.0.2.0/24", "192.0.2.1")
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"direct, header ignored", req("203.0.113.9:5000", "X-Forwarded-For", "6.6.6.6"), "203.0.113.9"},
		{"through proxy", req("192.0.2.2:5000", "X-Forwarded-For", "198.51.100.7"), "198.51.100.7"},
		{"spoof attempt: rightmost untrusted wins", req("192.0.2.2:5000", "X-Forwarded-For", "6.6.6.6, 198.51.100.7"), "198.51.100.7"},
		{"two proxies", req("192.0.2.1:5000", "X-Forwarded-For", "198.51.100.7, 192.0.2.3"), "198.51.100.7"},
		{"X-Real-IP fallback", req("192.0.2.2:5000", "X-Real-IP", "198.51.100.8"), "198.51.100.8"},
		{"proxy without headers", req("192.0.2.2:5000"), "192.0.2.2"},
		{"garbage header", req("192.0.2.2:5000", "X-Forwarded-For", "not-an-ip"), "192.0.2.2"},
	}
	for _, c := range cases {
		if got := s.clientIP(c.r); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestSecureOnlyFromTrustedProxy(t *testing.T) {
	s := proxyServer(t, "192.0.2.0/24")
	if s.secure(req("203.0.113.9:1", "X-Forwarded-Proto", "https")) {
		t.Error("believed X-Forwarded-Proto from an untrusted client")
	}
	if !s.secure(req("192.0.2.2:1", "X-Forwarded-Proto", "https")) {
		t.Error("ignored X-Forwarded-Proto from a trusted proxy")
	}
	s.BaseURL = "https://drawbridge.example"
	if !s.secure(req("203.0.113.9:1")) {
		t.Error("https base URL should mark requests secure")
	}
}

func TestParseProxiesRejectsJunk(t *testing.T) {
	if _, err := ParseProxies([]string{"192.0.2.0/8", "::1", "nope"}); err == nil {
		t.Fatal("accepted junk")
	}
}

// Behind a proxy, one attacker hitting the limit must not lock out other users.
func TestRateLimitPerRealClientBehindProxy(t *testing.T) {
	e := newEnv(t)
	e.admin()
	p, _ := ParseProxies([]string{"127.0.0.1"})
	srv := e.server()
	srv.TrustedProxies = p

	attacker := e.client()
	for range 5 {
		attacker.do("POST", "/login", url.Values{"username": {"nobody"}, "password": {"wrong password!!"}},
			"X-Forwarded-For", "198.51.100.66")
	}
	if resp, _ := attacker.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"test-password-123"}},
		"X-Forwarded-For", "198.51.100.66"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attacker not limited: %d", resp.StatusCode)
	}
	other := e.client()
	resp, _ := other.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"test-password-123"}},
		"X-Forwarded-For", "198.51.100.10", "X-Forwarded-Proto", "https")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("another client was locked out: %d", resp.StatusCode)
	}
	if c := resp.Header.Get("Set-Cookie"); !strings.Contains(c, "Secure") {
		t.Errorf("cookie not Secure behind an https proxy: %s", c)
	}
}
