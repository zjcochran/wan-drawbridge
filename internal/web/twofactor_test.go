// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/totp"
)

var (
	sealedRe   = regexp.MustCompile(`name="sealed" value="([^"]+)"`)
	secretRe   = regexp.MustCompile(`Key: <code>([A-Z2-7]+)</code>`)
	recoveryRe = regexp.MustCompile(`[a-z2-9]{4}-[a-z2-9]{4}-[a-z2-9]{4}`)
)

const password = "test-password-123"

// turnOnTwoFactor sets up two-factor sign-in for the signed-in client and
// returns the secret, the time step of the code it used, and the recovery
// codes.
func turnOnTwoFactor(t *testing.T, c *client) (string, int64, []string) {
	t.Helper()
	resp, body := c.htmx("/settings/2fa/start", url.Values{})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `src="data:image/png;base64,`) {
		t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	sealed, secret := html.UnescapeString(sealedRe.FindStringSubmatch(body)[1]), secretRe.FindStringSubmatch(body)[1]
	step := totp.Step(time.Now())
	code, _ := totp.Code(secret, step)
	if resp, _ := c.htmx("/settings/2fa/enable", url.Values{"sealed": {sealed}, "code": {code}, "password": {"wrong-password-1"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/settings/2fa/enable", url.Values{"sealed": {sealed}, "code": {"000000"}, "password": {password}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong code: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/settings/2fa/enable", url.Values{"sealed": {"v1:junk"}, "code": {code}, "password": {password}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("broken sealed secret: %d", resp.StatusCode)
	}
	resp, body = c.htmx("/settings/2fa/enable", url.Values{"sealed": {sealed}, "code": {code}, "password": {password}})
	codes := recoveryRe.FindAllString(body, -1)
	if resp.StatusCode != http.StatusOK || len(codes) != store.RecoveryCodeCount {
		t.Fatalf("enable: %d, %d codes", resp.StatusCode, len(codes))
	}
	return secret, step, codes
}

// signIn posts the password and returns the response page.
func signIn(c *client) (*http.Response, string) {
	return c.do("POST", "/login", url.Values{"username": {"admin"}, "password": {password}, "next": {"/reports"}})
}

func TestTwoFactorSignIn(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	secret, step, codes := turnOnTwoFactor(t, c)
	if _, page := c.get("/settings/account"); !strings.Contains(page, "10 recovery codes left") {
		t.Error("the account tab does not say two-factor is on")
	}
	if _, page := c.get("/settings/users"); !strings.Contains(page, "password + code") {
		t.Error("the users table does not show two-factor")
	}

	b := e.client()
	resp, page := signIn(b)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `action="/login/code"`) {
		t.Fatalf("password step: %d", resp.StatusCode)
	}
	if resp, page := b.do("POST", "/login/code", url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "not right") {
		t.Errorf("wrong code: %d", resp.StatusCode)
	}
	// The code used to turn it on cannot be used again; the next one works.
	used, _ := totp.Code(secret, step)
	if resp, _ := b.do("POST", "/login/code", url.Values{"code": {used}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a used code: %d", resp.StatusCode)
	}
	next, _ := totp.Code(secret, step+1)
	resp, _ = b.do("POST", "/login/code", url.Values{"code": {next}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/reports" {
		t.Fatalf("right code: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := b.get("/reports"); resp.StatusCode != http.StatusOK {
		t.Errorf("not signed in: %d", resp.StatusCode)
	}

	// A recovery code, typed in capitals without dashes, works once.
	r := e.client()
	signIn(r)
	typed := strings.ToUpper(strings.ReplaceAll(codes[0], "-", ""))
	if resp, _ := r.do("POST", "/login/code", url.Values{"code": {typed}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recovery code: %d", resp.StatusCode)
	}
	r2 := e.client()
	signIn(r2)
	if resp, _ := r2.do("POST", "/login/code", url.Values{"code": {codes[0]}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a used recovery code: %d", resp.StatusCode)
	}
	if evs, _ := e.st.Events(context.Background(), store.EventFilter{Type: "recovery_code_used"}); len(evs) != 1 || evs[0].Payload["left"] != float64(9) {
		t.Errorf("recovery_code_used events: %+v", evs)
	}
}

func TestTwoFactorCodeStepExpires(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	turnOnTwoFactor(t, c)
	// Without the cookie from the password step.
	if resp, page := e.client().do("POST", "/login/code", url.Values{"code": {"123456"}}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "expired") {
		t.Errorf("no pending sign-in: %d", resp.StatusCode)
	}
	// Five wrong codes use up the pending sign-in.
	b := e.client()
	signIn(b)
	for range codeAttempts {
		b.do("POST", "/login/code", url.Values{"code": {"000000"}})
	}
	if _, page := b.do("POST", "/login/code", url.Values{"code": {"000000"}}); !strings.Contains(page, "expired") &&
		!strings.Contains(page, "Too many") {
		t.Error("the pending sign-in survived its attempts")
	}
	// An expired one is dropped.
	p := &pendingCodes{m: map[string]*pendingCode{}}
	tok := p.add(&pendingCode{expires: time.Now().Add(-time.Second)}, time.Now())
	if _, ok := p.take(tok, time.Now()); ok {
		t.Error("an expired sign-in was accepted")
	}
	p.add(&pendingCode{expires: time.Now()}, time.Now().Add(time.Minute)) // sweeps the first
	if len(p.m) != 1 {
		t.Errorf("expired entries are not swept: %d", len(p.m))
	}
}

func TestTwoFactorManagement(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	turnOnTwoFactor(t, c)
	if resp, _ := c.htmx("/settings/2fa/start", url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("start while on: %d", resp.StatusCode)
	}
	resp, body := c.htmx("/settings/2fa/codes", url.Values{"password": {password}})
	if resp.StatusCode != http.StatusOK || len(recoveryRe.FindAllString(body, -1)) != store.RecoveryCodeCount {
		t.Fatalf("new codes: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/settings/2fa/off", url.Values{"password": {"wrong-password-1"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("off with a wrong password: %d", resp.StatusCode)
	}
	if resp, body := c.htmx("/settings/2fa/off", url.Values{"password": {password}}); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Set up") {
		t.Fatalf("off: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/settings/2fa/codes", url.Values{"password": {password}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("codes while off: %d", resp.StatusCode)
	}

	// An admin turns it off for someone else.
	u, _ := e.st.CreateUser(ctx, "viewer", password, store.RoleViewer, time.Now())
	e.st.EnableTwoFactor(ctx, u.ID, "v1:x", 0, nil)
	if resp, body := c.htmx("/settings/users/"+strconv.Itoa(u.ID)+"/two-factor", url.Values{}); resp.StatusCode != http.StatusOK || strings.Contains(body, "Turn off 2FA") {
		t.Fatalf("admin off: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/settings/users/999/two-factor", url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unknown user: %d", resp.StatusCode)
	}
	evs, _ := e.st.Events(ctx, store.EventFilter{Type: "two_factor_off"})
	if len(evs) != 2 || evs[0].Payload["by"] != "admin" || evs[0].Payload["user"] != "viewer" {
		t.Errorf("two_factor_off events: %+v", evs)
	}
}

func TestTwoFactorNotOfferedWithSSO(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	ctx := context.Background()
	e.st.LinkIdentity(ctx, 1, "https://id.example.com", "sub-1", "admin@example.com", time.Now())
	if _, page := c.get("/settings/account"); !strings.Contains(page, "use your provider's two-factor sign-in") || strings.Contains(page, "/settings/2fa/start") {
		t.Error("two-factor is offered to a linked account")
	}
	if resp, _ := c.htmx("/settings/2fa/start", url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("start with SSO linked: %d", resp.StatusCode)
	}
	if resp, _ := c.htmx("/settings/2fa/enable", url.Values{"password": {password}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("enable with SSO linked: %d", resp.StatusCode)
	}
}
