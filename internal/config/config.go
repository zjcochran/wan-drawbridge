// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config holds Drawbridge's settings. Paths and the listen address
// come from the environment only; detection settings start from the
// environment and can then be changed in the web UI, which stores them.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/probe"
	"github.com/zjcochran/wan-drawbridge/internal/syslog"
)

// Detection is the part of the configuration that can be edited in the UI.
type Detection struct {
	CheckInterval   time.Duration `json:"check_interval"`
	FailoverAfter   time.Duration `json:"failover_after"`    // backup seen this long before failover
	FailbackAfter   time.Duration `json:"failback_after"`    // primary stable this long before failback
	FailbackMaxHold time.Duration `json:"failback_max_hold"` // longest failback wait while the primary flaps
	Quorum          int           `json:"quorum"`
	Timeout         time.Duration `json:"timeout"`
	LearningPeriod  time.Duration `json:"learning_period"`
	IPProviders     []string      `json:"ip_providers"`
	Retention       time.Duration `json:"retention"`
}

// PendingCheck is the faster interval used while confirming a change.
func (d Detection) PendingCheck() time.Duration {
	return min(15*time.Second, d.CheckInterval)
}

// Limits are the allowed ranges, shown in the UI next to each field.
var Limits = struct {
	CheckInterval, Timeout, LearningPeriod, Retention [2]time.Duration
	FailoverAfter, FailbackAfter, FailbackMaxHold     [2]time.Duration
}{
	CheckInterval:   [2]time.Duration{15 * time.Second, time.Hour},
	Timeout:         [2]time.Duration{time.Second, 30 * time.Second},
	LearningPeriod:  [2]time.Duration{time.Hour, 14 * 24 * time.Hour},
	Retention:       [2]time.Duration{24 * time.Hour, 365 * 24 * time.Hour},
	FailoverAfter:   [2]time.Duration{0, 30 * time.Minute},
	FailbackAfter:   [2]time.Duration{0, 6 * time.Hour},
	FailbackMaxHold: [2]time.Duration{0, 24 * time.Hour},
}

// Validate checks every field against Limits and returns all problems at once.
func (d Detection) Validate() error {
	var errs []error
	dur := func(name string, v time.Duration, lim [2]time.Duration) {
		if v < lim[0] || v > lim[1] {
			errs = append(errs, fmt.Errorf("%s must be between %s and %s", name, lim[0], lim[1]))
		}
	}
	num := func(name string, v int, lo, hi int) {
		if v < lo || v > hi {
			errs = append(errs, fmt.Errorf("%s must be between %d and %d", name, lo, hi))
		}
	}
	dur("check interval", d.CheckInterval, Limits.CheckInterval)
	dur("timeout", d.Timeout, Limits.Timeout)
	dur("learning period", d.LearningPeriod, Limits.LearningPeriod)
	dur("retention", d.Retention, Limits.Retention)
	dur("failover delay", d.FailoverAfter, Limits.FailoverAfter)
	dur("failback delay", d.FailbackAfter, Limits.FailbackAfter)
	dur("longest failback wait", d.FailbackMaxHold, Limits.FailbackMaxHold)
	if d.FailbackMaxHold < d.FailbackAfter {
		errs = append(errs, errors.New("the longest failback wait cannot be shorter than the failback delay"))
	}
	if len(d.IPProviders) == 0 {
		errs = append(errs, errors.New("at least one IP provider is required"))
	}
	for _, p := range d.IPProviders {
		if !strings.HasPrefix(p, "https://") && !strings.HasPrefix(p, "http://") {
			errs = append(errs, fmt.Errorf("IP provider %q must be an http(s) URL", p))
		}
	}
	num("quorum", d.Quorum, 1, max(1, len(d.IPProviders)))
	return errors.Join(errs...)
}

// DefaultDetection is used when neither the environment nor the UI set a value.
func DefaultDetection() Detection {
	return Detection{
		CheckInterval:   60 * time.Second,
		FailoverAfter:   45 * time.Second,
		FailbackAfter:   5 * time.Minute,
		FailbackMaxHold: 2 * time.Hour,
		Quorum:          2,
		Timeout:         5 * time.Second,
		LearningPeriod:  24 * time.Hour,
		IPProviders:     probe.DefaultProviders,
		Retention:       30 * 24 * time.Hour,
	}
}

type Config struct {
	Detection
	DBDir        string   // holds only the SQLite files: mount it to back them up
	CacheDir     string   // the downloaded ASN database; safe to lose
	BackupDir    string   // scheduled backup files
	Listen       string   // web UI address
	BaseURL      string   // external URL; https here marks cookies Secure
	Proxies      []string // trusted reverse proxies: IPs or CIDR ranges
	SecretKey    string   // seals stored secrets; generated into DBDir when empty
	MetricsToken string   // enables /metrics behind this bearer token
	ForceLocal   bool     // DRAWBRIDGE_LOCAL_LOGIN=force: passwords work even in SSO-only mode
	PrimaryASN   int      // pinned primary; 0 = learn it
	LogLevel     slog.Level
	Syslog       syslog.Config // also send the log here; off when the URL is empty
}

// Load reads DRAWBRIDGE_* variables.
func Load() (Config, error) {
	c := Config{
		Detection:    DefaultDetection(),
		DBDir:        env("DRAWBRIDGE_DB_DIR", "/data/db"),
		CacheDir:     env("DRAWBRIDGE_CACHE_DIR", "/data/cache"),
		BackupDir:    env("DRAWBRIDGE_BACKUP_DIR", "/data/backups"),
		Listen:       env("DRAWBRIDGE_LISTEN", ":8080"),
		BaseURL:      strings.TrimRight(os.Getenv("DRAWBRIDGE_BASE_URL"), "/"),
		Proxies:      SplitList(os.Getenv("DRAWBRIDGE_TRUSTED_PROXIES")),
		SecretKey:    os.Getenv("DRAWBRIDGE_SECRET_KEY"),
		MetricsToken: os.Getenv("DRAWBRIDGE_METRICS_TOKEN"),
		Syslog: syslog.Config{
			URL:      os.Getenv("DRAWBRIDGE_SYSLOG"),
			Format:   os.Getenv("DRAWBRIDGE_SYSLOG_FORMAT"),
			Facility: os.Getenv("DRAWBRIDGE_SYSLOG_FACILITY"),
			CAFile:   os.Getenv("DRAWBRIDGE_SYSLOG_CA_FILE"),
		},
	}
	var errs []error
	dur := func(key string, dst *time.Duration) {
		if v := os.Getenv(key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
			}
			*dst = d
		}
	}
	num := func(key string, dst *int) {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s must be a number", key))
			}
			*dst = n
		}
	}
	dur("DRAWBRIDGE_CHECK_INTERVAL", &c.CheckInterval)
	dur("DRAWBRIDGE_TIMEOUT", &c.Timeout)
	dur("DRAWBRIDGE_LEARNING_PERIOD", &c.LearningPeriod)
	dur("DRAWBRIDGE_RETENTION", &c.Retention)
	dur("DRAWBRIDGE_FAILOVER_AFTER", &c.FailoverAfter)
	dur("DRAWBRIDGE_FAILBACK_AFTER", &c.FailbackAfter)
	dur("DRAWBRIDGE_FAILBACK_MAX_HOLD", &c.FailbackMaxHold)
	num("DRAWBRIDGE_QUORUM", &c.Quorum)
	num("DRAWBRIDGE_PRIMARY_ASN", &c.PrimaryASN)
	if v := os.Getenv("DRAWBRIDGE_IP_PROVIDERS"); v != "" {
		c.IPProviders = SplitList(v)
	}
	switch v := os.Getenv("DRAWBRIDGE_LOCAL_LOGIN"); v {
	case "", "auto":
	case "force":
		c.ForceLocal = true
	default:
		errs = append(errs, fmt.Errorf("DRAWBRIDGE_LOCAL_LOGIN must be force or unset, not %q", v))
	}
	if err := c.LogLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, errors.New("LOG_LEVEL must be debug, info, warn or error"))
	}
	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := c.Syslog.Check(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return c, nil
}

// SplitList splits a comma- or newline-separated list, dropping blanks.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
