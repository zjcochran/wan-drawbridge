// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// Limits on the container filters.
const (
	maxHidePatterns = 100
	maxPatternLen   = 200
)

// hideRules are the patterns of containers the Containers page hides.
type hideRules []string

// hides reports whether a pattern matches the container's name, its key or
// "stack/name".
func (h hideRules) hides(c containerRow) bool {
	for _, p := range h {
		if globMatch(p, c.Name) || globMatch(p, c.Key) || (c.Project != "" && globMatch(p, c.Project+"/"+c.Name)) {
			return true
		}
	}
	return false
}

// globMatch matches s against a pattern where * is any run of characters
// and ? is one character, ignoring case. The pattern must match all of s.
func globMatch(pattern, s string) bool {
	p, t := []rune(strings.ToLower(pattern)), []rune(strings.ToLower(s))
	pi, ti, star, mark := 0, 0, -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi, mark = star+1, mark+1
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// hideRules loads the saved patterns.
func (s *Server) hideRules(ctx context.Context) (hideRules, error) {
	var h hideRules
	_, err := s.Store.GetSetting(ctx, store.SettingContainerFilters, &h)
	return h, err
}

// parsePatterns reads one pattern per line, dropping blanks and repeats.
func parsePatterns(text string) (hideRules, error) {
	h := hideRules{}
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		if utf8.RuneCountInString(p) > maxPatternLen {
			return nil, fmt.Errorf("a pattern can be at most %d characters", maxPatternLen)
		}
		seen[strings.ToLower(p)] = true
		h = append(h, p)
	}
	if len(h) > maxHidePatterns {
		return nil, fmt.Errorf("at most %d patterns", maxHidePatterns)
	}
	return h, nil
}

func (s *Server) saveContainerFilters(w http.ResponseWriter, r *http.Request) {
	h, err := parsePatterns(r.PostFormValue("patterns"))
	if err != nil {
		invalid(w, err.Error())
		return
	}
	if err := s.Store.SetSetting(r.Context(), store.SettingContainerFilters, h); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(r.Context(), "settings_changed", map[string]any{"by": actorName(r)})
	w.Header().Set("HX-Trigger", `{"toast":"Container filters saved."}`)
	s.partial(w, r, "settings", "container-filters-form", settingsData{Filters: strings.Join(h, "\n"), IsAdmin: true})
}
