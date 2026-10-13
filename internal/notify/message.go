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
	case "flapping":
		m.Title = "The primary ISP is flapping"
		m.Text = fmt.Sprintf("It failed again soon after failing back (%s times in a row), so failback now waits %s of stable connection.",
			str("flaps"), str("failback_wait"))
	case "isp_new":
		m.Title = "New ISP seen"
		m.Text = fmt.Sprintf("Drawbridge saw %s (AS%s) for the first time. Until it is learned or set as primary on the ISPs page, it counts as a backup.",
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
		m.Title, m.Text = e.Type, ""
	}
	return m
}
