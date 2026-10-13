// SPDX-License-Identifier: AGPL-3.0-or-later

// Package syslog sends log records to a syslog server over UDP, TCP or TLS,
// as RFC 5424 or RFC 3164 messages. Sending never blocks the caller: records
// wait in a queue, and are dropped and counted when it is full.
package syslog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Config is the syslog destination. An empty URL turns syslog off.
type Config struct {
	URL      string // udp://host[:514], tcp://host[:514] or tls://host[:6514]
	Format   string // rfc5424 (default) or rfc3164
	Facility string // default daemon
	CAFile   string // tls: a PEM file of extra CAs to trust
}

// Enabled reports whether a destination is set.
func (c Config) Enabled() bool { return c.URL != "" }

var facilities = map[string]int{
	"kern": 0, "user": 1, "mail": 2, "daemon": 3, "auth": 4, "syslog": 5, "lpr": 6, "news": 7,
	"uucp": 8, "cron": 9, "authpriv": 10, "ftp": 11,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

var defaultPorts = map[string]string{"udp": "514", "tcp": "514", "tls": "6514"}

// Check returns every problem with the configuration, for the start-up error.
func (c Config) Check() error {
	if !c.Enabled() {
		return nil
	}
	var errs []error
	if _, _, err := c.endpoint(); err != nil {
		errs = append(errs, err)
	}
	if f := c.Format; f != "" && f != "rfc5424" && f != "rfc3164" {
		errs = append(errs, fmt.Errorf("DRAWBRIDGE_SYSLOG_FORMAT must be rfc5424 or rfc3164, not %q", f))
	}
	if _, ok := facilities[c.facility()]; !ok {
		errs = append(errs, fmt.Errorf("DRAWBRIDGE_SYSLOG_FACILITY %q is not a syslog facility (daemon, user, local0 to local7, …)", c.Facility))
	}
	if c.CAFile != "" {
		if scheme, _, _ := c.endpoint(); scheme != "tls" {
			errs = append(errs, errors.New("DRAWBRIDGE_SYSLOG_CA_FILE only applies to a tls:// syslog URL"))
		} else if _, err := c.roots(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c Config) facility() string {
	if c.Facility == "" {
		return "daemon"
	}
	return strings.ToLower(c.Facility)
}

// endpoint returns the scheme and host:port of the URL.
func (c Config) endpoint() (string, string, error) {
	u, err := url.Parse(c.URL)
	bad := fmt.Errorf("DRAWBRIDGE_SYSLOG must look like udp://host:514, tcp://host:514 or tls://host:6514, not %q", c.URL)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" {
		return "", "", bad
	}
	port, ok := defaultPorts[u.Scheme]
	if !ok {
		return "", "", bad
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", "", bad
		}
		port = p
	}
	return u.Scheme, net.JoinHostPort(u.Hostname(), port), nil
}

// roots is the system CA pool plus CAFile.
func (c Config) roots() (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if c.CAFile == "" {
		return pool, nil
	}
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("DRAWBRIDGE_SYSLOG_CA_FILE: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("DRAWBRIDGE_SYSLOG_CA_FILE: no PEM certificates in %s", c.CAFile)
	}
	return pool, nil
}

// Sizes and timings.
const (
	QueueSize    = 1024             // records waiting to be sent
	DialTimeout  = 10 * time.Second // connecting, including the TLS handshake
	WriteTimeout = 10 * time.Second // one record on TCP or TLS
	MaxBackoff   = 30 * time.Second // the wait between attempts doubles from 1 s to this
	DrainTimeout = 2 * time.Second  // at shutdown, for records still queued
)

// Writer sends records to one syslog server. Make it with New, start Run,
// and log through Handler.
type Writer struct {
	Log *slog.Logger // its own problems; must not log through Handler

	network, addr, host string
	tls                 *tls.Config
	format              string
	facility            int
	hostname            string

	dial          func(ctx context.Context, network, addr string) (net.Conn, error)
	now           func() time.Time
	retry         time.Duration // the first wait before reconnecting
	queue         chan []byte
	sent, dropped atomic.Uint64
}

// New checks cfg and makes a Writer. It does not connect yet.
func New(cfg Config, log *slog.Logger) (*Writer, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	scheme, addr, _ := cfg.endpoint()
	w := &Writer{
		Log: log, addr: addr, network: scheme,
		format: cfg.Format, facility: facilities[cfg.facility()],
		now: time.Now, retry: time.Second, queue: make(chan []byte, QueueSize),
	}
	if w.format == "" {
		w.format = "rfc5424"
	}
	w.host, _, _ = net.SplitHostPort(addr)
	if scheme == "tls" {
		roots, err := cfg.roots()
		if err != nil {
			return nil, err
		}
		w.network = "tcp"
		w.tls = &tls.Config{ServerName: w.host, RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	w.hostname, _ = os.Hostname()
	if w.hostname == "" {
		w.hostname = "-"
	}
	d := &net.Dialer{Timeout: DialTimeout}
	w.dial = d.DialContext
	return w, nil
}

// Counts returns records sent and records dropped because the queue was full.
func (w *Writer) Counts() (sent, dropped uint64) { return w.sent.Load(), w.dropped.Load() }

// Handler returns a slog handler that formats records like slog's text
// handler (without the time, which the syslog header carries) and queues
// them for the server.
func (w *Writer) Handler(level slog.Leveler) slog.Handler {
	return slog.NewTextHandler(sink{w}, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
}

// sink receives one formatted record per Write: "level=INFO msg=… k=v\n".
type sink struct{ w *Writer }

func (s sink) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	sev := 6 // info
	if rest, ok := strings.CutPrefix(line, "level="); ok {
		lvl, msg, _ := strings.Cut(rest, " ")
		sev = severity(lvl)
		line = msg
	}
	s.w.enqueue(s.w.frame(sev, line))
	return len(p), nil
}

// severity maps slog's level names (with offsets like WARN+2) to syslog's.
func severity(level string) int {
	switch {
	case strings.HasPrefix(level, "ERROR"):
		return 3
	case strings.HasPrefix(level, "WARN"):
		return 4
	case strings.HasPrefix(level, "DEBUG"):
		return 7
	}
	return 6
}

func (w *Writer) enqueue(b []byte) {
	select {
	case w.queue <- b:
	default:
		w.dropped.Add(1)
	}
}

// frame builds the message as it goes on the wire.
func (w *Writer) frame(sev int, msg string) []byte {
	pri := w.facility*8 + sev
	var m string
	if w.format == "rfc3164" {
		m = fmt.Sprintf("<%d>%s %s drawbridge: %s", pri, w.now().Local().Format(time.Stamp), w.hostname, msg)
	} else {
		ts := w.now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
		m = fmt.Sprintf("<%d>1 %s %s drawbridge - - - %s", pri, ts, w.hostname, msg)
	}
	switch {
	case w.network == "udp":
		return []byte(m)
	case w.format == "rfc3164":
		return []byte(m + "\n") // newline framing (RFC 6587 non-transparent)
	default:
		return []byte(strconv.Itoa(len(m)) + " " + m) // octet counting (RFC 6587, RFC 5425)
	}
}

// Run sends queued records until ctx is cancelled, then for up to
// DrainTimeout more. It reconnects after errors, waiting 1 s, then twice as
// long each time up to MaxBackoff; records queue meanwhile.
func (w *Writer) Run(ctx context.Context) {
	var conn net.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	backoff, down := w.retry, false
	for {
		var b []byte
		select {
		case b = <-w.queue:
		case <-ctx.Done():
			w.drain(conn)
			return
		}
		for {
			if conn == nil {
				c, err := w.connect(ctx)
				if err != nil {
					if !down {
						w.Log.Warn("syslog: cannot connect; retrying", "addr", w.addr, "err", err)
						down = true
					}
					select {
					case <-time.After(backoff):
					case <-ctx.Done():
						return
					}
					backoff = min(backoff*2, MaxBackoff)
					continue
				}
				conn = c
				if down {
					w.Log.Info("syslog: connected again", "addr", w.addr)
					down = false
				}
				backoff = w.retry
			}
			if err := w.write(conn, b); err != nil {
				conn.Close()
				conn = nil
				if ctx.Err() != nil {
					return
				}
				continue // reconnect and send this record again
			}
			w.sent.Add(1)
			break
		}
	}
}

func (w *Writer) connect(ctx context.Context) (net.Conn, error) {
	c, err := w.dial(ctx, w.network, w.addr)
	if err != nil || w.tls == nil {
		return c, err
	}
	tc := tls.Client(c, w.tls)
	hctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		c.Close()
		return nil, err
	}
	return tc, nil
}

func (w *Writer) write(conn net.Conn, b []byte) error {
	if err := conn.SetWriteDeadline(w.now().Add(WriteTimeout)); err != nil {
		return err
	}
	_, err := conn.Write(b)
	return err
}

// drain sends what is still queued at shutdown, on the open connection only.
func (w *Writer) drain(conn net.Conn) {
	if conn == nil {
		return
	}
	deadline := time.Now().Add(DrainTimeout)
	for {
		select {
		case b := <-w.queue:
			if time.Now().After(deadline) || w.write(conn, b) != nil {
				return
			}
			w.sent.Add(1)
		default:
			return
		}
	}
}
