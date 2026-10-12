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
//	drawbridge set-isp <id> <role>           primary (pinned), backup or ignored
//	drawbridge move-asn <asn> <id>|new       merge an ASN into a group, or split it out
//	drawbridge reset-password <username>     set a new password, read from stdin
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
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo; TZ still works

	"github.com/zjcochran/wan-drawbridge/internal/asn"
	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/executor"
	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/web"
)

const usage = `usage:
  drawbridge                               run the monitor
  drawbridge status                        WAN state and the open run
  drawbridge stacks                        stacks and their failover setting
  drawbridge containers                    containers and their effective policy
  drawbridge set-stack <project> on|off    disable a whole stack on failover
  drawbridge set-container <key> <policy>  inherit, disable or keep
  drawbridge isps                          ISP groups and their ASNs
  drawbridge set-isp <id> <role>           primary (pinned), backup or ignored
  drawbridge move-asn <asn> <id>|new       merge an ASN into a group, or split it out
  drawbridge reset-password <username>     set a new password, read from stdin`

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
	ex := executor.New(dc, st, log, self)

	if len(args) == 0 {
		return serve(ctx, cfg, st, dc, ex, log)
	}
	return command(ctx, args, st, ex, stdin, stdout, stderr)
}

func serve(ctx context.Context, cfg config.Config, st *store.Store, dc *docker.Client, ex *executor.Executor, log *slog.Logger) error {
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

	ui, err := web.New(web.Server{
		Store: st, Mon: m, Docker: dc, Sync: ex, Hub: hub, Log: log,
		BaseURL: cfg.BaseURL, DBPath: filepath.Join(cfg.DBDir, "drawbridge.db"), TrustedProxies: proxies,
		Secrets: box, ForceLocal: cfg.ForceLocal, MetricsToken: cfg.MetricsToken, Version: version,
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
	go ex.WatchInventory(ctx, dc, 30*time.Second)
	go func() { errc <- m.Run(ctx) }()

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

// healthcheck asks the running server for /healthz, for Docker's HEALTHCHECK.
func healthcheck() error {
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
