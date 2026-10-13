// SPDX-License-Identifier: AGPL-3.0-or-later

package syslog

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "ca.txt")
	os.WriteFile(notPEM, []byte("not a certificate"), 0o600)
	cases := []struct {
		cfg  Config
		want string // "" = valid
	}{
		{Config{}, ""},
		{Config{URL: "udp://192.0.2.10"}, ""},
		{Config{URL: "tcp://syslog.example.com:1514", Format: "rfc3164", Facility: "LOCAL3"}, ""},
		{Config{URL: "tls://syslog.example.com", Facility: "local7"}, ""},
		{Config{URL: "[2001:db8::1]:514"}, "must look like"},
		{Config{URL: "udp://[2001:db8::1]:514"}, ""},
		{Config{URL: "http://syslog.example.com"}, "must look like"},
		{Config{URL: "udp://"}, "must look like"},
		{Config{URL: "udp://host:0"}, "must look like"},
		{Config{URL: "udp://host:99999"}, "must look like"},
		{Config{URL: "udp://host/path"}, "must look like"},
		{Config{URL: "udp://user@host"}, "must look like"},
		{Config{URL: "udp://host?x=1"}, "must look like"},
		{Config{URL: "udp://host", Format: "json"}, "rfc5424 or rfc3164"},
		{Config{URL: "udp://host", Facility: "local9"}, "not a syslog facility"},
		{Config{URL: "udp://host", CAFile: notPEM}, "only applies to a tls://"},
		{Config{URL: "tls://host", CAFile: filepath.Join(dir, "missing.pem")}, "DRAWBRIDGE_SYSLOG_CA_FILE"},
		{Config{URL: "tls://host", CAFile: notPEM}, "no PEM certificates"},
	}
	for _, c := range cases {
		err := c.cfg.Check()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%+v: %v", c.cfg, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%+v: got %v, want %q", c.cfg, err, c.want)
		}
	}
	if _, err := New(Config{URL: "ftp://host"}, quiet); err == nil {
		t.Error("New accepted a bad URL")
	}
}

func fixed(w *Writer) {
	w.now = func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 890000000, time.UTC) }
	w.hostname = "drawbridge-host"
}

func TestFrame(t *testing.T) {
	cases := []struct {
		cfg  Config
		sev  int
		want string
	}{
		{Config{URL: "udp://h"}, 6, `<30>1 2026-03-04T05:06:07.890000Z drawbridge-host drawbridge - - - msg="state changed"`},
		{Config{URL: "udp://h", Facility: "local0"}, 3, `<131>1 2026-03-04T05:06:07.890000Z drawbridge-host drawbridge - - - msg="state changed"`},
		{Config{URL: "udp://h", Format: "rfc3164"}, 4, `<28>Mar  4 05:06:07 drawbridge-host drawbridge: msg="state changed"`},
		{Config{URL: "tcp://h", Format: "rfc3164"}, 7, "<31>Mar  4 05:06:07 drawbridge-host drawbridge: msg=\"state changed\"\n"},
	}
	for _, c := range cases {
		w, err := New(c.cfg, quiet)
		if err != nil {
			t.Fatal(err)
		}
		fixed(w)
		got := string(w.frame(c.sev, `msg="state changed"`))
		if c.cfg.Format == "rfc3164" {
			// RFC 3164 has no time zone: local time.
			want := strings.Replace(c.want, "Mar  4 05:06:07", w.now().Local().Format(time.Stamp), 1)
			if got != want {
				t.Errorf("%+v:\n got %q\nwant %q", c.cfg, got, want)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.cfg, got, c.want)
		}
	}
	// TCP with RFC 5424: octet counting.
	w, _ := New(Config{URL: "tcp://h"}, quiet)
	fixed(w)
	got := string(w.frame(6, "msg=x"))
	n, msg, _ := strings.Cut(got, " ")
	if strconv.Itoa(len(msg)) != n || !strings.HasPrefix(msg, "<30>1 ") {
		t.Errorf("octet counting: %q", got)
	}
}

func TestHandler(t *testing.T) {
	w, _ := New(Config{URL: "udp://h"}, quiet)
	fixed(w)
	log := slog.New(w.Handler(slog.LevelInfo)).With("component", "monitor")
	log.Debug("not sent")
	log.Info("state changed", "isp", "EXAMPLENET")
	log.Warn("flapping", "hold", "10m0s")
	log.Error("container action failed", "err", "boom")
	log.Log(context.Background(), slog.LevelWarn+2, "warn plus two")
	want := []string{
		`<30>1 2026-03-04T05:06:07.890000Z drawbridge-host drawbridge - - - msg="state changed" component=monitor isp=EXAMPLENET`,
		`<28>1 2026-03-04T05:06:07.890000Z drawbridge-host drawbridge - - - msg=flapping component=monitor hold=10m0s`,
		`<27>1 2026-03-04T05:06:07.890000Z drawbridge-host drawbridge - - - msg="container action failed" component=monitor err=boom`,
		`<28>1 2026-03-04T05:06:07.890000Z drawbridge-host drawbridge - - - msg="warn plus two" component=monitor`,
	}
	if len(w.queue) != len(want) {
		t.Fatalf("queued %d, want %d", len(w.queue), len(want))
	}
	for _, s := range want {
		if got := string(<-w.queue); got != s {
			t.Errorf("\n got %q\nwant %q", got, s)
		}
	}
	// A line without a level prefix still goes out, as info.
	sink{w}.Write([]byte("plain\n"))
	if got := string(<-w.queue); !strings.HasPrefix(got, "<30>1 ") || !strings.HasSuffix(got, " plain") {
		t.Errorf("plain line: %q", got)
	}
	if severity("DEBUG-4") != 7 || severity("INFO+1") != 6 {
		t.Error("severity offsets")
	}
}

func TestQueueFull(t *testing.T) {
	w, _ := New(Config{URL: "udp://h"}, quiet)
	for range QueueSize + 3 {
		w.enqueue([]byte("x"))
	}
	if sent, dropped := w.Counts(); sent != 0 || dropped != 3 {
		t.Errorf("sent %d dropped %d", sent, dropped)
	}
}

// run starts w and returns a function that stops it and waits.
func run(t *testing.T, w *Writer) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
	t.Cleanup(func() { cancel(); <-done })
	return stop
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out waiting for", what)
}

func TestUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	w, err := New(Config{URL: "udp://" + pc.LocalAddr().String()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	run(t, w)
	slog.New(w.Handler(slog.LevelInfo)).Info("hello", "n", 1)
	buf := make([]byte, 2048)
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); !strings.HasPrefix(got, "<30>1 ") || !strings.HasSuffix(got, " drawbridge - - - msg=hello n=1") {
		t.Errorf("got %q", got)
	}
	eventually(t, "sent count", func() bool { s, _ := w.Counts(); return s == 1 })
}

// lineServer accepts connections and collects the octet-counted messages.
type lineServer struct {
	ln    net.Listener
	mu    sync.Mutex
	got   []string
	conns []net.Conn
}

func (s *lineServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		go func() {
			r := bufio.NewReader(c)
			for {
				n, err := r.ReadString(' ')
				if err != nil {
					return
				}
				size, _ := strconv.Atoi(strings.TrimSpace(n))
				msg := make([]byte, size)
				if _, err := io.ReadFull(r, msg); err != nil {
					return
				}
				s.mu.Lock()
				s.got = append(s.got, string(msg))
				s.mu.Unlock()
			}
		}()
	}
}

func (s *lineServer) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// dropConns closes the server side of every connection so far.
func (s *lineServer) dropConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func newLineServer(t *testing.T, ln net.Listener) *lineServer {
	s := &lineServer{ln: ln}
	go s.serve()
	t.Cleanup(func() { ln.Close(); s.dropConns() })
	return s
}

func TestTCPReconnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newLineServer(t, ln)
	w, err := New(Config{URL: "tcp://" + ln.Addr().String()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	stop := run(t, w)
	log := slog.New(w.Handler(slog.LevelInfo))
	log.Info("first")
	eventually(t, "first", func() bool { return len(srv.messages()) == 1 })

	// The server drops the connection: the writer notices on a later write
	// and reconnects, so messages keep arriving.
	srv.dropConns()
	for i := 0; len(srv.messages()) < 3 && i < 200; i++ {
		log.Info("again", "i", i)
		time.Sleep(10 * time.Millisecond)
	}
	if len(srv.messages()) < 3 {
		t.Fatalf("no messages after the connection was dropped: %q", srv.messages())
	}

	// At shutdown, what is still queued is sent.
	before := len(srv.messages())
	log.Info("last words")
	stop()
	eventually(t, "drained", func() bool {
		m := srv.messages()
		return len(m) > before && strings.HasSuffix(m[len(m)-1], "msg=\"last words\"")
	})
}

func TestTLS(t *testing.T) {
	certPEM, cert := selfSigned(t)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, certPEM, 0o600)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	srv := newLineServer(t, ln)

	// Without the CA the handshake fails, and the writer keeps retrying.
	var logged strings.Builder
	var mu sync.Mutex
	untrusted, _ := New(Config{URL: "tls://" + ln.Addr().String()}, slog.New(slog.NewTextHandler(writerFunc(func(p []byte) {
		mu.Lock()
		logged.Write(p)
		mu.Unlock()
	}), nil)))
	untrusted.retry = 10 * time.Millisecond
	stopUntrusted := run(t, untrusted)
	slog.New(untrusted.Handler(slog.LevelInfo)).Info("not trusted")
	eventually(t, "connect warning", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logged.String(), "syslog: cannot connect")
	})
	stopUntrusted()
	if s, _ := untrusted.Counts(); s != 0 {
		t.Errorf("sent %d over an untrusted connection", s)
	}

	w, err := New(Config{URL: "tls://" + ln.Addr().String(), CAFile: ca}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	run(t, w)
	slog.New(w.Handler(slog.LevelInfo)).Warn("over tls")
	eventually(t, "tls message", func() bool {
		for _, m := range srv.messages() {
			if strings.HasPrefix(m, "<28>1 ") && strings.HasSuffix(m, `msg="over tls"`) {
				return true
			}
		}
		return false
	})
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func TestBackoffAndRecovery(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newLineServer(t, ln)
	var logged strings.Builder
	var mu sync.Mutex
	w, _ := New(Config{URL: "tcp://" + ln.Addr().String()}, slog.New(slog.NewTextHandler(writerFunc(func(p []byte) {
		mu.Lock()
		logged.Write(p)
		mu.Unlock()
	}), nil)))
	w.retry = 10 * time.Millisecond
	// The first two connection attempts fail, as if the server were down.
	real := w.dial
	var attempts int
	var amu sync.Mutex
	w.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		amu.Lock()
		attempts++
		n := attempts
		amu.Unlock()
		if n <= 2 {
			return nil, errors.New("connection refused")
		}
		return real(ctx, network, addr)
	}
	run(t, w)
	slog.New(w.Handler(slog.LevelInfo)).Info("after outage")
	eventually(t, "delivery after retries", func() bool { return len(srv.messages()) == 1 })
	mu.Lock()
	out := logged.String()
	mu.Unlock()
	if strings.Count(out, "syslog: cannot connect") != 1 || !strings.Contains(out, "syslog: connected again") {
		t.Errorf("want one warning and one recovery line:\n%s", out)
	}
}

func TestStopWhileDown(t *testing.T) {
	w, _ := New(Config{URL: "tcp://192.0.2.1"}, quiet)
	w.retry = time.Hour // stopping must not wait for it
	w.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unreachable") }
	stop := run(t, w)
	w.enqueue([]byte("x"))
	time.Sleep(20 * time.Millisecond) // into the backoff wait
	stop()
	// Stopping with nothing connected and nothing queued returns at once.
	w2, _ := New(Config{URL: "tcp://192.0.2.1"}, quiet)
	run(t, w2)()
}

func selfSigned(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "syslog test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, cert
}
