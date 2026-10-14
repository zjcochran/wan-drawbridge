// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// validateEmail checks an email channel: an smtp:// or smtps:// URL, a
// sender and at least one recipient.
func (c Channel) validateEmail() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" {
		return errors.New("the URL must be smtp://host:port (STARTTLS) or smtps://host:port (TLS)")
	}
	if _, err := mail.ParseAddress(c.From); err != nil {
		return fmt.Errorf("the sender %q is not an email address", c.From)
	}
	if _, err := mail.ParseAddressList(c.To); err != nil {
		return fmt.Errorf("the recipients %q are not email addresses, separated by commas", c.To)
	}
	return nil
}

// smtpAddr is host:port, with the port 587 for smtp:// and 465 for smtps://
// when the URL has none.
func smtpAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"smtp": "587", "smtps": "465"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// sendMail sends m over SMTP, always encrypted: smtps:// starts with TLS,
// smtp:// requires STARTTLS. It signs in when the channel has a user.
func (n *Notifier) sendMail(ctx context.Context, c Channel, m Message) error {
	if err := c.validateEmail(); err != nil {
		return fmt.Errorf("%w: %w", errPermanent, err)
	}
	u, _ := url.Parse(c.URL)
	from, _ := mail.ParseAddress(c.From)
	to, _ := mail.ParseAddressList(c.To)
	tlsConf := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if n.TLS != nil {
		tlsConf.RootCAs = n.TLS.RootCAs
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if u.Scheme == "smtps" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConf}).DialContext(ctx, "tcp", smtpAddr(u))
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", smtpAddr(u))
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	cl, err := smtp.NewClient(conn, u.Hostname())
	if err != nil {
		return smtpErr(err)
	}
	defer cl.Close()
	if u.Scheme == "smtp" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%w: the server does not offer STARTTLS; Drawbridge does not send mail unencrypted", errPermanent)
		}
		if err := cl.StartTLS(tlsConf); err != nil {
			return err
		}
	}
	if c.User != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Token, u.Hostname())); err != nil {
			return smtpErr(err)
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return smtpErr(err)
	}
	for _, a := range to {
		if err := cl.Rcpt(a.Address); err != nil {
			return smtpErr(err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return smtpErr(err)
	}
	if _, err := w.Write(mailMessage(from, to, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return smtpErr(err)
	}
	return smtpErr(cl.Quit())
}

// smtpErr marks permanent SMTP replies (5xx) so they are not retried.
func smtpErr(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return fmt.Errorf("%w: %w", errPermanent, err)
	}
	return err
}

// mailMessage is the message as sent: plain text, quoted-printable.
func mailMessage(from *mail.Address, to []*mail.Address, m Message) []byte {
	var b bytes.Buffer
	rcpt := make([]string, len(to))
	for i, a := range to {
		rcpt[i] = a.String()
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	host := "drawbridge.invalid"
	if _, h, ok := strings.Cut(from.Address, "@"); ok {
		host = h
	}
	header := [][2]string{
		{"From", from.String()},
		{"To", strings.Join(rcpt, ", ")},
		{"Subject", mime.QEncoding.Encode("utf-8", "WAN Drawbridge: "+m.Title)},
		{"Date", m.Time.Format(time.RFC1123Z)},
		{"Message-ID", "<" + hex.EncodeToString(id) + "@" + host + ">"},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "quoted-printable"},
	}
	if m.Priority >= 4 {
		header = append(header, [2]string{"X-Priority", map[int]string{4: "2", 5: "1"}[m.Priority]})
	}
	for _, h := range header {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	fmt.Fprintf(qp, "%s\r\n\r\nEvent: %s\r\nTime: %s\r\n", m.Text, m.Event, m.Time.UTC().Format(time.RFC3339))
	qp.Close()
	return b.Bytes()
}
