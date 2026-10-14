// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testMessage() Message {
	return Message{Event: "failover", Title: "Failed over", Text: "Traffic is on SKYLINK.", Priority: 4, Time: time.Unix(1_800_000_000, 0)}
}

func TestPushover(t *testing.T) {
	n := newNotifier(t)
	r := newReceiver(t)
	old := PushoverURL
	PushoverURL = r.srv.URL + "/1/messages.json"
	defer func() { PushoverURL = old }()
	c := Channel{Name: "p", Kind: KindPushover, Token: "app-token", User: "user-key"}
	if err := n.send(context.Background(), c, testMessage()); err != nil {
		t.Fatal(err)
	}
	h := r.got()[0]
	form, _ := url.ParseQuery(h.body)
	if h.path != "/1/messages.json" || form.Get("token") != "app-token" || form.Get("user") != "user-key" ||
		form.Get("title") != "Failed over" || form.Get("priority") != "1" || form.Get("timestamp") != "1800000000" {
		t.Errorf("pushover request = %s %v", h.path, form)
	}
}

func TestTelegram(t *testing.T) {
	n := newNotifier(t)
	r := newReceiver(t)
	c := Channel{Name: "t", Kind: KindTelegram, URL: r.srv.URL + "/", Token: "123:abc", To: "-100200"}
	if err := n.send(context.Background(), c, testMessage()); err != nil {
		t.Fatal(err)
	}
	h := r.got()[0]
	var body map[string]any
	json.Unmarshal([]byte(h.body), &body)
	if h.path != "/bot123:abc/sendMessage" || body["chat_id"] != "-100200" || body["text"] != "Failed over\n\nTraffic is on SKYLINK." {
		t.Errorf("telegram request = %s %v", h.path, body)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestErrorsHideTheURLPath(t *testing.T) {
	n := newNotifier(t)
	n.Client = &http.Client{Transport: failingTransport{}}
	for _, c := range []Channel{
		{Name: "t", Kind: KindTelegram, Token: "123:secret-bot-token", To: "1"},
		{Name: "d", Kind: KindDiscord, URL: "https://discord.example/api/webhooks/1/secret-webhook-token"},
	} {
		err := n.send(context.Background(), c, testMessage())
		if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("%s: error %v", c.Kind, err)
		}
	}
}

func TestValidateKinds(t *testing.T) {
	good := []Channel{
		{Name: "x", Kind: KindPushover, Token: "a", User: "u"},
		{Name: "x", Kind: KindTelegram, Token: "a", To: "1"},
		{Name: "x", Kind: KindTelegram, Token: "a", To: "1", URL: "https://bot.example"},
		{Name: "x", Kind: KindEmail, URL: "smtp://mail.example.com", From: "drawbridge@example.com", To: "alice@example.com, bob@example.com"},
		{Name: "x", Kind: KindEmail, URL: "smtps://mail.example.com:465", From: "Drawbridge <drawbridge@example.com>", To: "alice@example.com"},
	}
	for _, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := []Channel{
		{Name: "x", Kind: KindPushover, Token: "a"},
		{Name: "x", Kind: KindTelegram, Token: "a"},
		{Name: "x", Kind: KindTelegram, Token: "a", To: "1", URL: "ftp://bot.example"},
		{Name: "x", Kind: KindGotify, URL: "https://gotify.example"},
		{Name: "x", Kind: KindEmail, URL: "https://mail.example.com", From: "a@example.com", To: "b@example.com"},
		{Name: "x", Kind: KindEmail, URL: "smtp://mail.example.com", From: "nobody", To: "b@example.com"},
		{Name: "x", Kind: KindEmail, URL: "smtp://mail.example.com", From: "a@example.com", To: "bob"},
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
}
