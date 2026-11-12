// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/http"
	"net/url"
	"strings"
)

// jumpLink is one result of the jump box (⌘K): a page, a settings tab, a
// stack or a container.
type jumpLink struct {
	Href, Label, Kind string
}

const jumpLimit = 12

var jumpPages = []jumpLink{
	{"/", "Dashboard", "page"}, {"/stacks", "Stacks", "page"}, {"/containers", "Containers", "page"},
	{"/isps", "ISPs", "page"}, {"/events", "Events", "page"}, {"/reports", "Reports", "page"},
	{"/preview", "Preview a failover", "page"},
}

// jump lists what matches the jump box's text: pages and settings tabs, then
// stacks, then containers.
func (s *Server) jump(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	match := func(label string) bool { return strings.Contains(strings.ToLower(label), q) }
	var out []jumpLink
	addLink := func(l jumpLink) {
		if len(out) < jumpLimit && match(l.Label) {
			out = append(out, l)
		}
	}
	for _, l := range jumpPages {
		addLink(l)
	}
	admin := sessionFrom(r).User.IsAdmin()
	for _, t := range settingsTabs {
		if !t.Admin || admin {
			addLink(jumpLink{"/settings/" + t.ID, "Settings · " + t.Name, "settings"})
		}
	}
	if q != "" {
		rows, err := s.Store.Containers(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		stacks := map[string]bool{}
		for _, c := range rows {
			if c.Project != "" && !stacks[c.Project] {
				stacks[c.Project] = true
				addLink(jumpLink{"/stacks?q=" + url.QueryEscape(c.Project+"/"), c.Project, "stack"})
			}
		}
		for _, c := range rows {
			addLink(jumpLink{"/stacks?q=" + url.QueryEscape(c.Name), c.Name, "container"})
		}
	}
	s.partial(w, r, "dashboard", "jump-results", out)
}
