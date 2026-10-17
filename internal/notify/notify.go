// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notify sends selected events to ntfy, Gotify, Pushover, Telegram,
// Discord, email or any webhook. Channels are kept in the settings table;
// their URL, token and user are sealed like the single sign-on client secret.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// SettingKey is the settings row that holds Settings as JSON.
const SettingKey = "notifications"

// Channel kinds.
const (
	KindWebhook  = "webhook"
	KindNtfy     = "ntfy"
	KindGotify   = "gotify"
	KindDiscord  = "discord"
	KindPushover = "pushover"
	KindTelegram = "telegram"
	KindEmail    = "email"
)

// Kinds lists the channel kinds in menu order with their names.
var Kinds = []struct{ ID, Name string }{
	{KindNtfy, "ntfy"}, {KindGotify, "Gotify"}, {KindPushover, "Pushover"}, {KindTelegram, "Telegram"},
	{KindDiscord, "Discord"}, {KindEmail, "Email (SMTP)"}, {KindWebhook, "Webhook (JSON)"},
}

// Service addresses, variables so tests can point them elsewhere.
var (
	PushoverURL = "https://api.pushover.net/1/messages.json"
	TelegramURL = "https://api.telegram.org" // the default; a channel's URL replaces it
)

// Channel is one place notifications go.
type Channel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	URL     string   `json:"url"`            // sealed when stored
	Token   string   `json:"token"`          // sealed when stored; optional
	User    string   `json:"user,omitempty"` // sealed when stored: Pushover user key, SMTP username
	To      string   `json:"to,omitempty"`   // Telegram chat ID; email recipients, comma-separated
	From    string   `json:"from,omitempty"` // email sender
	Topics  []string `json:"topics"`
	Enabled bool     `json:"enabled"`
}

// Settings are all channels.
type Settings struct {
	Channels []Channel `json:"channels"`
}

// Validate checks a channel with its URL in plain text.
func (c Channel) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("give the channel a name")
	}
	if !slices.ContainsFunc(Kinds, func(k struct{ ID, Name string }) bool { return k.ID == c.Kind }) {
		return fmt.Errorf("unknown kind %q", c.Kind)
	}
	if err := c.validateKind(); err != nil {
		return err
	}
	for _, t := range c.Topics {
		if topicByID(t) == nil {
			return fmt.Errorf("unknown topic %q", t)
		}
	}
	return nil
}

// validateKind checks the fields each kind needs.
func (c Channel) validateKind() error {
	switch c.Kind {
	case KindPushover:
		if c.Token == "" || c.User == "" {
			return errors.New("a Pushover channel needs the application token and your user key")
		}
		return nil
	case KindTelegram:
		if c.Token == "" || c.To == "" {
			return errors.New("a Telegram channel needs the bot token and the chat ID")
		}
		if c.URL == "" {
			return nil
		}
	case KindEmail:
		return c.validateEmail()
	case KindGotify:
		if c.Token == "" {
			return errors.New("a Gotify channel needs an application token")
		}
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("the URL must start with http:// or https://")
	}
	return nil
}

// Topic is a group of event types a channel can subscribe to.
type Topic struct {
	ID, Name string
	Events   []string
	Default  bool // ticked for a new channel
}

// Topics lists what can be sent, in menu order.
var Topics = []Topic{
	{"failover", "Failover and failback, failover tests, and data limits reached", []string{"failover", "failback", "manual_failover", "manual_restore", "drill_started", "drill_ended", "drill_skipped", "data_alert"}, true},
	{"errors", "Failures: containers that did not stop or start, scheduled backups", []string{"run_failed", "backup_failed"}, true},
	{"flapping", "Flapping primary ISP", []string{"flapping"}, true},
	{"hosts", "Hosts going offline and coming back, and agent key problems", []string{"host_offline", "host_online", "host_key_expiring", "agent_refused"}, true},
	{"isps", "New ISPs and primary ISP changes", []string{"isp_new", "primary_learned", "primary_suggested", "primary_undecided"}, true},
	{"pause", "Automation paused or resumed", []string{"paused", "resumed"}, false},
	{"backups", "Backups made or restored", []string{"backup_created", "backup_restored"}, false},
}

func topicByID(id string) *Topic {
	for i := range Topics {
		if Topics[i].ID == id {
			return &Topics[i]
		}
	}
	return nil
}

func topicOf(eventType string) string {
	for _, t := range Topics {
		if slices.Contains(t.Events, eventType) {
			return t.ID
		}
	}
	return ""
}

// DefaultTopics are ticked for a new channel.
func DefaultTopics() []string {
	var ids []string
	for _, t := range Topics {
		if t.Default {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// Opener decrypts sealed values.
type Opener interface {
	Open(sealed string) (string, error)
}

// Result is the outcome of the last delivery to a channel.
type Result struct {
	At  time.Time
	Err string // empty when it worked
}

// Notifier delivers events in the background. Set its fields, then call Run.
type Notifier struct {
	Store   *store.Store
	Secrets Opener
	Log     *slog.Logger
	Client  *http.Client
	Retry   []time.Duration // waits before each retry; nil means 5 s, then 30 s
	Version string          // sent as the User-Agent
	TLS     *tls.Config     // RootCAs for SMTP servers; nil uses the system's

	queue        chan store.Event
	once         sync.Once
	mu           sync.Mutex
	last         map[string]Result
	sent, failed atomic.Uint64
}

func (n *Notifier) init() {
	n.once.Do(func() {
		n.queue = make(chan store.Event, 64)
		n.last = map[string]Result{}
		if n.Client == nil {
			n.Client = &http.Client{Timeout: 10 * time.Second}
		}
		if n.Retry == nil {
			n.Retry = []time.Duration{5 * time.Second, 30 * time.Second}
		}
	})
}

// Event queues an event for delivery. It never blocks: when the queue is
// full the event is dropped and logged. Use it as the store's OnEvent hook.
func (n *Notifier) Event(e store.Event) {
	n.init()
	if topicOf(e.Type) == "" {
		return
	}
	select {
	case n.queue <- e:
	default:
		n.Log.Warn("notification queue full; dropped", "event", e.Type)
	}
}

// Run delivers queued events until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	n.init()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-n.queue:
			n.deliver(ctx, e)
		}
	}
}

// Counts returns deliveries that worked and that failed after every retry.
func (n *Notifier) Counts() (sent, failed uint64) { return n.sent.Load(), n.failed.Load() }

// Last returns the last delivery result for a channel.
func (n *Notifier) Last(id string) (Result, bool) {
	n.init()
	n.mu.Lock()
	defer n.mu.Unlock()
	r, ok := n.last[id]
	return r, ok
}

// Load reads the channels with their URL and token still sealed.
func Load(ctx context.Context, st *store.Store) (Settings, error) {
	var s Settings
	_, err := st.GetSetting(ctx, SettingKey, &s)
	return s, err
}

// Open returns a copy of c with its URL, token and user decrypted.
func Open(c Channel, o Opener) (Channel, error) {
	var err error
	if c.URL, err = o.Open(c.URL); err != nil {
		return c, err
	}
	if c.Token, err = o.Open(c.Token); err != nil {
		return c, err
	}
	if c.User, err = o.Open(c.User); err != nil {
		return c, err
	}
	return c, nil
}

func (n *Notifier) deliver(ctx context.Context, e store.Event) {
	topic := topicOf(e.Type)
	s, err := Load(ctx, n.Store)
	if err != nil {
		n.Log.Error("cannot read notification settings", "err", err)
		return
	}
	var targets []Channel
	for _, c := range s.Channels {
		if c.Enabled && slices.Contains(c.Topics, topic) {
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 {
		return
	}
	names, _ := n.Store.ISPNames(ctx)
	msg := Format(e, names)
	var wg sync.WaitGroup
	for _, c := range targets {
		wg.Go(func() {
			plain, err := Open(c, n.Secrets)
			if err == nil {
				err = n.sendRetrying(ctx, plain, msg)
			}
			n.record(c, err)
		})
	}
	wg.Wait()
}

func (n *Notifier) record(c Channel, err error) {
	r := Result{At: time.Now()}
	if err != nil {
		r.Err = err.Error()
		n.failed.Add(1)
		n.Log.Warn("notification failed", "channel", c.Name, "err", err)
	} else {
		n.sent.Add(1)
	}
	n.mu.Lock()
	n.last[c.ID] = r
	n.mu.Unlock()
}

func (n *Notifier) sendRetrying(ctx context.Context, c Channel, m Message) error {
	err := n.send(ctx, c, m)
	for _, wait := range n.Retry {
		if err == nil || errors.Is(err, errPermanent) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		err = n.send(ctx, c, m)
	}
	return err
}

// Test sends a test message to a channel (URL and token in plain text) at
// once, without retries, and records the result.
func (n *Notifier) Test(ctx context.Context, c Channel) error {
	n.init()
	err := n.send(ctx, c, Message{
		Event: "test", Title: "Test notification", Priority: 3, Time: time.Now(),
		Text: "Notifications from WAN Drawbridge reach this channel.",
	})
	if c.ID != "" {
		n.record(c, err)
	}
	return err
}

// errPermanent marks a refusal that retrying cannot fix (4xx).
var errPermanent = errors.New("refused")

func (n *Notifier) send(ctx context.Context, c Channel, m Message) error {
	if c.Kind == KindEmail {
		return n.sendMail(ctx, c, m)
	}
	req, err := request(ctx, c, m)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "wan-drawbridge/"+n.Version)
	resp, err := n.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			// The path can hold a secret: a Discord webhook's, a Telegram bot token.
			if u, perr := url.Parse(ue.URL); perr == nil {
				ue.URL = u.Scheme + "://" + u.Host
			}
		}
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s %s", errPermanent, resp.Status, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("%s %s", resp.Status, strings.TrimSpace(string(body)))
}

// request builds the HTTP request each kind of channel expects.
func request(ctx context.Context, c Channel, m Message) (*http.Request, error) {
	post := func(u string, body []byte, contentType string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", contentType)
		}
		return req, err
	}
	switch c.Kind {
	case KindNtfy:
		// The URL is the topic, e.g. https://ntfy.sh/my-topic.
		req, err := post(c.URL, []byte(m.Text), "text/plain; charset=utf-8")
		if err != nil {
			return nil, err
		}
		req.Header.Set("Title", m.Title)
		req.Header.Set("Priority", fmt.Sprint(m.Priority))
		req.Header.Set("Tags", ntfyTag(m))
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		return req, nil
	case KindGotify:
		// The URL is the server; the token is an application token.
		b, _ := json.Marshal(map[string]any{"title": m.Title, "message": m.Text, "priority": gotifyPriority(m.Priority)})
		req, err := post(strings.TrimRight(c.URL, "/")+"/message", b, "application/json")
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Gotify-Key", c.Token)
		return req, nil
	case KindPushover:
		form := url.Values{"token": {c.Token}, "user": {c.User}, "title": {m.Title}, "message": {m.Text},
			"priority": {fmt.Sprint(pushoverPriority(m.Priority))}, "timestamp": {fmt.Sprint(m.Time.Unix())}}
		return post(PushoverURL, []byte(form.Encode()), "application/x-www-form-urlencoded")
	case KindTelegram:
		base := TelegramURL
		if c.URL != "" {
			base = c.URL
		}
		b, _ := json.Marshal(map[string]any{
			"chat_id": c.To, "text": m.Title + "\n\n" + m.Text, "disable_web_page_preview": true,
		})
		return post(strings.TrimRight(base, "/")+"/bot"+c.Token+"/sendMessage", b, "application/json")
	case KindDiscord:
		// The URL is a channel webhook; it carries its own secret.
		b, _ := json.Marshal(map[string]any{
			"username": "WAN Drawbridge",
			"content":  "**" + m.Title + "**\n" + m.Text,
		})
		return post(c.URL, b, "application/json")
	default:
		b, _ := json.Marshal(m)
		req, err := post(c.URL, b, "application/json")
		if err != nil {
			return nil, err
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		return req, nil
	}
}

func ntfyTag(m Message) string {
	switch {
	case m.Priority >= 4:
		return "warning"
	case m.Event == "test":
		return "white_check_mark"
	}
	return "information_source"
}

// pushoverPriority maps 1-5 to Pushover's -2 to 1. Pushover's 2 (emergency)
// repeats until acknowledged, so it is not used.
func pushoverPriority(p int) int { return map[int]int{1: -2, 2: -1, 3: 0, 4: 1, 5: 1}[p] }

// gotifyPriority maps 1-5 to Gotify's 0-10 scale.
func gotifyPriority(p int) int { return map[int]int{1: 1, 2: 3, 3: 5, 4: 8, 5: 10}[p] }
