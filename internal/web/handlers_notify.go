// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/zjcochran/wan-drawbridge/internal/notify"
)

// notifyData fills the Notifications card in Settings.
type notifyData struct {
	Channels []channelView
	New      channelView
	Kinds    []choice
	Topics   []choice
}

type channelView struct {
	ID, Name, Kind, KindName, Host string
	HasToken, Enabled              bool
	Topics                         map[string]bool
	Last                           *notify.Result
}

func kindChoices() []choice {
	out := make([]choice, len(notify.Kinds))
	for i, k := range notify.Kinds {
		out[i] = choice{k.ID, k.Name}
	}
	return out
}

func topicChoices() []choice {
	out := make([]choice, len(notify.Topics))
	for i, t := range notify.Topics {
		out[i] = choice{t.ID, t.Name}
	}
	return out
}

func topicSet(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func (s *Server) notifyData(ctx context.Context) (notifyData, error) {
	d := notifyData{Kinds: kindChoices(), Topics: topicChoices(),
		New: channelView{Kind: notify.KindNtfy, Enabled: true, Topics: topicSet(notify.DefaultTopics())}}
	set, err := notify.Load(ctx, s.Store)
	if err != nil {
		return d, err
	}
	for _, c := range set.Channels {
		v := channelView{ID: c.ID, Name: c.Name, Kind: c.Kind, HasToken: c.Token != "", Enabled: c.Enabled, Topics: topicSet(c.Topics)}
		for _, k := range d.Kinds {
			if k.ID == c.Kind {
				v.KindName = k.Name
			}
		}
		// Only the host is shown: the rest of a webhook URL can be its secret.
		if plain, err := s.Secrets.Open(c.URL); err != nil {
			v.Host = "cannot decrypt the saved URL"
		} else if u, err := url.Parse(plain); err == nil {
			v.Host = u.Host
		}
		if r, ok := s.Notify.Last(c.ID); ok {
			v.Last = &r
		}
		d.Channels = append(d.Channels, v)
	}
	return d, nil
}

func (s *Server) renderNotifyList(w http.ResponseWriter, r *http.Request) {
	d, err := s.notifyData(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "settings", "notify-list", d)
}

// channelFromForm builds a channel with plain-text URL and token. Blank URL
// and token fields keep the saved ones, unless remove_token is ticked.
func (s *Server) channelFromForm(r *http.Request, saved *notify.Channel) (notify.Channel, error) {
	if err := r.ParseForm(); err != nil {
		return notify.Channel{}, err
	}
	c := notify.Channel{
		ID: r.PostForm.Get("id"), Name: strings.TrimSpace(r.PostForm.Get("name")), Kind: r.PostForm.Get("kind"),
		URL: strings.TrimSpace(r.PostForm.Get("url")), Token: strings.TrimSpace(r.PostForm.Get("token")),
		Topics: r.PostForm["topics"], Enabled: r.PostForm.Get("enabled") != "",
	}
	if saved != nil {
		old, err := notify.Open(*saved, s.Secrets)
		if err != nil {
			return c, err
		}
		if c.URL == "" {
			c.URL = old.URL
		}
		if c.Token == "" && r.PostForm.Get("remove_token") == "" {
			c.Token = old.Token
		}
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	if c.Kind == notify.KindGotify && c.Token == "" {
		return c, errors.New("a Gotify channel needs an application token")
	}
	return c, nil
}

func findChannel(set notify.Settings, id string) int {
	return slices.IndexFunc(set.Channels, func(c notify.Channel) bool { return c.ID == id })
}

func (s *Server) saveChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set, err := notify.Load(ctx, s.Store)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	i := -1
	var saved *notify.Channel
	if id := r.FormValue("id"); id != "" {
		if i = findChannel(set, id); i < 0 {
			http.Error(w, "that channel no longer exists; reload the page", http.StatusNotFound)
			return
		}
		saved = &set.Channels[i]
	}
	c, err := s.channelFromForm(r, saved)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if c.URL, err = s.Secrets.Seal(c.URL); err == nil {
		c.Token, err = s.Secrets.Seal(c.Token)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	action := "updated"
	if i < 0 {
		b := make([]byte, 8)
		rand.Read(b)
		c.ID, action = hex.EncodeToString(b), "added"
		set.Channels = append(set.Channels, c)
	} else {
		set.Channels[i] = c
	}
	if err := s.Store.SetSetting(ctx, notify.SettingKey, set); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(ctx, "notifications_changed", map[string]any{"channel": c.Name, "action": action, "by": actorName(r)})
	s.renderNotifyList(w, r)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set, err := notify.Load(ctx, s.Store)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if i := findChannel(set, r.FormValue("id")); i >= 0 {
		name := set.Channels[i].Name
		set.Channels = slices.Delete(set.Channels, i, i+1)
		if err := s.Store.SetSetting(ctx, notify.SettingKey, set); err != nil {
			s.fail(w, r, err)
			return
		}
		s.event(ctx, "notifications_changed", map[string]any{"channel": name, "action": "deleted", "by": actorName(r)})
	}
	s.renderNotifyList(w, r)
}

// testChannel sends a test message with the form as it is, saved or not.
func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	set, err := notify.Load(r.Context(), s.Store)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var saved *notify.Channel
	if i := findChannel(set, r.FormValue("id")); i >= 0 {
		saved = &set.Channels[i]
	}
	c, err := s.channelFromForm(r, saved)
	if err == nil {
		err = s.Notify.Test(r.Context(), c)
	}
	msg := "✓ Sent. Check that it arrived."
	if err != nil {
		msg = "✗ " + err.Error()
	}
	s.partial(w, r, "settings", "notify-test", msg)
}
