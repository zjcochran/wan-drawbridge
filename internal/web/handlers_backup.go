// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/backup"
	"github.com/zjcochran/wan-drawbridge/internal/notify"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// maxBackupUpload is the largest backup file accepted for a restore.
const maxBackupUpload = 128 << 20

// limitBody caps the request body before anything reads it.
func limitBody(n int64, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, n)
		h.ServeHTTP(w, r)
	})
}

// downloadBackup sends an encrypted backup file. It is a plain form post, so
// the browser saves the response; problems go back to Settings as a message.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := func(msg string) { redirect(w, r, "/settings/backup?flash="+url.QueryEscape(msg)) }
	pass := r.PostFormValue("passphrase")
	if pass != r.PostFormValue("confirm") {
		back("The passphrases do not match. No backup was made.")
		return
	}
	if len(pass) < backup.MinPassphrase {
		back(fmt.Sprintf("The passphrase must be at least %d characters. No backup was made.", backup.MinPassphrase))
		return
	}
	var buf bytes.Buffer
	meta, err := s.encodeBackup(ctx, &buf, r.PostFormValue("history"), pass)
	if errors.Is(err, backup.ErrHistoryRange) {
		back(err.Error())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(ctx, "backup_created", map[string]any{"history": meta.History, "by": actorName(r)})
	name := backupName(meta.Created)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes()) //nolint:errcheck,gosec // G705: a download, not HTML; nothing to do if the client went away
}

// encodeBackup writes an encrypted backup with the given history choice
// ("none", "30", "60", "90", "all") to w.
func (s *Server) encodeBackup(ctx context.Context, w io.Writer, history, pass string) (backup.Meta, error) {
	include, since, label, err := backup.HistoryRange(history, s.now())
	if err != nil {
		return backup.Meta{}, err
	}
	snap, err := s.Store.Export(ctx, include, since)
	if err != nil {
		return backup.Meta{}, err
	}
	schema, err := s.Store.SchemaVersion(ctx)
	if err != nil {
		return backup.Meta{}, err
	}
	// Stored secrets are sealed with this instance's key; the backup carries
	// them in the clear, inside its own encryption, so another instance can
	// seal them with its key.
	if err := mapSecrets(snap, s.Secrets.Open); err != nil {
		return backup.Meta{}, err
	}
	meta := backup.Meta{AppVersion: s.Version, Schema: schema, Created: s.now().UTC(), History: label}
	if meta.AppVersion == "" {
		meta.AppVersion = "dev"
	}
	return meta, backup.Encode(w, meta, snap, pass)
}

// backupName is the file name for a backup made at t.
func backupName(t time.Time) string {
	return "drawbridge-backup-" + t.UTC().Format("20060102-150405") + ".json"
}

// restoreBackup replaces the configuration from Settings. Everyone is signed
// out, because the users are replaced.
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.restoreFrom(r, actorName(r)); err != nil {
		invalid(w, err.Error())
		return
	}
	redirect(w, r, "/login?flash="+url.QueryEscape("Backup restored. Sign in with an account from the backup."))
}

// setupRestore restores a backup instead of creating the first admin.
func (s *Server) setupRestore(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n > 0 {
		http.Error(w, "Drawbridge is already set up; restore from Settings", http.StatusForbidden)
		return
	}
	if err := s.restoreFrom(r, "setup"); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.page(w, r, "setup", "Set up Drawbridge", setupData{Step: 1, Steps: setupSteps, RestoreError: err.Error()})
		return
	}
	redirect(w, r, "/login?flash="+url.QueryEscape("Backup restored. Sign in with an account from the backup."))
}

// restoreFrom reads the uploaded file and passphrase and restores the backup.
// Every error it returns is meant for the person restoring.
func (s *Server) restoreFrom(r *http.Request, actor string) error {
	ctx := r.Context()
	if err := r.ParseMultipartForm(maxBackupUpload); err != nil {
		if mbe := new(http.MaxBytesError); errors.As(err, &mbe) {
			return fmt.Errorf("the file is larger than %d MB", maxBackupUpload>>20)
		}
		return errors.New("choose a backup file")
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return errors.New("choose a backup file")
	}
	defer f.Close()
	meta, snap, err := backup.Decode(f, r.PostFormValue("passphrase"))
	if err != nil {
		return err
	}
	schema, err := s.Store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if meta.Schema > schema {
		return fmt.Errorf("this backup was made by a newer version of Drawbridge (%s); upgrade Drawbridge first", meta.AppVersion)
	}
	if err := mapSecrets(snap, s.Secrets.Seal); err != nil {
		return fmt.Errorf("the single sign-on client secret in the backup cannot be stored: %w", err)
	}
	err = s.Mon.LoadBackup(ctx, func(ctx context.Context) error {
		if err := s.Store.Import(ctx, snap); err != nil {
			return err
		}
		return s.Store.AddEvent(ctx, s.now(), "backup_restored", map[string]any{
			"created": meta.Created, "app_version": meta.AppVersion, "history": meta.History, "by": actor,
		})
	})
	if err != nil {
		return fmt.Errorf("the backup was not restored: %w", err)
	}
	s.Log.Info("backup restored", "created", meta.Created, "app_version", meta.AppVersion, "history", meta.History, "by", actor)
	return nil
}

// mapSecrets rewrites every sealed value in a snapshot's settings with fn:
// the single sign-on client secret, each notification channel's URL, token
// and user, and each user's two-factor secret. Open on export puts them in the encrypted backup as plain text; Seal
// on restore encrypts them with this instance's key.
func mapSecrets(snap store.Snapshot, fn func(string) (string, error)) error {
	for _, row := range snap.Config["users"] {
		if err := mapField(row, "totp_secret", fn); err != nil {
			return err
		}
	}
	for _, row := range snap.Config["settings"] {
		key, _ := row["key"].(string)
		if key != SettingOIDC && key != notify.SettingKey {
			continue
		}
		raw, ok := row["value"].(string)
		if !ok {
			return fmt.Errorf("malformed %s settings", key)
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var o map[string]any
		if err := dec.Decode(&o); err != nil {
			return err
		}
		var err error
		if key == SettingOIDC {
			err = mapField(o, "client_secret", fn)
		} else {
			chans, _ := o["channels"].([]any)
			for _, c := range chans {
				ch, ok := c.(map[string]any)
				if !ok {
					return errors.New("malformed notification settings")
				}
				for _, f := range []string{"url", "token", "user"} {
					if err == nil {
						err = mapField(ch, f, fn)
					}
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return err
		}
		b, err := json.Marshal(o)
		if err != nil {
			return err
		}
		row["value"] = string(b)
	}
	return nil
}

// mapField applies fn to a non-empty string field.
func mapField(o map[string]any, k string, fn func(string) (string, error)) error {
	v, _ := o[k].(string)
	if v == "" {
		return nil
	}
	out, err := fn(v)
	if err == nil {
		o[k] = out
	}
	return err
}
