// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseProxies parses DRAWBRIDGE_TRUSTED_PROXIES: IPs or CIDR ranges.
func ParseProxies(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q is not an IP or CIDR range", s)
		}
		out = append(out, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
	}
	return out, nil
}

func (s *Server) trusted(ip netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

// remoteAddr is the address of the direct peer.
func remoteAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	return ip.Unmap(), err == nil
}

// fromProxy reports whether the request came through a trusted proxy, whose
// forwarding headers can be believed.
func (s *Server) fromProxy(r *http.Request) bool {
	ip, ok := remoteAddr(r)
	return ok && s.trusted(ip)
}

// clientIP is the visitor's address. Behind trusted proxies it walks
// X-Forwarded-For from the right, skipping trusted hops, so a client cannot
// spoof it by sending its own header. Otherwise it is the direct peer.
func (s *Server) clientIP(r *http.Request) string {
	peer, ok := remoteAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	if !s.trusted(peer) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !s.trusted(ip) {
			return ip.Unmap().String()
		}
	}
	if ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return ip.Unmap().String()
	}
	return peer.String()
}

// secure reports whether the browser reached us over HTTPS, directly, through
// a trusted proxy, or per the configured external URL.
func (s *Server) secure(r *http.Request) bool {
	if r.TLS != nil || strings.HasPrefix(s.BaseURL, "https://") {
		return true
	}
	return s.fromProxy(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
