// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/backup"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

const schedPass = "a-long-backup-passphrase"

func TestScheduleSlot(t *testing.T) {
	b := BackupSchedule{Time: "03:30"}
	at := func(h, m int) time.Time { return time.Date(2027, 1, 15, h, m, 0, 0, time.Local) }
	if got := b.slot(at(9, 0)); !got.Equal(at(3, 30)) {
		t.Errorf("after: %v", got)
	}
	if got := b.slot(at(3, 30)); !got.Equal(at(3, 30)) {
		t.Errorf("at: %v", got)
	}
	if got := b.slot(at(1, 0)); !got.Equal(at(3, 30).AddDate(0, 0, -1)) {
		t.Errorf("before: %v", got)
	}
}

func scheduleEnv(t *testing.T) (*env, *client) {
	e := newEnv(t)
	e.ui.BackupDir = filepath.Join(t.TempDir(), "backups")
	return e, e.admin()
}

// checkScheduledFile checks that a scheduled backup is private and restores
// with the passphrase, and returns its contents.
func checkScheduledFile(t *testing.T, e *env, name string) []byte {
	t.Helper()
	path := filepath.Join(e.ui.BackupDir, name)
	f, _ := fs.ReadFile(os.DirFS(e.ui.BackupDir), name)
	meta, snap, err := backup.Decode(bytes.NewReader(f), schedPass)
	if err != nil || meta.History != "30d" || len(snap.Config["users"]) == 0 {
		t.Fatalf("decode: %+v %v", meta, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
	return f
}

// checkBackupDownloads checks that Settings lists the backup file name and
// that only admins can download it, and only files Drawbridge wrote.
func checkBackupDownloads(t *testing.T, e *env, c *client, name, content string) {
	t.Helper()
	_, page := c.get("/settings/backup")
	if !strings.Contains(page, name) || !strings.Contains(page, "Last scheduled backup") {
		t.Error("the settings page does not list the backups")
	}
	resp, body := c.get("/settings/backups/" + name)
	if resp.StatusCode != http.StatusOK || body != content || !strings.Contains(resp.Header.Get("Content-Disposition"), name) {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	for _, bad := range []string{"notes.txt", "..%2Fdb", "drawbridge-backup-x.json"} {
		if resp, _ := c.get("/settings/backups/" + bad); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := e.viewerClient().get("/settings/backups/" + name); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer download: %d", resp.StatusCode)
	}
}

func saveSchedule(t *testing.T, c *client, v url.Values) (*http.Response, string) {
	t.Helper()
	form := url.Values{"enabled": {"on"}, "time": {"03:00"}, "keep": {"2"}, "history": {"30"},
		"passphrase": {schedPass}, "confirm": {schedPass}}
	for k, vs := range v {
		form[k] = vs
	}
	return c.htmx("/settings/backup/schedule", form)
}

func TestScheduleValidation(t *testing.T) {
	_, c := scheduleEnv(t)
	for name, v := range map[string]url.Values{
		"bad time":      {"time": {"3am"}},
		"keep 0":        {"keep": {"0"}},
		"keep text":     {"keep": {"lots"}},
		"bad history":   {"history": {"7"}},
		"mismatch":      {"confirm": {"something-else-entirely"}},
		"short":         {"passphrase": {"short"}, "confirm": {"short"}},
		"no passphrase": {"passphrase": {""}, "confirm": {""}},
	} {
		if resp, _ := saveSchedule(t, c, v); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := c.htmx("/settings/backup/run", nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("run without a passphrase: %d", resp.StatusCode)
	}
}

func TestScheduledBackups(t *testing.T) {
	e, c := scheduleEnv(t)
	ctx := context.Background()
	resp, body := saveSchedule(t, c, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Saved. Leave blank") || !strings.Contains(body, "Back up now") {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	var stored BackupSchedule
	e.st.GetSetting(ctx, SettingBackupSchedule, &stored)
	if !strings.HasPrefix(stored.Passphrase, "v1:") {
		t.Fatalf("passphrase stored as %q", stored.Passphrase)
	}
	// Saving again with a blank passphrase keeps it.
	saveSchedule(t, c, url.Values{"passphrase": {""}, "confirm": {""}, "keep": {"2"}})
	e.st.GetSetting(ctx, SettingBackupSchedule, &stored)
	if stored.Passphrase == "" || stored.Keep != 2 {
		t.Fatalf("kept: %+v", stored)
	}

	// Due: never ran, so the first check makes one.
	clock := time.Date(2027, 1, 15, 9, 0, 0, 0, time.Local)
	e.ui.now = func() time.Time { return clock }
	e.ui.scheduledBackupDue(ctx)
	files, _ := e.ui.backupFiles()
	if len(files) != 1 {
		t.Fatalf("files after first run: %v", files)
	}
	// Not due again until tomorrow 03:00.
	clock = clock.Add(time.Hour)
	e.ui.scheduledBackupDue(ctx)
	if files, _ = e.ui.backupFiles(); len(files) != 1 {
		t.Fatalf("ran twice in one day: %v", files)
	}
	os.WriteFile(filepath.Join(e.ui.BackupDir, "notes.txt"), []byte("mine"), 0o600)
	for i := range 2 {
		clock = clock.AddDate(0, 0, 1).Add(time.Duration(i) * time.Minute)
		e.ui.scheduledBackupDue(ctx)
	}
	files, _ = e.ui.backupFiles()
	if len(files) != 2 || files[0].Name != backupName(clock) {
		t.Fatalf("kept %v, want the newest 2", files)
	}
	if _, err := os.Stat(filepath.Join(e.ui.BackupDir, "notes.txt")); err != nil {
		t.Error("pruning removed a file that is not a backup")
	}

	f := checkScheduledFile(t, e, files[0].Name)
	evs, _ := e.st.Events(ctx, store.EventFilter{Type: "backup_created"})
	if len(evs) != 3 || evs[0].Payload["by"] != "schedule" || evs[0].Payload["file"] != files[0].Name {
		t.Fatalf("events: %v", evs)
	}

	// Page and download, on the real clock so the session is valid.
	e.ui.now = time.Now
	checkBackupDownloads(t, e, c, files[0].Name, string(f))

	// Back up now ignores the schedule.
	if _, body := c.htmx("/settings/backup/run", nil); !strings.Contains(body, "drawbridge-backup-"+time.Now().UTC().Format("20060102")) {
		t.Fatalf("run now: %s", body)
	}
}

func TestScheduledBackupFailure(t *testing.T) {
	e, c := scheduleEnv(t)
	ctx := context.Background()
	saveSchedule(t, c, nil)
	os.WriteFile(e.ui.BackupDir, []byte("a file where the folder should be"), 0o600)
	last := e.ui.scheduledBackup(ctx)
	if last.Error == "" || !last.Success.IsZero() {
		t.Fatalf("last = %+v", last)
	}
	evs, _ := e.st.Events(ctx, store.EventFilter{Type: "backup_failed"})
	if len(evs) != 1 || evs[0].Payload["by"] != "schedule" {
		t.Fatalf("events: %v", evs)
	}
	if _, body := c.get("/settings/backup"); !strings.Contains(body, "failed: ") || !strings.Contains(body, "Cannot list") {
		t.Error("the failure is not shown")
	}
	// A failed run is not retried every minute.
	e.ui.scheduledBackupDue(ctx)
	if evs, _ := e.st.Events(ctx, store.EventFilter{Type: "backup_failed"}); len(evs) != 1 {
		t.Errorf("retried at once: %d failures", len(evs))
	}
	// A passphrase that cannot be decrypted fails too.
	var b BackupSchedule
	e.st.GetSetting(ctx, SettingBackupSchedule, &b)
	b.Passphrase = "v1:garbage"
	e.st.SetSetting(ctx, SettingBackupSchedule, b)
	if last := e.ui.scheduledBackup(ctx); !strings.Contains(last.Error, "malformed") {
		t.Errorf("bad passphrase: %+v", last)
	}
}

func TestRunBackupScheduleStops(t *testing.T) {
	e, _ := scheduleEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.ui.RunBackupSchedule(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

func TestMountedOrUnknown(t *testing.T) {
	if !mountedOrUnknown("/proc/self") { // a mount on Linux; unknown elsewhere
		t.Error("/proc is mounted")
	}
	mountedOrUnknown(t.TempDir()) // either answer is fine; it must not fail
}
