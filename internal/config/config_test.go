// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	t.Setenv("DRAWBRIDGE_DB_DIR", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBDir != "/data/db" || c.CacheDir != "/data/cache" || c.BackupDir != "/data/backups" || c.Listen != ":8080" || c.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if !reflect.DeepEqual(c.Detection, DefaultDetection()) {
		t.Fatalf("detection defaults: %+v", c.Detection)
	}
}

func TestLoadFromEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"DRAWBRIDGE_DB_DIR":            "/db",
		"DRAWBRIDGE_CACHE_DIR":         "/cache",
		"DRAWBRIDGE_BACKUP_DIR":        "/backups",
		"DRAWBRIDGE_LISTEN":            ":9090",
		"DRAWBRIDGE_BASE_URL":          "https://drawbridge.example.com/",
		"DRAWBRIDGE_TRUSTED_PROXIES":   "192.0.2.1, 192.0.2.0/24",
		"DRAWBRIDGE_SECRET_KEY":        "key",
		"DRAWBRIDGE_METRICS_TOKEN":     "token",
		"DRAWBRIDGE_LOCAL_LOGIN":       "force",
		"DRAWBRIDGE_PRIMARY_ASN":       "64500",
		"DRAWBRIDGE_CHECK_INTERVAL":    "30s",
		"DRAWBRIDGE_TIMEOUT":           "3s",
		"DRAWBRIDGE_LEARNING_PERIOD":   "2h",
		"DRAWBRIDGE_RETENTION":         "48h",
		"DRAWBRIDGE_FAILOVER_AFTER":    "1m",
		"DRAWBRIDGE_FAILBACK_AFTER":    "10m",
		"DRAWBRIDGE_FAILBACK_MAX_HOLD": "1h",
		"DRAWBRIDGE_QUORUM":            "1",
		"DRAWBRIDGE_IP_PROVIDERS":      "https://a.example\nhttps://b.example",
		"LOG_LEVEL":                    "debug",
	} {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Detection: Detection{
			CheckInterval: 30 * time.Second, FailoverAfter: time.Minute, FailbackAfter: 10 * time.Minute,
			FailbackMaxHold: time.Hour, Quorum: 1, Timeout: 3 * time.Second, LearningPeriod: 2 * time.Hour,
			IPProviders: []string{"https://a.example", "https://b.example"}, Retention: 48 * time.Hour,
		},
		DBDir: "/db", CacheDir: "/cache", BackupDir: "/backups", Listen: ":9090", BaseURL: "https://drawbridge.example.com",
		Proxies: []string{"192.0.2.1", "192.0.2.0/24"}, SecretKey: "key", MetricsToken: "token",
		ForceLocal: true, PrimaryASN: 64500, LogLevel: slog.LevelDebug,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got  %+v\nwant %+v", c, want)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	for k, v := range map[string]string{
		"DRAWBRIDGE_CHECK_INTERVAL": "soon",
		"DRAWBRIDGE_QUORUM":         "two",
		"DRAWBRIDGE_LOCAL_LOGIN":    "always",
		"LOG_LEVEL":                 "loud",
	} {
		t.Setenv(k, v)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DRAWBRIDGE_CHECK_INTERVAL", "DRAWBRIDGE_QUORUM must be a number",
		"DRAWBRIDGE_LOCAL_LOGIN must be force", "LOG_LEVEL must be"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestValidate(t *testing.T) {
	d := DefaultDetection()
	d.CheckInterval = time.Second
	d.Quorum = 9
	d.FailbackAfter = 3 * time.Hour
	d.IPProviders = []string{"ftp://example.com"}
	err := d.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"check interval must be between", "quorum must be between 1 and 1",
		"cannot be shorter than the failback delay", `"ftp://example.com" must be an http(s) URL`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	d = DefaultDetection()
	d.IPProviders = nil
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "at least one IP provider") {
		t.Errorf("no providers: %v", err)
	}
}

func TestPendingCheck(t *testing.T) {
	d := Detection{CheckInterval: time.Minute}
	if d.PendingCheck() != 15*time.Second {
		t.Errorf("pending check for 1m: %s", d.PendingCheck())
	}
	d.CheckInterval = 5 * time.Second
	if d.PendingCheck() != 5*time.Second {
		t.Errorf("pending check for 5s: %s", d.PendingCheck())
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList(" a, b\r\n\nc ,, ")
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %q", got)
	}
	if SplitList("") != nil {
		t.Fatal("empty input should give nil")
	}
}
