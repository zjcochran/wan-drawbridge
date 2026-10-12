// SPDX-License-Identifier: AGPL-3.0-or-later

package asn

import (
	"net/netip"
	"strings"
	"testing"
)

const sample = "1.0.0.0\t1.0.0.255\t13335\tUS\tCLOUDFLARENET\n" +
	"192.0.2.0\t192.0.2.255\t0\tNone\tNot routed\n" +
	"203.0.113.0\t203.0.113.255\t64500\tUS\tEXAMPLENET-64500\n" +
	"2001:db8::\t2001:db8::ffff\t64512\tZZ\tV6-ONLY\n" +
	"malformed line\n"

func TestParseAndLookup(t *testing.T) {
	db, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if db.Len() != 2 {
		t.Fatalf("expected 2 routed IPv4 ranges, got %d", db.Len())
	}
	cases := map[string]int{
		"203.0.113.56": 64500,
		"1.0.0.1":      13335,
		"192.0.2.3":    0, // not routed
		"8.8.8.8":      0, // not in table
	}
	for ip, want := range cases {
		info, ok := db.Lookup(netip.MustParseAddr(ip))
		if (want == 0 && ok) || info.ASN != want {
			t.Errorf("Lookup(%s) = %+v, %v; want ASN %d", ip, info, ok, want)
		}
	}
}

func TestParseOrg(t *testing.T) {
	info, err := ParseOrg("AS64500 Examplenet Communications, LLC")
	if err != nil || info.ASN != 64500 || info.Name != "Examplenet Communications, LLC" {
		t.Fatalf("got %+v, %v", info, err)
	}
	for _, bad := range []string{"", "Examplenet", "ASabc Foo"} {
		if _, err := ParseOrg(bad); err == nil {
			t.Errorf("ParseOrg(%q) should fail", bad)
		}
	}
}
