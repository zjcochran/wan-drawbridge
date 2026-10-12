// SPDX-License-Identifier: AGPL-3.0-or-later

package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseBody(t *testing.T) {
	cases := map[string]string{
		"203.0.113.7\n": "203.0.113.7",
		"fl=1\nh=1.1.1.1\nip=198.51.100.4\nts=1\n": "198.51.100.4",
	}
	for body, want := range cases {
		got, err := ParseBody(body)
		if err != nil || got.String() != want {
			t.Errorf("ParseBody(%q) = %v, %v; want %s", body, got, err, want)
		}
	}
	for _, bad := range []string{"", "<html>", "2001:db8::1"} {
		if _, err := ParseBody(bad); err == nil {
			t.Errorf("ParseBody(%q) should fail", bad)
		}
	}
}

func TestAgree(t *testing.T) {
	rs := []ProviderResult{{IP: "203.0.113.7"}, {IP: "203.0.113.7"}, {Error: "timeout"}}
	if ip, ok := Agree(rs, 2); !ok || ip.String() != "203.0.113.7" {
		t.Fatalf("expected agreement, got %v %v", ip, ok)
	}
	rs = []ProviderResult{{IP: "203.0.113.7"}, {IP: "198.51.100.4"}, {Error: "timeout"}}
	if _, ok := Agree(rs, 2); ok {
		t.Fatal("split answers must not reach quorum")
	}
}

func TestProbeQuorum(t *testing.T) {
	serve := func(body string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}))
	}
	a := serve("203.0.113.7", 200)
	b := serve("ip=203.0.113.7\n", 200)
	c := serve("", 500)
	defer a.Close()
	defer b.Close()
	defer c.Close()

	p := New([]string{a.URL, b.URL, c.URL}, 2, 2*time.Second)
	res := p.Probe(context.Background())
	if !res.Conclusive || !res.Clean || res.IP.String() != "203.0.113.7" {
		t.Fatalf("expected clean 203.0.113.7, got %+v", res)
	}
	if res.Providers[2].Error == "" {
		t.Fatal("expected failing provider to record an error")
	}

	p = New([]string{a.URL, c.URL}, 2, 2*time.Second)
	if res := p.Probe(context.Background()); res.Conclusive {
		t.Fatalf("one answer must not satisfy a quorum of 2: %+v", res)
	}

	other := serve("198.51.100.4", 200)
	defer other.Close()
	p = New([]string{a.URL, b.URL, other.URL}, 2, 2*time.Second)
	if res := p.Probe(context.Background()); !res.Conclusive || res.Clean {
		t.Fatalf("2 of 3 agreeing with one dissent must be conclusive but not clean: %+v", res)
	}
}
