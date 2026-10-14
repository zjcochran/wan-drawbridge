// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCert makes a certificate for 127.0.0.1 and a pool that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// fakeSMTP is a minimal SMTP server: implicit TLS or STARTTLS, AUTH PLAIN,
// and a recipient that is refused.
type fakeSMTP struct {
	ln       net.Listener
	cert     tls.Certificate
	implicit bool // TLS from the start (smtps)
	noTLS    bool // does not offer STARTTLS
	mu       sync.Mutex
	auth     string // the decoded AUTH PLAIN
	rcpt     []string
	data     string
}

func newFakeSMTP(t *testing.T, cert tls.Certificate, implicit, noTLS bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	f := &fakeSMTP{ln: ln, cert: cert, implicit: implicit, noTLS: noTLS}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	say := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }
	say("220 fake ESMTP")
	secure := f.implicit
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO":
			if !secure && !f.noTLS {
				say("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 AUTH PLAIN")
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if tc.Handshake() != nil {
				return
			}
			conn, secure = tc, true
			r, w = bufio.NewReader(conn), bufio.NewWriter(conn)
		case "AUTH":
			b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
			f.mu.Lock()
			f.auth = string(b)
			f.mu.Unlock()
			say("235 ok")
		case "MAIL":
			say("250 ok")
		case "RCPT":
			if strings.Contains(line, "refused@") {
				say("550 no such user")
				continue
			}
			f.mu.Lock()
			f.rcpt = append(f.rcpt, line)
			f.mu.Unlock()
			say("250 ok")
		case "DATA":
			say("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) url(scheme string) string { return scheme + "://" + f.ln.Addr().String() }

func emailChannel(u string) Channel {
	return Channel{Name: "mail", Kind: KindEmail, URL: u, User: "alice", Token: "test-password-123",
		From: "Drawbridge <drawbridge@example.com>", To: "alice@example.com, bob@example.com"}
}

func TestEmailWithSTARTTLS(t *testing.T) {
	cert, pool := testCert(t)
	srv := newFakeSMTP(t, cert, false, false)
	n := newNotifier(t)
	n.TLS = &tls.Config{RootCAs: pool}
	if err := n.send(context.Background(), emailChannel(srv.url("smtp")), testMessage()); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.auth != "\x00alice\x00test-password-123" || len(srv.rcpt) != 2 {
		t.Errorf("auth %q, rcpt %v", srv.auth, srv.rcpt)
	}
	for _, want := range []string{"From: \"Drawbridge\" <drawbridge@example.com>", "To: <alice@example.com>, <bob@example.com>",
		"Subject: WAN Drawbridge: Failed over", "X-Priority: 2", "Traffic is on SKYLINK.", "Event: failover"} {
		if !strings.Contains(srv.data, want) {
			t.Errorf("message lacks %q:\n%s", want, srv.data)
		}
	}
}

func TestEmailWithImplicitTLS(t *testing.T) {
	cert, pool := testCert(t)
	srv := newFakeSMTP(t, cert, true, false)
	n := newNotifier(t)
	n.TLS = &tls.Config{RootCAs: pool}
	c := emailChannel(srv.url("smtps"))
	c.User = "" // no sign-in
	if err := n.send(context.Background(), c, testMessage()); err != nil {
		t.Fatal(err)
	}
	if srv.auth != "" {
		t.Errorf("signed in without a user: %q", srv.auth)
	}
}

func TestEmailRefusals(t *testing.T) {
	cert, pool := testCert(t)
	n := newNotifier(t)
	n.TLS = &tls.Config{RootCAs: pool}
	ctx := context.Background()

	// No STARTTLS: refused, not retried.
	plainSrv := newFakeSMTP(t, cert, false, true)
	if err := n.send(ctx, emailChannel(plainSrv.url("smtp")), testMessage()); !errors.Is(err, errPermanent) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("without STARTTLS: %v", err)
	}
	// A 550 for a recipient is permanent.
	srv := newFakeSMTP(t, cert, false, false)
	c := emailChannel(srv.url("smtp"))
	c.To = "refused@example.com"
	if err := n.send(ctx, c, testMessage()); !errors.Is(err, errPermanent) {
		t.Errorf("refused recipient: %v", err)
	}
	// A certificate the system does not trust.
	n.TLS = nil
	if err := n.send(ctx, emailChannel(srv.url("smtp")), testMessage()); err == nil {
		t.Error("an untrusted certificate was accepted")
	}
	// A broken channel never dials.
	if err := n.send(ctx, Channel{Kind: KindEmail, URL: "smtp://", From: "x"}, testMessage()); !errors.Is(err, errPermanent) {
		t.Errorf("invalid channel: %v", err)
	}
}

func TestSMTPAddr(t *testing.T) {
	for raw, want := range map[string]string{
		"smtp://mail.example.com": "mail.example.com:587", "smtps://mail.example.com": "mail.example.com:465",
		"smtp://mail.example.com:25": "mail.example.com:25",
	} {
		u, _ := url.Parse(raw)
		if got := smtpAddr(u); got != want {
			t.Errorf("smtpAddr(%s) = %s, want %s", raw, got, want)
		}
	}
}
