// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"web", "web", true},
		{"web", "web-1", false},
		{"WEB*", "web-1", true},
		{"*-init", "db-init", true},
		{"*-init", "db-init-2", false},
		{"backup-?", "backup-1", true},
		{"backup-?", "backup-12", false},
		{"*a*b*c", "xaybzc", true},
		{"*a*b*c", "xaybzcd", false},
		{"monitoring/*", "monitoring/agent", true},
		{"**", "", true},
		{"?", "", false},
		{"é?", "éa", true},
	} {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pattern, c.s, got)
		}
	}
}

func TestParsePatterns(t *testing.T) {
	h, err := parsePatterns(" *-init \n\nweb\r\n*-INIT\n")
	if err != nil || strings.Join(h, ",") != "*-init,web" {
		t.Fatalf("parsePatterns = %q, %v", h, err)
	}
	if _, err := parsePatterns(strings.Repeat("a", maxPatternLen+1)); err == nil {
		t.Error("a long pattern was accepted")
	}
	var many []string
	for i := range maxHidePatterns + 1 {
		many = append(many, strings.Repeat("x", i+1))
	}
	if _, err := parsePatterns(strings.Join(many, "\n")); err == nil {
		t.Error("too many patterns were accepted")
	}
}

func TestContainerFiltersHideContainers(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	if _, page := c.get("/containers"); strings.Contains(page, "Show hidden") {
		t.Error("Show hidden is offered without filters")
	}
	resp, body := c.htmx("/settings/container-filters", url.Values{"patterns": {"*-indexer-?\nd?s"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "d?s") {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	_, page := c.get("/containers")
	if strings.Contains(page, "media-indexer-1") || strings.Contains(page, ">dns<") || !strings.Contains(page, "media-player-1") {
		t.Error("the filters did not hide the matching containers")
	}
	if !strings.Contains(page, "2 hidden by") || !strings.Contains(page, "Show hidden") {
		t.Error("the page does not say containers are hidden")
	}
	_, page = c.get("/containers?hidden=1")
	if !strings.Contains(page, "media-indexer-1") || !strings.Contains(page, ">hidden</span>") {
		t.Error("Show hidden does not list the hidden containers")
	}
	if _, page := c.get("/settings/container-filters"); !strings.Contains(page, "*-indexer-?\nd?s") {
		t.Error("the tab does not show the saved patterns")
	}
}

func TestContainerFiltersAccess(t *testing.T) {
	e := newEnv(t)
	a := e.admin()
	v := e.viewerClient()
	if resp, page := v.get("/settings/container-filters"); resp.StatusCode != http.StatusOK || strings.Contains(page, "Save container filters") {
		t.Errorf("viewer tab: %d", resp.StatusCode)
	}
	if resp, _ := v.htmx("/settings/container-filters", url.Values{"patterns": {"x"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer save: %d", resp.StatusCode)
	}
	if resp, _ := a.htmx("/settings/container-filters", url.Values{"patterns": {strings.Repeat("a", maxPatternLen+1)}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("long pattern: %d", resp.StatusCode)
	}
}
