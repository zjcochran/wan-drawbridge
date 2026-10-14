// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"net/http"
	"sync"
	"time"

	"rsc.io/qr"

	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/totp"
)

// ---- Settings → Your account ----

// twoFactorView is the two-factor card on the account tab.
type twoFactorView struct {
	Offer  bool         // the user can use it: a password, no linked single sign-on, password sign-in allowed
	Linked bool         // not offered because single sign-on is linked
	On     bool         // turned on
	Left   int          // recovery codes left
	Secret string       // during setup: the secret to type in
	Sealed string       // during setup: the sealed secret, sent back with the code
	QR     template.URL // during setup: the QR code as a data: URL
	Codes  []string
}

func (s *Server) twoFactorView(r *http.Request) (twoFactorView, error) {
	ctx := r.Context()
	u, err := s.Store.UserByID(ctx, sessionFrom(r).User.ID)
	if err != nil {
		return twoFactorView{}, err
	}
	o, err := s.oidcSettings(ctx)
	if err != nil {
		return twoFactorView{}, err
	}
	v := twoFactorView{On: u.TwoFactor, Linked: u.HasSSO}
	v.Offer = u.HasLocal && !u.HasSSO && s.localAllowed(o)
	if v.On {
		v.Left, err = s.Store.RecoveryCodesLeft(ctx, u.ID)
	}
	return v, err
}

func (s *Server) renderTwoFactor(w http.ResponseWriter, r *http.Request, v twoFactorView) {
	s.partial(w, r, "settings", "two-factor", v)
}

// confirmPassword checks the current password sent with a two-factor change.
func (s *Server) confirmPassword(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.Store.VerifyPassword(r.Context(), sessionFrom(r).User.ID, r.PostFormValue("password"))
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	if !ok {
		invalid(w, "Your password is not right.")
	}
	return ok
}

// startTwoFactor shows a new secret and its QR code. Nothing is stored
// until enableTwoFactor gets a matching code.
func (s *Server) startTwoFactor(w http.ResponseWriter, r *http.Request) {
	v, err := s.twoFactorView(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !v.Offer || v.On {
		invalid(w, "Two-factor sign-in cannot be set up for this account.")
		return
	}
	v.Secret = totp.NewSecret()
	if v.Sealed, err = s.Secrets.Seal(v.Secret); err != nil {
		s.fail(w, r, err)
		return
	}
	code, err := qr.Encode(totp.URI("WAN Drawbridge", sessionFrom(r).User.Username, v.Secret), qr.M)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	code.Scale = 4
	// A PNG made here, not user input: safe as an image source.
	// nosemgrep
	v.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())) //nolint:gosec // G203: our own PNG

	s.renderTwoFactor(w, r, v)
}

func (s *Server) enableTwoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v, err := s.twoFactorView(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !v.Offer || v.On {
		invalid(w, "Two-factor sign-in cannot be set up for this account.")
		return
	}
	if !s.confirmPassword(w, r) {
		return
	}
	sealed := r.PostFormValue("sealed")
	secret, err := s.Secrets.Open(sealed)
	if err != nil || secret == "" {
		invalid(w, "The setup has expired. Start again.")
		return
	}
	step, ok := totp.Verify(secret, r.PostFormValue("code"), s.now())
	if !ok {
		invalid(w, "That code is not right. Check the time on your phone and try the next code.")
		return
	}
	codes, hashes := store.NewRecoveryCodes()
	me := sessionFrom(r).User
	if err := s.Store.EnableTwoFactor(ctx, me.ID, sealed, step, hashes); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(ctx, "two_factor_on", map[string]any{"user": me.Username})
	v.On, v.Left, v.Codes = true, len(codes), codes
	s.renderTwoFactor(w, r, v)
}

func (s *Server) newRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v, err := s.twoFactorView(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !v.On {
		invalid(w, "Two-factor sign-in is off.")
		return
	}
	if !s.confirmPassword(w, r) {
		return
	}
	codes, hashes := store.NewRecoveryCodes()
	me := sessionFrom(r).User
	if err := s.Store.ReplaceRecoveryCodes(ctx, me.ID, hashes); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(ctx, "recovery_codes_new", map[string]any{"user": me.Username})
	v.Left, v.Codes = len(codes), codes
	s.renderTwoFactor(w, r, v)
}

func (s *Server) disableTwoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.confirmPassword(w, r) {
		return
	}
	me := sessionFrom(r).User
	if on, err := s.Store.DisableTwoFactor(ctx, me.ID); err != nil {
		s.fail(w, r, err)
		return
	} else if on {
		s.event(ctx, "two_factor_off", map[string]any{"user": me.Username, "by": me.Username})
	}
	v, err := s.twoFactorView(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderTwoFactor(w, r, v)
}

// ---- sign-in ----

const (
	codeCookie   = "drawbridge_2fa"
	codeTTL      = 5 * time.Minute
	codeAttempts = 5
)

// pendingCode is a sign-in whose password was right and that waits for the
// code.
type pendingCode struct {
	user    store.User
	next    string
	expires time.Time
	tries   int
}

type pendingCodes struct {
	mu sync.Mutex
	m  map[string]*pendingCode
}

func (p *pendingCodes) add(pc *pendingCode, now time.Time) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.m {
		if now.After(v.expires) {
			delete(p.m, k)
		}
	}
	p.m[token] = pc
	return token
}

// take returns the pending sign-in for a token, counting the attempt; it is
// dropped when it has expired or used up its attempts.
func (p *pendingCodes) take(token string, now time.Time) (*pendingCode, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pc, ok := p.m[token]
	if !ok || now.After(pc.expires) || pc.tries >= codeAttempts {
		delete(p.m, token)
		return nil, false
	}
	pc.tries++
	return pc, true
}

func (p *pendingCodes) done(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, token)
}

func (s *Server) setCodeCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set from s.secure(r)
		Name: codeCookie, Value: token, Path: "/login", HttpOnly: true, Secure: s.secure(r),
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

// askForCode starts the second step of a password sign-in.
func (s *Server) askForCode(w http.ResponseWriter, r *http.Request, u store.User, data loginData) {
	token := s.codes.add(&pendingCode{user: u, next: data.Next, expires: s.now().Add(codeTTL)}, s.now())
	s.setCodeCookie(w, r, token, int(codeTTL.Seconds()))
	data.Code = true
	s.page(w, r, "login", "Sign in", data)
}

// loginCode checks the code of a two-factor sign-in: a code from the app,
// or a recovery code.
func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), s.now()
	data, err := s.loginData(r, "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := r.Cookie(codeCookie)
	var pc *pendingCode
	ok := err == nil
	if ok {
		pc, ok = s.codes.take(c.Value, now)
	}
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		data.Error = "The sign-in has expired. Sign in again."
		s.page(w, r, "login", "Sign in", data)
		return
	}
	data.Code, data.Next = true, pc.next
	keys := []string{"ip:" + s.clientIP(r), "user:" + pc.user.Username}
	if s.limiter.blocked(now, keys...) {
		w.WriteHeader(http.StatusTooManyRequests)
		data.Error = "Too many failed sign-ins. Try again in 15 minutes."
		s.page(w, r, "login", "Sign in", data)
		return
	}
	used, recovery, err := s.checkCode(r, pc.user.ID, r.PostFormValue("code"), now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !used {
		s.limiter.fail(now, keys...)
		s.Log.Warn("failed sign-in code", "user", pc.user.Username, "ip", s.clientIP(r))
		w.WriteHeader(http.StatusUnauthorized)
		data.Error = "That code is not right."
		s.page(w, r, "login", "Sign in", data)
		return
	}
	s.codes.done(c.Value)
	s.setCodeCookie(w, r, "", -1)
	s.limiter.reset(keys...)
	if recovery {
		left, _ := s.Store.RecoveryCodesLeft(ctx, pc.user.ID)
		s.event(ctx, "recovery_code_used", map[string]any{"user": pc.user.Username, "left": left})
	}
	if err := s.Store.TouchLogin(ctx, pc.user.ID, now); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.startSession(w, r, pc.user); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, pc.next)
}

// checkCode accepts a current app code (once) or an unused recovery code.
func (s *Server) checkCode(r *http.Request, id int, code string, now time.Time) (ok, recovery bool, err error) {
	ctx := r.Context()
	sealed, err := s.Store.TwoFactorSecret(ctx, id)
	if err != nil || sealed == "" {
		return false, false, err
	}
	secret, err := s.Secrets.Open(sealed)
	if err != nil {
		return false, false, err
	}
	if step, match := totp.Verify(secret, code, now); match {
		ok, err = s.Store.UseTOTPStep(ctx, id, step)
		return ok, false, err
	}
	ok, err = s.Store.UseRecoveryCode(ctx, id, code)
	return ok, ok, err
}
