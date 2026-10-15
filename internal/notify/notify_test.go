// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// plain "seals" by prefixing, so tests can see what was opened.
type plain struct{}

func (plain) Open(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, "sealed:") {
		return "", errors.New("not sealed")
	}
	return strings.TrimPrefix(s, "sealed:"), nil
}

type hit struct {
	path, body string
	header     http.Header
}

// receiver records requests and answers with the next status in codes
// (200 once they run out).
type receiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  []hit
	codes []int
}

func newReceiver(t *testing.T, codes ...int) *receiver {
	r := &receiver{codes: codes}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, hit{req.URL.Path, string(b), req.Header.Clone()})
		code := http.StatusOK
		if len(r.codes) > 0 {
			code, r.codes = r.codes[0], r.codes[1:]
		}
		r.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) got() []hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hit(nil), r.hits...)
}

func newNotifier(t *testing.T) *Notifier {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := &Notifier{Store: st, Secrets: plain{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Retry: []time.Duration{time.Millisecond, time.Millisecond}, Version: "test"}
	n.init()
	return n
}

func save(t *testing.T, n *Notifier, chans ...Channel) {
	t.Helper()
	for i := range chans {
		chans[i].URL = "sealed:" + chans[i].URL
		if chans[i].Token != "" {
			chans[i].Token = "sealed:" + chans[i].Token
		}
	}
	if err := n.Store.SetSetting(context.Background(), SettingKey, Settings{Channels: chans}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// delivered reports whether the receiver got a message for each channel and
// the notifier recorded every result.
func delivered(n *Notifier, r *receiver, ids ...string) bool {
	sent, _ := n.Counts()
	for _, id := range ids {
		if _, ok := n.Last(id); !ok {
			return false
		}
	}
	return len(r.got()) == len(ids) && sent == uint64(len(ids))
}

func TestDeliveryByTopicAndKind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNotifier(t)
	r := newReceiver(t)
	save(t, n,
		Channel{ID: "a", Name: "phone", Kind: KindNtfy, URL: r.srv.URL + "/alerts", Token: "tk", Topics: []string{"failover"}, Enabled: true},
		Channel{ID: "b", Name: "gotify", Kind: KindGotify, URL: r.srv.URL + "/", Token: "app", Topics: []string{"failover"}, Enabled: true},
		Channel{ID: "c", Name: "discord", Kind: KindDiscord, URL: r.srv.URL + "/api/webhooks/1/x", Topics: []string{"failover"}, Enabled: true},
		Channel{ID: "d", Name: "hook", Kind: KindWebhook, URL: r.srv.URL + "/hook", Token: "bearer", Topics: []string{"failover"}, Enabled: true},
		Channel{ID: "e", Name: "off", Kind: KindWebhook, URL: r.srv.URL + "/off", Topics: []string{"failover"}},
		Channel{ID: "f", Name: "pauses", Kind: KindWebhook, URL: r.srv.URL + "/pauses", Topics: []string{"pause"}, Enabled: true},
	)
	n.Store.OnEvent(n.Event)
	go n.Run(ctx)

	if err := n.Store.AddEvent(ctx, time.Unix(1_800_000_000, 0), "failover",
		map[string]any{"asn": 64510, "name": "SKYLINK-64510", "isp": 2}); err != nil {
		t.Fatal(err)
	}
	n.Store.AddEvent(ctx, time.Now(), "settings_changed", map[string]any{"by": "alice"}) // no topic: never sent
	// The receiver can see a message before the notifier has recorded the
	// result: wait for both.
	waitFor(t, "four deliveries", func() bool { return delivered(n, r, "a", "b", "c", "d") })

	by := map[string]hit{}
	for _, h := range r.got() {
		by[h.path] = h
	}
	ntfy := by["/alerts"]
	if ntfy.header.Get("Title") != "Failed over to a backup ISP" || ntfy.header.Get("Priority") != "4" ||
		ntfy.header.Get("Authorization") != "Bearer tk" || !strings.Contains(ntfy.body, "SKYLINK-64510 (AS64510)") {
		t.Errorf("ntfy: %v %q", ntfy.header, ntfy.body)
	}
	var g map[string]any
	json.Unmarshal([]byte(by["/message"].body), &g)
	if by["/message"].header.Get("X-Gotify-Key") != "app" || g["priority"] != float64(8) || g["title"] != "Failed over to a backup ISP" {
		t.Errorf("gotify: %v %v", by["/message"].header, g)
	}
	if !strings.Contains(by["/api/webhooks/1/x"].body, `"content":"**Failed over to a backup ISP**\nTraffic is on SKYLINK`) {
		t.Errorf("discord: %s", by["/api/webhooks/1/x"].body)
	}
	var m Message
	json.Unmarshal([]byte(by["/hook"].body), &m)
	if by["/hook"].header.Get("Authorization") != "Bearer bearer" || m.Event != "failover" || m.Details["asn"] != float64(64510) ||
		!m.Time.Equal(time.Unix(1_800_000_000, 0)) || by["/hook"].header.Get("User-Agent") != "wan-drawbridge/test" {
		t.Errorf("webhook: %v %+v", by["/hook"].header, m)
	}
	if r, ok := n.Last("a"); !ok || r.Err != "" {
		t.Errorf("last result: %+v %v", r, ok)
	}
	if sent, failed := n.Counts(); sent != 4 || failed != 0 {
		t.Errorf("counts %d %d", sent, failed)
	}
}

func TestRetries(t *testing.T) {
	ctx := context.Background()
	n := newNotifier(t)
	n.init()
	r := newReceiver(t, 503, 502)
	c := Channel{ID: "a", Kind: KindWebhook, URL: r.srv.URL}
	if err := n.sendRetrying(ctx, c, Message{}); err != nil || len(r.got()) != 3 {
		t.Fatalf("recovered after two failures: %v, %d tries", err, len(r.got()))
	}
	r = newReceiver(t, 500, 500, 500, 500)
	c.URL = r.srv.URL
	if err := n.sendRetrying(ctx, c, Message{}); err == nil || len(r.got()) != 3 {
		t.Fatalf("gave up after three tries: %v, %d", err, len(r.got()))
	}
	r = newReceiver(t, 401)
	c.URL = r.srv.URL
	if err := n.sendRetrying(ctx, c, Message{}); !errors.Is(err, errPermanent) || len(r.got()) != 1 {
		t.Fatalf("4xx is not retried: %v, %d", err, len(r.got()))
	}
	r = newReceiver(t, 429)
	c.URL = r.srv.URL
	if err := n.sendRetrying(ctx, c, Message{}); err != nil || len(r.got()) != 2 {
		t.Fatalf("429 is retried: %v, %d", err, len(r.got()))
	}
}

func TestFailuresAreRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNotifier(t)
	n.Retry = []time.Duration{}
	r := newReceiver(t, 404)
	save(t, n, Channel{ID: "a", Name: "gone", Kind: KindWebhook, URL: r.srv.URL, Topics: []string{"errors"}, Enabled: true},
		Channel{ID: "b", Name: "broken", Kind: KindWebhook, URL: "x", Topics: []string{"errors"}, Enabled: true})
	// b's URL cannot be opened: save sealed it, so break it on purpose.
	set, _ := Load(ctx, n.Store)
	set.Channels[1].URL = "garbage"
	n.Store.SetSetting(ctx, SettingKey, set)
	go n.Run(ctx)
	n.Event(store.Event{Type: "run_failed", Payload: map[string]any{"action": "stopping", "error": "boom"}})
	waitFor(t, "two results", func() bool { _, a := n.Last("a"); _, b := n.Last("b"); return a && b })
	if r, _ := n.Last("a"); !strings.Contains(r.Err, "404") {
		t.Errorf("a: %+v", r)
	}
	if r, _ := n.Last("b"); !strings.Contains(r.Err, "not sealed") {
		t.Errorf("b: %+v", r)
	}
	if _, failed := n.Counts(); failed != 2 {
		t.Errorf("failed = %d", failed)
	}
}

func TestQueueNeverBlocks(t *testing.T) {
	n := newNotifier(t)
	for range 200 { // nobody runs the queue
		n.Event(store.Event{Type: "failover"})
	}
	if len(n.queue) != cap(n.queue) {
		t.Fatalf("queue %d of %d", len(n.queue), cap(n.queue))
	}
}

func TestTest(t *testing.T) {
	n := newNotifier(t)
	r := newReceiver(t, 200, 403)
	if err := n.Test(context.Background(), Channel{Kind: KindNtfy, URL: r.srv.URL}); err != nil {
		t.Fatal(err)
	}
	if h := r.got()[0]; h.header.Get("Title") != "Test notification" || h.header.Get("Tags") != "white_check_mark" {
		t.Errorf("test message: %v", h.header)
	}
	if err := n.Test(context.Background(), Channel{ID: "x", Kind: KindNtfy, URL: r.srv.URL}); err == nil {
		t.Fatal("403 reported as sent")
	}
	if res, ok := n.Last("x"); !ok || !strings.Contains(res.Err, "403") {
		t.Errorf("test result not recorded: %+v", res)
	}
}

func TestValidate(t *testing.T) {
	good := Channel{Name: "phone", Kind: KindNtfy, URL: "https://ntfy.sh/t", Topics: []string{"failover"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Channel{
		"no name":   {Kind: KindNtfy, URL: good.URL},
		"bad kind":  {Name: "x", Kind: "pager", URL: good.URL},
		"bad url":   {Name: "x", Kind: KindNtfy, URL: "ftp://example.com"},
		"no host":   {Name: "x", Kind: KindNtfy, URL: "https://"},
		"bad topic": {Name: "x", Kind: KindNtfy, URL: good.URL, Topics: []string{"everything"}},
		"not a url": {Name: "x", Kind: KindNtfy, URL: "%zz"},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if d := DefaultTopics(); len(d) != 4 || d[0] != "failover" {
		t.Errorf("default topics %v", d)
	}
}

func TestFormat(t *testing.T) {
	names := map[int]string{1: "EXAMPLENET", 2: "SKYLINK"}
	cases := []struct {
		typ     string
		payload map[string]any
		title   string
		text    string
	}{
		{"failover", map[string]any{"asn": 64510.0, "name": "SKYLINK-64510", "paused": true}, "Failed over", "Automation is paused"},
		{"failback", map[string]any{"asn": 64500.0, "name": "EXAMPLENET-64500"}, "Back on the primary", "being started"},
		{"failback", map[string]any{"paused": true}, "Back on the primary", "an unknown ISP again. Automation is paused"},
		{"manual_failover", map[string]any{"by": "alice"}, "Manual failover", "alice stopped"},
		{"manual_restore", map[string]any{"by": "alice"}, "Manual restore", "alice started"},
		{"run_failed", map[string]any{"action": "starting", "error": "timeout"}, "Starting containers failed", "timeout."},
		{"run_failed", map[string]any{"action": "applying", "error": "x"}, "Updating containers failed", "x."},
		{"flapping", map[string]any{"flaps": 2.0, "failback_wait": "20m0s"}, "flapping", "2 times in a row), so failback now waits 20m0s"},
		{"isp_new", map[string]any{"name": "BIGTEL", "asn": 64511.0}, "New ISP seen", "BIGTEL (AS64511)"},
		{"primary_learned", map[string]any{"isp": 1.0}, "Primary ISP learned", "EXAMPLENET is now"},
		{"primary_suggested", map[string]any{"isp": 2.0, "current": 9.0}, "Another ISP", "SKYLINK now carries most of the traffic, but the primary is still ISP #9"},
		{"primary_undecided", nil, "not decided", "Set one"},
		{"paused", map[string]any{"by": "alice"}, "Automation paused", "until it is resumed"},
		{"paused", map[string]any{"by": "alice", "until": "2027-01-15T07:00:00Z"}, "Automation paused", "alice paused automation until "},
		{"resumed", map[string]any{"by": "alice"}, "Automation resumed", "alice resumed"},
		{"resumed", map[string]any{"by": "timer"}, "Automation resumed", "The timed pause ended."},
		{"backup_created", map[string]any{"by": "alice", "history": "30d"}, "Backup downloaded", "(history: 30d)"},
		{"backup_restored", map[string]any{"by": "alice", "created": "2027-01-15T07:00:00Z"}, "Backup restored", "restored a backup made 2027-01-1"},
		{"backup_created", map[string]any{"by": "schedule", "history": "none", "file": "drawbridge-backup-20270115-030000.json"}, "Scheduled backup saved", "Saved drawbridge-backup-20270115-030000.json"},
		{"backup_failed", map[string]any{"error": "disk full"}, "Scheduled backup failed", "disk full."},
		{"drill_started", map[string]any{"by": "alice", "minutes": 5.0}, "Failover test started", "They start again in 5 minutes."},
		{"drill_started", map[string]any{"by": "schedule", "minutes": 5.0}, "Failover test started", "The schedule stopped"},
		{"drill_skipped", map[string]any{"by": "schedule", "reason": "skipped: automation is paused"}, "Scheduled failover test skipped", "(skipped: automation is paused)"},
		{"drill_ended", map[string]any{"by": "timer"}, "Failover test ended", "being started again"},
		{"drill_ended", map[string]any{"by": "failover", "kept_stopped": true}, "Failover test ended", "stay stopped until failback"},
		{"something_else", nil, "something_else", ""},
	}
	for _, c := range cases {
		m := Format(store.Event{Type: c.typ, Payload: c.payload, TS: time.Unix(1, 0)}, names)
		if !strings.Contains(m.Title, c.title) || !strings.Contains(m.Text, c.text) {
			t.Errorf("%s: %q / %q", c.typ, m.Title, m.Text)
		}
	}
	if m := Format(store.Event{Type: "run_failed"}, nil); m.Priority != 5 {
		t.Errorf("run_failed priority %d", m.Priority)
	}
}
