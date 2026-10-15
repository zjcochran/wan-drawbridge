// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"html/template"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/wan"
)

var funcs = template.FuncMap{
	"ago":        ago,
	"datetime":   func(t time.Time) string { return zeroOr(t, t.Local().Format("2006-01-02 15:04:05")) },
	"isodate":    func(t time.Time) string { return zeroOr(t, t.UTC().Format(time.RFC3339)) },
	"duration":   shortDuration,
	"until":      until,
	"clock":      clock,
	"hours":      func(sec int64) string { return fmt.Sprintf("%.1f h", float64(sec)/3600) },
	"pct":        pct,
	"stateLabel": stateLabel,
	"stateClass": stateClass,
	"summary":    summary,
	"add":        func(a, b int) int { return a + b },
	"sub":        func(a, b int) int { return a - b },
	"join":       strings.Join,
	"query":      url.QueryEscape,
	"list":       func(v ...string) []string { return v },
	"dict":       dict,
	"kb":         func(n int64) float64 { return float64(n) / 1000 },
	"initial":    initial,
	"static":     staticURL,
	"span":       span,
	"add64":      func(a, b int64) int64 { return a + b },
	"bytes":      humanBytes,
	"month":      func(t time.Time) string { return t.Local().Format("Jan 2006") },
	"keyAge":     func(t time.Time) string { return keyAge(t, time.Now()) },
}

// dict builds a map from key/value pairs, to pass several values to a template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict needs key/value pairs")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

func zeroOr(t time.Time, s string) string {
	if t.IsZero() {
		return "never"
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// until prints the time left until t, rounded: "3m 20s", "45s", "now".
func until(t time.Time) string {
	d := time.Until(t).Round(time.Second)
	switch {
	case t.IsZero() || d <= 0:
		return "a moment"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// clock prints a time of day, with the weekday when it is not today: "15:04",
// "Tue 09:30".
func clock(t time.Time) string {
	t, now := t.Local(), time.Now().Local()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

// shortDuration prints durations the way people type them: 60s, 5m, 24h, 7d.
func shortDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// parseDuration also accepts days ("7d"), which time.ParseDuration does not.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err == nil && fmt.Sprint(days) == n {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (examples: 60s, 5m, 24h, 7d)", s)
	}
	return d, nil
}

func pct(part, total int64) string {
	if total == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(part)/float64(total))
}

func stateLabel(s wan.State) string {
	switch s {
	case wan.Learning:
		return "Learning"
	case wan.Primary:
		return "On primary"
	case wan.FailoverPending:
		return "Failover pending"
	case wan.Failover:
		return "Failover"
	case wan.FailbackPending:
		return "Failback pending"
	}
	return "Starting"
}

func stateClass(s wan.State) string {
	switch s {
	case wan.Primary:
		return "ok"
	case wan.FailoverPending, wan.FailbackPending:
		return "warn"
	case wan.Failover:
		return "bad"
	}
	return "muted"
}

// summary renders an event payload as "key=value" pairs, sorted.
func summary(p map[string]any) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := p[k]
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			v = int64(f)
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// initial is the first letter of a name, upper-cased, for an avatar without a
// picture.
func initial(name string) string {
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return "?"
}

// span prints how long something lasted, to the second under a minute and
// rounded down to the minute above: "45s", "12m", "3h 20m", "2d 4h".
func span(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// humanBytes prints a byte count in decimal units: "980 B", "1.2 MB", "3.4 GB".
func humanBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	f, units := float64(n)/1000, "kMGTP"
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f %cB", f, units[i])
	}
	return fmt.Sprintf("%.1f %cB", f, units[i])
}
