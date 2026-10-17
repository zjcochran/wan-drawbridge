// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"fmt"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// Message is what a channel receives. The webhook kind gets it as JSON.
type Message struct {
	Event    string         `json:"event"`
	Title    string         `json:"title"`
	Text     string         `json:"message"`
	Priority int            `json:"priority"` // 1 (lowest) to 5 (urgent), as in ntfy
	Time     time.Time      `json:"time"`
	Details  map[string]any `json:"details,omitempty"` // the event's own fields
}

// Format turns an event into a message. names maps ISP IDs to names.
func Format(e store.Event, names map[int]string) Message {
	p := e.Payload
	str := func(k string) string {
		if v, ok := p[k]; ok && v != nil {
			if f, ok := v.(float64); ok && f == float64(int64(f)) {
				return fmt.Sprint(int64(f))
			}
			return fmt.Sprint(v)
		}
		return ""
	}
	isp := func(k string) string {
		if f, ok := p[k].(float64); ok {
			if n := names[int(f)]; n != "" {
				return n
			}
		}
		return "ISP #" + str(k)
	}
	on := func() string {
		if str("name") == "" {
			return "an unknown ISP"
		}
		return fmt.Sprintf("%s (AS%s)", str("name"), str("asn"))
	}
	paused := p["paused"] == true

	m := Message{Event: e.Type, Priority: 3, Time: e.TS.UTC(), Details: p}
	switch e.Type {
	case "failover":
		m.Title, m.Priority = "Failed over to a backup ISP", 4
		m.Text = "Traffic is on " + on() + ". Containers set to stop on failover are being stopped."
		if paused {
			m.Text = "Traffic is on " + on() + ". Automation is paused, so no container was stopped."
		}
	case "failback":
		m.Title = "Back on the primary ISP"
		m.Text = "Traffic is on " + on() + " again. Containers Drawbridge stopped are being started."
		if paused {
			m.Text = "Traffic is on " + on() + " again. Automation is paused, so no container was started."
		}
	case "manual_failover":
		m.Title, m.Priority = "Manual failover", 4
		m.Text = str("by") + " stopped the containers set to stop on failover."
	case "manual_restore":
		m.Title = "Manual restore"
		m.Text = str("by") + " started the containers Drawbridge had stopped."
	case "run_failed":
		m.Title, m.Priority = map[string]string{
			"stopping": "Stopping containers failed", "starting": "Starting containers failed",
		}[str("action")], 5
		if m.Title == "" {
			m.Title = "Updating containers failed"
		}
		m.Text = str("error") + ". See the dashboard for which containers are affected."
	default:
		if !formatISP(&m, str, isp) && !formatDrill(&m, str) && !formatHost(&m, str) && !formatData(&m, p, isp) {
			formatAdmin(&m, str)
		}
	}
	return m
}

// formatISP fills in the messages about ISPs and learning the primary, and
// reports whether m.Event is one of them.
func formatISP(m *Message, str, isp func(string) string) bool {
	switch m.Event {
	case "flapping":
		m.Title = "The primary ISP is flapping"
		m.Text = fmt.Sprintf("It failed again soon after failing back (%s times in a row), so failback now waits %s of stable connection.",
			str("flaps"), str("failback_wait"))
	case "isp_new":
		m.Title = "New ISP seen"
		m.Text = fmt.Sprintf("Drawbridge saw %s (AS%s) for the first time. Until it is learned or set as primary on the ISPs page, it counts as a metered backup.",
			str("name"), str("asn"))
	case "primary_learned":
		m.Title = "Primary ISP learned"
		m.Text = isp("isp") + " is now the primary ISP."
	case "primary_suggested":
		m.Title = "Another ISP looks primary"
		m.Text = isp("isp") + " now carries most of the traffic, but the primary is still " + isp("current") +
			". Change it on the ISPs page if that is permanent."
	case "primary_undecided":
		m.Title = "Primary ISP not decided"
		m.Text = "Learning ended without a clear primary ISP. Set one on the ISPs page."
	default:
		return false
	}
	return true
}

// formatDrill fills in the messages about failover tests, and reports
// whether m.Event is one of them.
func formatDrill(m *Message, str func(string) string) bool {
	switch m.Event {
	case "drill_started":
		m.Title = "Failover test started"
		who := str("by")
		if who == "schedule" {
			who = "The schedule"
		}
		m.Text = fmt.Sprintf("%s stopped the containers set to stop on failover, as a test. They start again in %s minutes.",
			who, str("minutes"))
	case "drill_ended":
		m.Title = "Failover test ended"
		m.Text = "The containers stopped for the test are being started again."
		if m.Details["kept_stopped"] == true {
			m.Text = "A failover took over during the test, so the containers stay stopped until failback."
		}
	case "drill_skipped":
		m.Title, m.Priority = "Scheduled failover test skipped", 4
		m.Text = "The scheduled failover test did not run (" + str("reason") + ")."
	default:
		return false
	}
	return true
}

// formatData formats data_alert; it reports false for other events.
func formatData(m *Message, p map[string]any, isp func(string) string) bool {
	if m.Event != "data_alert" {
		return false
	}
	num := func(k string) int64 { f, _ := p[k].(float64); return int64(f) }
	during := "during this outage"
	m.Title = "Data limit for this outage reached"
	if p["scope"] == "month" {
		during, m.Title = "during this month's outages", "Data limit for this month reached"
	}
	m.Priority = 4
	m.Text = fmt.Sprintf("Containers have used %s on %s %s, over its %s limit.", humanBytes(num("used")), isp("isp"), during, humanBytes(num("limit")))
	return true
}

// humanBytes writes n in decimal units, as the web UI does.
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

// formatAdmin covers events started by people or schedules: pausing, backups.
// formatHost formats agent host events; it reports false for other events.
func formatHost(m *Message, str func(string) string) bool {
	switch m.Event {
	case "host_offline":
		m.Title, m.Priority = "Host "+str("host")+" is offline", 4
		m.Text = "Its agent has not been in touch for 90 seconds. Its containers are stopped or started when it is back."
	case "host_online":
		m.Title = "Host " + str("host") + " is back"
		m.Text = "Its agent is in touch again."
	case "host_key_expiring":
		m.Title, m.Priority = "Agent key of "+str("host")+" is about to expire", 4
		m.Text = "The agent has not rotated its key. It is refused from " + str("expires") +
			" unless it rotates; check that it runs, or make a new enrollment token in Settings → Hosts."
	case "agent_refused":
		m.Title, m.Priority = "Agent of "+str("host")+" was refused", 4
		m.Text = str("reason") + ". If this was not you, revoke the host in Settings → Hosts."
	default:
		return false
	}
	return true
}

func formatAdmin(m *Message, str func(string) string) {
	switch m.Event {
	case "paused":
		m.Title = "Automation paused"
		m.Text = str("by") + " paused automation until it is resumed."
		if u, err := time.Parse(time.RFC3339, str("until")); err == nil {
			m.Text = fmt.Sprintf("%s paused automation until %s.", str("by"), u.Local().Format("Mon 15:04"))
		}
	case "resumed":
		m.Title = "Automation resumed"
		m.Text = str("by") + " resumed automation."
		if str("by") == "timer" {
			m.Text = "The timed pause ended."
		}
	case "backup_created":
		m.Title = "Backup downloaded"
		m.Text = fmt.Sprintf("%s downloaded a backup (history: %s).", str("by"), str("history"))
		if str("by") == "schedule" {
			m.Title = "Scheduled backup saved"
			m.Text = fmt.Sprintf("Saved %s (history: %s).", str("file"), str("history"))
		}
	case "backup_failed":
		m.Title, m.Priority = "Scheduled backup failed", 4
		m.Text = str("error") + ". Settings → Backup and restore shows the details."
	case "backup_restored":
		m.Title = "Backup restored"
		made := str("created")
		if t, err := time.Parse(time.RFC3339, made); err == nil {
			made = t.Local().Format("2006-01-02 15:04")
		}
		m.Text = fmt.Sprintf("%s restored a backup made %s. Everyone was signed out.", str("by"), made)
	default:
		m.Title, m.Text = m.Event, ""
	}
}
