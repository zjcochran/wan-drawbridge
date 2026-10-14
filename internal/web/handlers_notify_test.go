// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/zjcochran/wan-drawbridge/internal/notify"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// inbox is a notification receiver that answers with status.
type inbox struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newInbox(t *testing.T) *inbox {
	in := &inbox{status: http.StatusOK}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		in.mu.Lock()
		in.bodies = append(in.bodies, r.Header.Get("Title")+"|"+string(b))
		status := in.status
		in.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(in.srv.Close)
	return in
}

func (e *env) channels(t *testing.T) []notify.Channel {
	t.Helper()
	set, err := notify.Load(context.Background(), e.st)
	if err != nil {
		t.Fatal(err)
	}
	return set.Channels
}

func TestNotificationChannels(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	in := newInbox(t)
	if _, page := c.get("/settings/notifications"); !strings.Contains(page, "Add a channel") {
		t.Fatal("no notifications card for admins")
	}
	viewer := e.viewerClient()
	if _, page := viewer.get("/settings"); strings.Contains(page, "Notifications") {
		t.Fatal("viewers see the notifications card")
	}

	add := url.Values{"name": {"Phone"}, "kind": {"ntfy"}, "url": {in.srv.URL + "/alerts"}, "token": {"tk_secret"},
		"topics": {"failover", "errors"}, "enabled": {"on"}}
	resp, body := c.htmx("/settings/notifications", add)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Phone") || !strings.Contains(body, strings.TrimPrefix(in.srv.URL, "http://")) {
		t.Fatalf("add: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "/alerts") || strings.Contains(body, "tk_secret") {
		t.Fatal("the page shows the saved URL path or token")
	}
	chans := e.channels(t)
	if len(chans) != 1 || !strings.HasPrefix(chans[0].URL, "v1:") || !strings.HasPrefix(chans[0].Token, "v1:") {
		t.Fatalf("stored: %+v", chans)
	}
	id := chans[0].ID

	// Editing with the URL and token left blank keeps them.
	resp, _ = c.htmx("/settings/notifications", url.Values{"id": {id}, "name": {"Phone 2"}, "kind": {"ntfy"}, "topics": {"pause"}})
	ch := e.channels(t)[0]
	if resp.StatusCode != http.StatusOK || ch.Name != "Phone 2" || ch.Enabled || len(ch.Topics) != 1 {
		t.Fatalf("edit: %d %+v", resp.StatusCode, ch)
	}
	if plain, _ := notify.Open(ch, e.ui.Secrets); plain.URL != in.srv.URL+"/alerts" || plain.Token != "tk_secret" {
		t.Fatalf("kept: %+v", plain)
	}
	c.htmx("/settings/notifications", url.Values{"id": {id}, "name": {"Phone 2"}, "kind": {"ntfy"}, "remove_token": {"on"}})
	if e.channels(t)[0].Token != "" {
		t.Error("token not removed")
	}

	testAndDelete(t, e, c, viewer, in, id, add)
}

func testAndDelete(t *testing.T, e *env, c, viewer *client, in *inbox, id string, add url.Values) {
	t.Helper()
	for name, v := range map[string]url.Values{
		"no name":         {"kind": {"ntfy"}, "url": {in.srv.URL}},
		"bad url":         {"name": {"x"}, "kind": {"ntfy"}, "url": {"ntfy.sh/topic"}},
		"gotify no token": {"name": {"x"}, "kind": {"gotify"}, "url": {in.srv.URL}},
		"unknown topic":   {"name": {"x"}, "kind": {"ntfy"}, "url": {in.srv.URL}, "topics": {"all"}},
	} {
		if resp, _ := c.htmx("/settings/notifications", v); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := c.htmx("/settings/notifications", url.Values{"id": {"gone"}, "name": {"x"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: %d", resp.StatusCode)
	}

	// Test uses the form as it is, falling back to the saved URL.
	if _, body := c.htmx("/settings/notifications/test", url.Values{"id": {id}, "name": {"Phone 2"}, "kind": {"ntfy"}}); !strings.Contains(body, "✓ Sent") {
		t.Fatalf("test: %s", body)
	}
	if len(in.bodies) != 1 || !strings.HasPrefix(in.bodies[0], "Test notification|") {
		t.Fatalf("received %v", in.bodies)
	}
	in.status = http.StatusUnauthorized
	if _, body := c.htmx("/settings/notifications/test", url.Values{"id": {id}, "name": {"Phone 2"}, "kind": {"ntfy"}}); !strings.Contains(body, "✗ refused: 401") {
		t.Fatalf("failed test: %s", body)
	}
	if _, page := c.get("/settings/notifications"); !strings.Contains(page, "Last send failed") {
		t.Error("the failed test is not shown on the channel")
	}
	if _, body := c.htmx("/settings/notifications/test", url.Values{"name": {"x"}, "kind": {"ntfy"}, "url": {"nope"}}); !strings.Contains(body, "✗ the URL must") {
		t.Fatalf("invalid test: %s", body)
	}

	if resp, body := c.htmx("/settings/notifications/delete", url.Values{"id": {id}}); resp.StatusCode != http.StatusOK || strings.Contains(body, "Phone 2") {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if len(e.channels(t)) != 0 {
		t.Fatal("not deleted")
	}
	evs, _ := e.st.Events(context.Background(), store.EventFilter{Type: "notifications_changed"})
	if len(evs) != 4 || evs[0].Payload["action"] != "deleted" || evs[3].Payload["action"] != "added" {
		t.Errorf("events: %v", evs)
	}
	if resp, _ := viewer.htmx("/settings/notifications", add); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer could add a channel: %d", resp.StatusCode)
	}
}

func TestMapSecretsNotifications(t *testing.T) {
	snap := store.Snapshot{Config: map[string][]store.Row{"settings": {
		{"key": notify.SettingKey, "value": `{"channels":[{"id":"a","url":"u1","token":""},{"id":"b","url":"u2","token":"t2"}]}`},
	}}}
	if err := mapSecrets(snap, func(s string) (string, error) { return "[" + s + "]", nil }); err != nil {
		t.Fatal(err)
	}
	v := snap.Config["settings"][0]["value"].(string)
	for _, want := range []string{`"url":"[u1]"`, `"token":""`, `"url":"[u2]"`, `"token":"[t2]"`} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %s in %s", want, v)
		}
	}
	snap.Config["settings"][0]["value"] = `{"channels":[42]}`
	if err := mapSecrets(snap, func(s string) (string, error) { return s, nil }); err == nil {
		t.Error("malformed channel accepted")
	}
}
