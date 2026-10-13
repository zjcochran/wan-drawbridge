// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/backup"
)

// Settings rows for scheduled backups.
const (
	SettingBackupSchedule = "backup_schedule"      // BackupSchedule as JSON
	settingBackupLast     = "backup_schedule_last" // backupLast as JSON
)

// BackupSchedule makes an encrypted backup every day at Time (server local
// time) into the backup folder, keeping the newest Keep files.
type BackupSchedule struct {
	Enabled    bool   `json:"enabled"`
	Time       string `json:"time"`       // "HH:MM"
	Keep       int    `json:"keep"`       // files to keep, 1 to 365
	History    string `json:"history"`    // a backup.HistoryRange choice
	Passphrase string `json:"passphrase"` // sealed with the secret key
}

func defaultSchedule() BackupSchedule {
	return BackupSchedule{Time: "03:00", Keep: 14, History: "none"}
}

// backupLast is the outcome of the latest scheduled backup.
type backupLast struct {
	At      time.Time `json:"at"`      // when it ran
	File    string    `json:"file"`    // the file written; "" on failure
	Error   string    `json:"error"`   // why it failed
	Success time.Time `json:"success"` // the latest run that worked
}

var scheduleTime = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// backupFile matches the files Drawbridge writes and prunes; nothing else in
// the folder is touched.
var backupFile = regexp.MustCompile(`^drawbridge-backup-[0-9]{8}-[0-9]{6}\.json$`)

// slot returns the latest scheduled time at or before now.
func (b BackupSchedule) slot(now time.Time) time.Time {
	h, _ := strconv.Atoi(b.Time[:2])
	m, _ := strconv.Atoi(b.Time[3:])
	now = now.Local()
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

func (s *Server) backupSchedule(ctx context.Context) (BackupSchedule, backupLast, error) {
	b, last := defaultSchedule(), backupLast{}
	if _, err := s.Store.GetSetting(ctx, SettingBackupSchedule, &b); err != nil {
		return b, last, err
	}
	_, err := s.Store.GetSetting(ctx, settingBackupLast, &last)
	return b, last, err
}

// RunBackupSchedule makes scheduled backups until ctx is cancelled. It checks
// every minute, and once at start, so a backup missed while Drawbridge was
// down is made when it starts.
func (s *Server) RunBackupSchedule(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.scheduledBackupDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) scheduledBackupDue(ctx context.Context) {
	b, last, err := s.backupSchedule(ctx)
	if err != nil {
		s.Log.Error("cannot read the backup schedule", "err", err)
		return
	}
	if b.Enabled && last.At.Before(b.slot(s.now())) {
		s.scheduledBackup(ctx)
	}
}

// scheduledBackup makes one backup now with the saved schedule's settings,
// records the outcome and prunes old files. Failures are not retried until
// the next scheduled time, so a broken setup does not repeat every minute.
func (s *Server) scheduledBackup(ctx context.Context) backupLast {
	b, last, err := s.backupSchedule(ctx)
	last.At, last.File, last.Error = s.now(), "", ""
	if err == nil {
		last.File, err = s.writeScheduledBackup(ctx, b)
	}
	if err != nil {
		last.Error = err.Error()
		s.Log.Error("scheduled backup failed", "err", err)
		s.event(ctx, "backup_failed", map[string]any{"error": last.Error, "by": "schedule"})
	} else {
		last.Success = last.At
		s.Log.Info("scheduled backup written", "file", last.File)
		s.event(ctx, "backup_created", map[string]any{"history": historyLabel(b.History), "by": "schedule", "file": last.File})
	}
	if err := s.Store.SetSetting(ctx, settingBackupLast, last); err != nil {
		s.Log.Error("cannot record the scheduled backup", "err", err)
	}
	return last
}

func historyLabel(choice string) string {
	_, _, label, _ := backup.HistoryRange(choice, time.Time{})
	return label
}

func (s *Server) writeScheduledBackup(ctx context.Context, b BackupSchedule) (string, error) {
	pass, err := s.Secrets.Open(b.Passphrase)
	if err != nil {
		return "", err
	}
	if pass == "" {
		return "", errors.New("no passphrase is saved")
	}
	if err := os.MkdirAll(s.BackupDir, 0o750); err != nil {
		return "", err
	}
	// Written under a temporary name and renamed, so a half-written file
	// never looks like a backup.
	tmp, err := os.CreateTemp(s.BackupDir, ".partial-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	meta, err := s.encodeBackup(ctx, tmp, b.History, pass)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	name := backupName(meta.Created)
	if err := os.Rename(tmp.Name(), filepath.Join(s.BackupDir, name)); err != nil {
		return "", err
	}
	if err := s.pruneBackups(b.Keep); err != nil {
		s.Log.Warn("cannot remove old backups", "err", err)
	}
	return name, nil
}

type backupFileInfo struct {
	Name string
	Size int64
	Time time.Time
}

// backupFiles lists the backup files in the folder, newest first.
func (s *Server) backupFiles() ([]backupFileInfo, error) {
	entries, err := os.ReadDir(s.BackupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []backupFileInfo
	for _, e := range entries {
		if !e.Type().IsRegular() || !backupFile.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backupFileInfo{Name: e.Name(), Size: info.Size(), Time: info.ModTime()})
	}
	// The name holds the UTC time, so name order is time order.
	slices.SortFunc(out, func(a, b backupFileInfo) int { return strings.Compare(b.Name, a.Name) })
	return out, nil
}

func (s *Server) pruneBackups(keep int) error {
	files, err := s.backupFiles()
	if err != nil || len(files) <= keep {
		return err
	}
	var errs []error
	for _, f := range files[keep:] {
		errs = append(errs, os.Remove(filepath.Join(s.BackupDir, f.Name)))
	}
	return errors.Join(errs...)
}

// mountedOrUnknown reports whether dir is on a mount of its own (a volume),
// using /proc/self/mountinfo; true when that cannot be read (not Linux).
func mountedOrUnknown(dir string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return true
	}
	defer f.Close()
	dir = filepath.Clean(dir)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[4] == "/" {
			continue
		}
		mp := fields[4]
		if dir == mp || strings.HasPrefix(dir, mp+"/") {
			return true
		}
	}
	return false
}

// ---- handlers ----

type scheduleView struct {
	BackupSchedule
	HasPassphrase bool
	Last          backupLast
	Files         []backupFileInfo
	Dir           string
	NotMounted    bool
	Error         string
}

func (s *Server) scheduleView(ctx context.Context) (scheduleView, error) {
	b, last, err := s.backupSchedule(ctx)
	if err != nil {
		return scheduleView{}, err
	}
	v := scheduleView{BackupSchedule: b, HasPassphrase: b.Passphrase != "", Last: last, Dir: s.BackupDir,
		NotMounted: !mountedOrUnknown(s.BackupDir)}
	v.Files, err = s.backupFiles()
	if err != nil {
		v.Error = "Cannot list " + s.BackupDir + ": " + err.Error()
	}
	return v, nil
}

func (s *Server) renderSchedule(w http.ResponseWriter, r *http.Request, msg string) {
	v, err := s.scheduleView(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if msg != "" {
		v.Error = msg
	}
	s.partial(w, r, "settings", "backup-schedule", v)
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, _, err := s.backupSchedule(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	keep, err := strconv.Atoi(r.FormValue("keep"))
	switch {
	case !scheduleTime.MatchString(r.FormValue("time")):
		invalid(w, "Enter the time as HH:MM, for example 03:00.")
		return
	case err != nil || keep < 1 || keep > 365:
		invalid(w, "Keep between 1 and 365 backups.")
		return
	}
	if _, _, _, err := backup.HistoryRange(r.FormValue("history"), s.now()); err != nil {
		invalid(w, err.Error())
		return
	}
	if pass := r.FormValue("passphrase"); pass != "" {
		if pass != r.FormValue("confirm") {
			invalid(w, "The passphrases do not match.")
			return
		}
		if len(pass) < backup.MinPassphrase {
			invalid(w, fmt.Sprintf("The passphrase must be at least %d characters.", backup.MinPassphrase))
			return
		}
		if b.Passphrase, err = s.Secrets.Seal(pass); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	b.Enabled, b.Time, b.Keep, b.History = r.FormValue("enabled") != "", r.FormValue("time"), keep, r.FormValue("history")
	if b.Enabled && b.Passphrase == "" {
		invalid(w, "Set a passphrase for the scheduled backups.")
		return
	}
	if err := s.Store.SetSetting(ctx, SettingBackupSchedule, b); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(ctx, "backup_schedule", map[string]any{"enabled": b.Enabled, "time": b.Time, "keep": b.Keep, "history": historyLabel(b.History), "by": actorName(r)})
	s.renderSchedule(w, r, "")
}

func (s *Server) runBackupNow(w http.ResponseWriter, r *http.Request) {
	b, _, err := s.backupSchedule(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if b.Passphrase == "" {
		invalid(w, "Save a passphrase for the scheduled backups first.")
		return
	}
	last := s.scheduledBackup(r.Context())
	s.renderSchedule(w, r, last.Error)
}

// downloadStoredBackup sends one of the files in the backup folder.
func (s *Server) downloadStoredBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !backupFile.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(s.BackupDir, name))
}
