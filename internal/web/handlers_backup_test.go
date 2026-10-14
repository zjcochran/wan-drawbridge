// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/backup"
	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

const passphrase = "test-passphrase-123"

// download makes a backup through the Settings form.
func (c *client) download(history, pass, confirm string) (*http.Response, string) {
	return c.do("POST", "/settings/backup", url.Values{
		"_csrf": {c.csrf}, "history": {history}, "passphrase": {pass}, "confirm": {confirm},
	})
}

// upload posts a backup file the way the restore forms do.
func (c *client) upload(path string, file []byte, pass string, headers ...string) (*http.Response, string) {
	c.e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if file != nil {
		fw, _ := mw.CreateFormFile("file", "backup.json")
		fw.Write(file)
	}
	mw.WriteField("passphrase", pass)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, c.e.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// withSSO stores single sign-on settings with a sealed client secret.
func (e *env) withSSO(secret string) {
	sealed, err := e.ui.Secrets.Seal(secret)
	if err != nil {
		e.t.Fatal(err)
	}
	o := OIDCSettings{Enabled: true, Issuer: "https://id.example.com", ClientID: "drawbridge", ClientSecret: sealed}
	if err := e.st.SetSetting(context.Background(), SettingOIDC, o); err != nil {
		e.t.Fatal(err)
	}
}

func TestDownloadBackup(t *testing.T) {
	e := newEnv(t)
	e.withSSO("client-secret-value")
	c := e.admin()
	resp, body := c.download("30", passphrase, passphrase)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), `attachment; filename="drawbridge-backup-`) {
		t.Fatalf("download: %d %v", resp.StatusCode, resp.Header)
	}
	meta, snap, err := backup.Decode(strings.NewReader(body), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	if meta.History != "30d" || meta.AppVersion != "dev" || meta.Schema == 0 || snap.History == nil {
		t.Fatalf("meta = %+v", meta)
	}
	// The secret travels in the clear inside the encryption, so another
	// instance with another key can use it.
	var found bool
	for _, row := range snap.Config["settings"] {
		if row["key"] == SettingOIDC {
			found = strings.Contains(row["value"].(string), `"client_secret":"client-secret-value"`)
		}
	}
	if !found {
		t.Fatal("client secret not exported in the clear")
	}
	if _, body := c.get("/events"); !strings.Contains(body, "backup_created") {
		t.Error("no backup_created event")
	}
	if resp, body := c.download("none", passphrase, passphrase); resp.StatusCode != http.StatusOK {
		t.Fatalf("config only: %d", resp.StatusCode)
	} else if _, snap, _ := backup.Decode(strings.NewReader(body), passphrase); snap.History != nil {
		t.Fatal("config-only backup has history")
	}
}

func TestDownloadBackupProblems(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	for name, form := range map[string][3]string{
		"do not match": {"none", passphrase, passphrase + "x"},
		"at least 12":  {"none", "short", "short"},
		"unknown":      {"7", passphrase, passphrase},
	} {
		resp, _ := c.download(form[0], form[1], form[2])
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/settings/backup?flash=") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, loc)
			continue
		}
		if msg, _ := url.QueryUnescape(strings.TrimPrefix(loc, "/settings/backup?flash=")); !strings.Contains(msg, name) {
			t.Errorf("%s: message %q", name, msg)
		}
	}
	viewer := e.viewerClient()
	if resp, _ := viewer.download("none", passphrase, passphrase); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer download: %d", resp.StatusCode)
	}
}

// viewerClient signs in as a viewer created by the admin.
func (e *env) viewerClient() *client {
	if _, err := e.st.CreateUser(context.Background(), "viewer", "test-password-123", store.RoleViewer, time.Now()); err != nil {
		e.t.Fatal(err)
	}
	c := e.client()
	c.do("POST", "/login", url.Values{"username": {"viewer"}, "password": {"test-password-123"}})
	c.get("/settings")
	return c
}

// backupOf returns a backup file of another environment: admin "alice",
// single sign-on with a secret, and the ISP "BIGTEL".
func backupOf(t *testing.T) []byte {
	t.Helper()
	src := newEnv(t)
	ctx := context.Background()
	src.withSSO("moved-secret")
	src.st.CreateUser(ctx, "alice", "test-password-123", store.RoleAdmin, time.Now())
	src.st.SeeASN(ctx, 64511, "BIGTEL-64511", time.Now())
	c := src.client()
	c.do("POST", "/login", url.Values{"username": {"alice"}, "password": {"test-password-123"}})
	c.get("/settings")
	resp, body := c.download("all", passphrase, passphrase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("source backup: %d", resp.StatusCode)
	}
	return []byte(body)
}

func TestRestoreFromSettings(t *testing.T) {
	file := backupOf(t)
	e := newEnv(t)
	c := e.admin()
	if resp, body := c.upload("/settings/restore", file, "wrong passphrase!", "HX-Request", "true", "X-CSRF-Token", c.csrf); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "wrong passphrase") {
		t.Fatalf("wrong passphrase: %d %s", resp.StatusCode, body)
	}
	resp, _ := c.upload("/settings/restore", file, passphrase, "HX-Request", "true", "X-CSRF-Token", c.csrf)
	if resp.StatusCode != http.StatusNoContent || !strings.HasPrefix(resp.Header.Get("HX-Redirect"), "/login?flash=Backup+restored") {
		t.Fatalf("restore: %d %v", resp.StatusCode, resp.Header)
	}
	// Everyone is signed out; the old admin is gone, the backup's is back.
	if resp, _ := c.do("GET", "/settings", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("still signed in: %d", resp.StatusCode)
	}
	ctx := context.Background()
	if _, err := e.st.Authenticate(ctx, "admin", "test-password-123", time.Now()); err == nil {
		t.Error("the replaced admin can still sign in")
	}
	if _, err := e.st.Authenticate(ctx, "alice", "test-password-123", time.Now()); err != nil {
		t.Errorf("restored admin: %v", err)
	}
	if names, _ := e.st.ISPNames(ctx); !strings.Contains(strings.Join(mapValues(names), ","), "BIGTEL") {
		t.Errorf("ISPs not restored: %v", names)
	}
	o, err := e.ui.oidcSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if secret, err := e.ui.Secrets.Open(o.ClientSecret); err != nil || secret != "moved-secret" {
		t.Errorf("client secret after restore: %q %v", secret, err)
	}
	events, _ := e.st.Events(ctx, store.EventFilter{Type: "backup_restored", Limit: 10})
	if len(events) != 1 {
		t.Errorf("backup_restored events: %d", len(events))
	}
}

func mapValues(m map[int]string) []string {
	out := []string{}
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestRestoreProblems(t *testing.T) {
	file := backupOf(t)
	e := newEnv(t)
	c := e.admin()
	hx := []string{"HX-Request", "true", "X-CSRF-Token", c.csrf}
	if resp, body := c.upload("/settings/restore", nil, passphrase, hx...); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "choose a backup file") {
		t.Errorf("no file: %d %s", resp.StatusCode, body)
	}
	if resp, body := c.upload("/settings/restore", []byte("not a backup"), passphrase, hx...); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "not a Drawbridge backup") {
		t.Errorf("other file: %d %s", resp.StatusCode, body)
	}
	big := bytes.Repeat([]byte(" "), maxBackupUpload+1)
	if resp, body := c.upload("/settings/restore", big, passphrase, hx...); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "larger than") {
		t.Errorf("too large: %d %s", resp.StatusCode, body)
	}
	e.mon.loadErr = monitor.ErrStopped
	if resp, body := c.upload("/settings/restore", file, passphrase, hx...); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "while containers are stopped") {
		t.Errorf("during failover: %d %s", resp.StatusCode, body)
	}
	e.mon.loadErr = nil
	if resp, _ := c.upload("/settings/restore", file, passphrase); resp.StatusCode != http.StatusForbidden {
		t.Errorf("without CSRF token: %d", resp.StatusCode)
	}
	viewer := e.viewerClient()
	if resp, _ := viewer.upload("/settings/restore", file, passphrase, "HX-Request", "true", "X-CSRF-Token", viewer.csrf); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer restore: %d", resp.StatusCode)
	}
}

func TestRestoreRejectsNewerBackup(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	snap, _ := e.st.Export(ctx, false, time.Time{})
	var buf bytes.Buffer
	backup.Encode(&buf, backup.Meta{AppVersion: "v9.0.0", Schema: 9999, History: "none"}, snap, passphrase)
	resp, body := c.upload("/settings/restore", buf.Bytes(), passphrase, "HX-Request", "true", "X-CSRF-Token", c.csrf)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "newer version of Drawbridge (v9.0.0)") {
		t.Fatalf("newer backup: %d %s", resp.StatusCode, body)
	}
}

func TestRestoreInSetupWizard(t *testing.T) {
	file := backupOf(t)
	e := newEnv(t)
	c := e.client()
	if _, body := c.get("/setup"); !strings.Contains(body, "Or restore a backup") {
		t.Fatal("step 1 does not offer a restore")
	}
	resp, body := c.upload("/setup/restore", file, "wrong passphrase!")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "wrong passphrase") || !strings.Contains(body, "Create the admin account") {
		t.Fatalf("wrong passphrase: %d", resp.StatusCode)
	}
	resp, _ = c.upload("/setup/restore", file, passphrase)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?flash=") {
		t.Fatalf("restore: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := c.get(resp.Header.Get("Location")); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Backup restored") {
		t.Fatalf("login page: %d", resp.StatusCode)
	}
	if _, err := e.st.Authenticate(context.Background(), "alice", "test-password-123", time.Now()); err != nil {
		t.Fatalf("restored admin: %v", err)
	}
	// Once accounts exist, the wizard's restore is closed.
	if resp, _ := e.client().upload("/setup/restore", file, passphrase); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second wizard restore: %d", resp.StatusCode)
	}
}

func TestMapTwoFactorSecrets(t *testing.T) {
	snap := store.Snapshot{Config: map[string][]store.Row{"users": {
		{"username": "alice", "totp_secret": "abc"}, {"username": "bob", "totp_secret": ""},
	}}}
	if err := mapSecrets(snap, func(s string) (string, error) { return strings.ToUpper(s), nil }); err != nil {
		t.Fatal(err)
	}
	if u := snap.Config["users"]; u[0]["totp_secret"] != "ABC" || u[1]["totp_secret"] != "" {
		t.Errorf("users = %v", u)
	}
	fail := errors.New("cannot open")
	if err := mapSecrets(snap, func(string) (string, error) { return "", fail }); !errors.Is(err, fail) {
		t.Errorf("error: %v", err)
	}
}

func TestMapClientSecret(t *testing.T) {
	snap := store.Snapshot{Config: map[string][]store.Row{"settings": {
		{"key": "detection", "value": `{"quorum":2}`},
		{"key": SettingOIDC, "value": `{"enabled":true,"client_secret":""}`},
	}}}
	called := false
	if err := mapSecrets(snap, func(s string) (string, error) { called = true; return s, nil }); err != nil || called {
		t.Fatalf("empty secret: %v called=%v", err, called)
	}
	snap.Config["settings"][1]["value"] = `{"client_secret":"abc","issuer":"https://id.example.com"}`
	if err := mapSecrets(snap, func(s string) (string, error) { return strings.ToUpper(s), nil }); err != nil {
		t.Fatal(err)
	}
	if v := snap.Config["settings"][1]["value"].(string); !strings.Contains(v, `"client_secret":"ABC"`) || !strings.Contains(v, "id.example.com") {
		t.Fatalf("value = %s", v)
	}
	fail := errors.New("cannot seal")
	if err := mapSecrets(snap, func(string) (string, error) { return "", fail }); !errors.Is(err, fail) {
		t.Fatalf("seal error: %v", err)
	}
	for _, bad := range []any{42, "not json"} {
		snap.Config["settings"][1]["value"] = bad
		if err := mapSecrets(snap, func(s string) (string, error) { return s, nil }); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
}
