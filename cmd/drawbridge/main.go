// SPDX-License-Identifier: AGPL-3.0-or-later

// Command drawbridge monitors the WAN connection and stops selected Docker
// containers while traffic runs over a backup ISP.
//
//	drawbridge                               run the monitor (default)
//	drawbridge status                        WAN state and the open run
//	drawbridge stacks                        stacks and their failover setting
//	drawbridge containers                    containers and their effective policy
//	drawbridge set-stack <project> on|off    disable a whole stack on failover
//	drawbridge set-container <key> <policy>  inherit, disable or keep
//	drawbridge isps                          ISP groups and their ASNs
//	drawbridge set-isp <id> <setting>        primary (pinned), backup, ignored, metered or unmetered
//	drawbridge set-data-limits <id> <outage-GB> <month-GB>  0 turns a limit off
//	drawbridge move-asn <asn> <id>|new       merge an ASN into a group, or split it out
//	drawbridge reset-password <username>     set a new password, read from stdin
//	drawbridge reset-2fa <username>          turn off two-factor sign-in
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo; TZ still works

	"github.com/zjcochran/wan-drawbridge/internal/agent"
	"github.com/zjcochran/wan-drawbridge/internal/agents"
	"github.com/zjcochran/wan-drawbridge/internal/asn"
	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/kube"
	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/notify"
	"github.com/zjcochran/wan-drawbridge/internal/proxmox"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/syslog"
	"github.com/zjcochran/wan-drawbridge/internal/web"
)

const usage = `usage:
  drawbridge                               run the monitor
  drawbridge agent                         run as an agent for another Docker host
  drawbridge status                        WAN state and the open run
  drawbridge stacks                        stacks and their failover setting
  drawbridge containers                    containers and their effective policy
  drawbridge set-stack <project> on|off    disable a whole stack on failover
  drawbridge set-container <key> <policy>  inherit, disable or keep
      (on another host: <host>:<project> or <host>:<key>)
  drawbridge isps                          ISP groups and their ASNs
  drawbridge set-isp <id> <setting>        primary (pinned), backup, ignored, metered or unmetered
  drawbridge set-data-limits <id> <outage-GB> <month-GB>
                                           an ISP's data limits; 0 turns one off
  drawbridge move-asn <asn> <id>|new       merge an ASN into a group, or split it out
  drawbridge reset-password <username>     set a new password, read from stdin
  drawbridge reset-2fa <username>          turn off two-factor sign-in
  drawbridge hosts                         hosts and their status
  drawbridge enroll-token <host>           a one-time token to enroll an agent (adds the host if new)
  drawbridge revoke-host <host>            refuse a host's agent from now on`

// Set at build time: -ldflags "-X main.version=v1.2.3 -X main.sourceURL=https://…".
// The AGPL asks that people using Drawbridge over a network can get its
// source; sourceURL is linked from every page. DRAWBRIDGE_SOURCE_URL overrides
// it, for a modified build hosted elsewhere.
var (
	version   = "dev"
	sourceURL = ""
)

// newResolver builds the ASN resolver; tests point it at a fake.
var newResolver = asn.NewResolver

func main() {
	os.Exit(cli(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// cli runs one invocation and returns the process exit code.
func cli(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "version":
			fmt.Fprintln(stdout, version)
			return 0
		case "healthcheck":
			if err := healthcheck(); err != nil {
				fmt.Fprintln(stderr, "unhealthy:", err)
				return 1
			}
			return 0
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) == 1 && args[0] == "agent" {
		if err := runAgent(ctx, stderr); err != nil {
			fmt.Fprintln(stderr, "drawbridge agent:", err)
			return 1
		}
		return 0
	}
	if err := run(ctx, args, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "drawbridge:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	var sl *syslog.Writer
	if len(args) == 0 && cfg.Syslog.Enabled() {
		// Only the server sends its log to syslog; commands log to the terminal.
		if sl, err = syslog.New(cfg.Syslog, log); err != nil {
			return err
		}
		sctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { sl.Run(sctx); close(done) }()
		// Stopped last, after the server, so its final lines are sent too.
		defer func() { cancel(); <-done }()
		log = slog.New(slog.NewMultiHandler(log.Handler(), sl.Handler(cfg.LogLevel)))
		log.Info("sending the log to syslog", "url", cfg.Syslog.URL)
	}

	for _, dir := range []string{cfg.DBDir, cfg.CacheDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DBDir, "drawbridge.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	dc, err := docker.New(env("DOCKER_HOST", "unix:///var/run/docker.sock"))
	if err != nil {
		return err
	}
	self, _ := os.Hostname() // Docker sets it to the container's short ID
	names, err := st.HostNames(ctx)
	if err != nil {
		return err
	}
	local := hosts.Host{ID: store.LocalHost, Name: names[store.LocalHost], Runtime: hosts.Local{Docker: dc, SelfID: self}}
	ex := executor.New(hosts.NewRegistry(local), st, log)

	if len(args) == 0 {
		return serve(ctx, cfg, st, dc, ex, local, sl, log)
	}
	box := func() (*secrets.Box, error) {
		return secrets.Load(cfg.SecretKey, filepath.Join(cfg.DBDir, "secret.key"))
	}
	return command(ctx, args, st, ex, box, cfg.BaseURL, stdin, stdout, stderr)
}

func serve(ctx context.Context, cfg config.Config, st *store.Store, dc *docker.Client, ex *executor.Executor, local hosts.Host,
	sl *syslog.Writer, log *slog.Logger) error {
	// The background goroutines are stopped and waited for before returning,
	// so none of them still uses the store when the caller closes it.
	var bg sync.WaitGroup
	defer bg.Wait()
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	if engine, err := dc.Ping(ctx); err != nil {
		log.Error("cannot reach Docker; is /var/run/docker.sock mounted? Continuing without container control", "err", err)
	} else {
		log.Info("connected to Docker", "version", engine)
	}

	proxies, err := web.ParseProxies(cfg.Proxies)
	if err != nil {
		return err
	}
	box, err := secrets.Load(cfg.SecretKey, filepath.Join(cfg.DBDir, "secret.key"))
	if err != nil {
		return err
	}
	if cfg.ForceLocal {
		log.Warn("DRAWBRIDGE_LOCAL_LOGIN=force: password sign-in is on even if single sign-on only is set")
	}
	hub := web.NewHub()
	ex.OnChange = func() { hub.Publish(web.TopicInventory) }
	m := monitor.New(cfg, st, newResolver(cfg.CacheDir, log), ex, log, func() { hub.Publish(web.TopicState) })

	notifier := &notify.Notifier{Store: st, Secrets: box, Log: log, Version: version}
	st.OnEvent(notifier.Event)
	bg.Go(func() { notifier.Run(ctx) })

	mgr := agents.New(st, box, ex.Hosts, local, log)
	mgr.Version = version
	resync := make(chan struct{}, 1)
	mgr.OnChange = func() {
		select {
		case resync <- struct{}{}:
		default:
		}
	}
	if err := mgr.Load(ctx); err != nil {
		return err
	}
	bg.Go(func() { mgr.Watch(ctx, 15*time.Second) })
	bg.Go(func() {
		// An agent's inventory or a host's status changed: sync, a burst at a time.
		for {
			select {
			case <-ctx.Done():
				return
			case <-resync:
				if err := ex.Sync(ctx); err != nil {
					log.Debug("inventory sync after an agent update", "err", err)
				}
				hub.Publish(web.TopicInventory)
			}
		}
	})

	ui, err := web.New(web.Server{
		Store: st, Mon: m, Docker: dc, Hosts: ex.Hosts, Agents: mgr, Sync: ex, Planner: ex, Hub: hub, Log: log,
		BaseURL: cfg.BaseURL, DBPath: filepath.Join(cfg.DBDir, "drawbridge.db"), TrustedProxies: proxies,
		Secrets: box, Notify: notifier, Syslog: sl, BackupDir: cfg.BackupDir, ForceLocal: cfg.ForceLocal, MetricsToken: cfg.MetricsToken, Version: version,
		SourceURL: env("DRAWBRIDGE_SOURCE_URL", sourceURL),
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 2)
	go func() {
		log.Info("web UI listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	bg.Go(func() { ex.WatchInventory(ctx, dc, 30*time.Second) })
	bg.Go(func() { ex.WatchUsage(ctx, time.Minute) })
	bg.Go(func() { ui.RunBackupSchedule(ctx) })
	bg.Go(func() { ui.RunTestSchedule(ctx) })
	bg.Go(func() { errc <- m.Run(ctx) })

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if serr := srv.Shutdown(shutdown); serr != nil {
		log.Warn("web server did not shut down cleanly", "err", serr)
	}
	return err
}

// runAgent runs agent mode: no database, web UI or monitor, and no port.
func runAgent(ctx context.Context, stderr io.Writer) error {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return errors.New("LOG_LEVEL must be debug, info, warn or error")
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	cfg, err := agent.ConfigFromEnv(os.Getenv, version)
	if err != nil {
		return err
	}
	self, _ := os.Hostname()
	switch cfg.Platform {
	case agent.PlatformProxmox:
		rt, err := proxmoxRuntime(ctx, self)
		if err != nil {
			return err
		}
		return agent.New(cfg, rt, nil, log).Run(ctx)
	case agent.PlatformKubernetes:
		c, err := kube.InCluster()
		if err != nil {
			return err
		}
		rt := hosts.Kubernetes{API: c, Namespaces: cfg.Allow}
		if _, err := rt.List(ctx); err != nil {
			return fmt.Errorf("cannot use the Kubernetes API (check the agent's RBAC): %w", err)
		}
		return agent.New(cfg, rt, nil, log).Run(ctx)
	}
	dc, err := docker.New(env("DOCKER_HOST", "unix:///var/run/docker.sock"))
	if err != nil {
		return err
	}
	if engine, err := dc.Ping(ctx); err != nil {
		log.Error("cannot reach Docker; is /var/run/docker.sock mounted?", "err", err)
	} else {
		log.Info("connected to Docker", "version", engine)
	}
	var rt hosts.Runtime = hosts.Local{Docker: dc, SelfID: self}
	if cfg.Platform == agent.PlatformSwarm {
		info, err := dc.Swarm(ctx)
		if err != nil {
			return err
		}
		if !info.Manager {
			return errors.New("DRAWBRIDGE_AGENT_PLATFORM=swarm needs a swarm manager node: place the agent on one (node.role == manager)")
		}
		rt = hosts.Swarm{Docker: dc, SelfID: self}
	}
	return agent.New(cfg, rt, dc, log).Run(ctx)
}

// proxmoxRuntime connects to the Proxmox API from PROXMOX_* variables, and
// checks the token and certificate work.
func proxmoxRuntime(ctx context.Context, self string) (hosts.Runtime, error) {
	secret := os.Getenv("PROXMOX_TOKEN_SECRET")
	if f := os.Getenv("PROXMOX_TOKEN_SECRET_FILE"); f != "" {
		// The path is the admin's own setting, like every other variable here.
		b, err := os.ReadFile(f) //nolint:gosec // nosemgrep // G304: the admin's own path
		if err != nil {
			return nil, fmt.Errorf("PROXMOX_TOKEN_SECRET_FILE: %w", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	c, err := proxmox.New(os.Getenv("PROXMOX_URL"), os.Getenv("PROXMOX_TOKEN_ID"), secret, os.Getenv("PROXMOX_FINGERPRINT"))
	if err != nil {
		return nil, err
	}
	if _, err := c.Guests(ctx); err != nil {
		return nil, fmt.Errorf("cannot use the Proxmox API: %w", err)
	}
	return hosts.Proxmox{API: c, Self: self}, nil
}

// healthcheck asks the running server for /healthz, for Docker's HEALTHCHECK.
// In agent mode (DRAWBRIDGE_MANAGER set) it checks the agent reached the
// manager within the last 90 seconds.
func healthcheck() error {
	if os.Getenv("DRAWBRIDGE_MANAGER") != "" {
		return agent.Healthy(env("DRAWBRIDGE_AGENT_DIR", "/data/agent"), time.Now())
	}
	host, port, err := net.SplitHostPort(env("DRAWBRIDGE_LISTEN", ":8080"))
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
