// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// SettingOIDC holds OIDCSettings as JSON. The client secret is sealed.
const SettingOIDC = "oidc"

// OIDCSettings configure single sign-on (Pocket ID or any OIDC provider).
type OIDCSettings struct {
	Enabled       bool   `json:"enabled"`
	Issuer        string `json:"issuer"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"` // sealed with the secret key
	DisplayName   string `json:"display_name"`
	AutoProvision bool   `json:"auto_provision"`
	AllowedGroup  string `json:"allowed_group"`
	AdminGroup    string `json:"admin_group"`
	SSOOnly       bool   `json:"sso_only"`
}

// Name is the sign-in button's label: DisplayName, or "SSO".
func (o OIDCSettings) Name() string {
	if o.DisplayName != "" {
		return o.DisplayName
	}
	return "SSO"
}

func (s *Server) oidcSettings(ctx context.Context) (OIDCSettings, error) {
	o := OIDCSettings{AutoProvision: true}
	_, err := s.Store.GetSetting(ctx, SettingOIDC, &o)
	return o, err
}

// localAllowed reports whether password sign-in is on.
func (s *Server) localAllowed(o OIDCSettings) bool {
	return !o.Enabled || !o.SSOOnly || s.ForceLocal
}

// externalURL is how browsers reach Drawbridge, for the OIDC callback.
func (s *Server) externalURL(r *http.Request) string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	scheme := "http"
	if s.secure(r) {
		scheme = "https"
	}
	host := r.Host
	if s.fromProxy(r) && r.Header.Get("X-Forwarded-Host") != "" {
		host = r.Header.Get("X-Forwarded-Host")
	}
	return scheme + "://" + host
}

func (s *Server) callbackURL(r *http.Request) string { return s.externalURL(r) + "/auth/oidc/callback" }

// oidcFlows holds the discovered provider and logins in progress.
type oidcFlows struct {
	mu       sync.Mutex
	issuer   string
	provider *oidc.Provider
	meta     providerMeta
	keys     oidc.KeySet // the provider's published keys (JWKS)
	pending  map[string]pendingLogin
}

// providerMeta is the part of the discovery document Drawbridge adapts to.
type providerMeta struct {
	JWKSURI string   `json:"jwks_uri"`
	Scopes  []string `json:"scopes_supported"`
	Algs    []string `json:"id_token_signing_alg_values_supported"`
}

// scopes are the scopes to request. "groups" is not a standard scope:
// providers that list their scopes without it (Entra ID, Google) refuse a
// sign-in that asks for it, so it is requested only if listed, or if the
// provider does not list its scopes at all.
func (m providerMeta) scopes() []string {
	sc := []string{oidc.ScopeOpenID, "profile", "email"}
	if len(m.Scopes) == 0 || slices.Contains(m.Scopes, "groups") {
		sc = append(sc, "groups")
	}
	return sc
}

// hmacKeySet verifies ID tokens signed with HS256, whose key is the client
// secret (OIDC Core 10.1). Other algorithms go to the provider's published
// keys. The published keys are never used as an HMAC key: that confusion
// would let anyone forge tokens.
type hmacKeySet struct {
	secret []byte
	keys   oidc.KeySet
}

func (k hmacKeySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return k.keys.VerifySignature(ctx, raw) // not HS256
	}
	return jws.Verify(k.secret)
}

// verifier checks ID tokens. HS256 is accepted only when the provider
// lists it and a client secret is saved; otherwise the library's defaults
// apply (asymmetric algorithms only).
func (s *Server) verifier(p *oidc.Provider, clientID, secret string) *oidc.IDTokenVerifier {
	s.oidc.mu.Lock()
	issuer, meta, keys := s.oidc.issuer, s.oidc.meta, s.oidc.keys
	s.oidc.mu.Unlock()
	if secret == "" || !slices.Contains(meta.Algs, string(jose.HS256)) {
		return p.Verifier(&oidc.Config{ClientID: clientID})
	}
	var algs []string
	for _, a := range meta.Algs {
		if signingAlgs[a] {
			algs = append(algs, a)
		}
	}
	return oidc.NewVerifier(issuer, hmacKeySet{secret: []byte(secret), keys: keys},
		&oidc.Config{ClientID: clientID, SupportedSigningAlgs: algs})
}

// signingAlgs are the ID token algorithms Drawbridge accepts.
var signingAlgs = map[string]bool{
	oidc.RS256: true, oidc.RS384: true, oidc.RS512: true, oidc.ES256: true, oidc.ES384: true, oidc.ES512: true,
	oidc.PS256: true, oidc.PS384: true, oidc.PS512: true, oidc.EdDSA: true, string(jose.HS256): true,
}

type pendingLogin struct {
	verifier, nonce, next string
	linkUser              int // set when a signed-in user links their account
	expires               time.Time
}

func (s *Server) provider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	s.oidc.mu.Lock()
	defer s.oidc.mu.Unlock()
	if s.oidc.provider != nil && s.oidc.issuer == issuer {
		return s.oidc.provider, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", issuer, err)
	}
	var meta providerMeta
	if err := p.Claims(&meta); err != nil {
		return nil, fmt.Errorf("cannot read the discovery document of %s: %w", issuer, err)
	}
	s.oidc.issuer, s.oidc.provider, s.oidc.meta = issuer, p, meta
	s.oidc.keys = oidc.NewRemoteKeySet(ctx, meta.JWKSURI)
	return p, nil
}

func (s *Server) scopes() []string {
	s.oidc.mu.Lock()
	defer s.oidc.mu.Unlock()
	return s.oidc.meta.scopes()
}

func (s *Server) forgetProvider() {
	s.oidc.mu.Lock()
	s.oidc.provider = nil
	s.oidc.mu.Unlock()
}

func (s *Server) oauthConfig(ctx context.Context, r *http.Request, o OIDCSettings) (*oidc.Provider, oauth2.Config, error) {
	p, err := s.provider(ctx, o.Issuer)
	if err != nil {
		return nil, oauth2.Config{}, err
	}
	secret, err := s.Secrets.Open(o.ClientSecret)
	if err != nil {
		return nil, oauth2.Config{}, err
	}
	return p, oauth2.Config{
		ClientID: o.ClientID, ClientSecret: secret, Endpoint: p.Endpoint(),
		RedirectURL: s.callbackURL(r),
		Scopes:      s.scopes(),
	}, nil
}

func randomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// oidcLogin starts the authorization code flow with PKCE.
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := s.oidcSettings(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !o.Enabled {
		http.NotFound(w, r)
		return
	}
	p := pendingLogin{
		verifier: oauth2.GenerateVerifier(), nonce: randomString(),
		next: safeNext(r.URL.Query().Get("next")), expires: s.now().Add(10 * time.Minute),
	}
	if r.URL.Query().Get("link") == "1" {
		sess, err := s.loadSession(r)
		if err != nil || sess == nil {
			redirect(w, r, "/login")
			return
		}
		p.linkUser, p.next = sess.User.ID, "/settings/account"
	}
	_, cfg, err := s.oauthConfig(ctx, r, o)
	if err != nil {
		s.loginError(w, r, o, err.Error())
		return
	}
	state := randomString()
	s.oidc.mu.Lock()
	for k, v := range s.oidc.pending {
		if s.now().After(v.expires) {
			delete(s.oidc.pending, k)
		}
	}
	s.oidc.pending[state] = p
	s.oidc.mu.Unlock()
	http.Redirect(w, r, cfg.AuthCodeURL(state, oidc.Nonce(p.nonce), oauth2.S256ChallengeOption(p.verifier)), http.StatusFound)
}

type ssoClaims struct {
	Subject           string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	Groups            []string `json:"groups"`
	Picture           string   `json:"picture"`
	Nonce             string   `json:"nonce"`
}

func (c ssoClaims) username() string {
	switch {
	case c.PreferredUsername != "":
		return c.PreferredUsername
	case c.Email != "":
		return c.Email
	}
	return c.Subject
}

// oidcCallback finishes the flow: verify, then sign in, provision or link.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := s.oidcSettings(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !o.Enabled {
		http.NotFound(w, r)
		return
	}
	login, msg := s.verifyCallback(r, o)
	if msg != "" {
		s.loginError(w, r, o, msg)
		return
	}
	if login.pending.linkUser != 0 {
		s.linkAccount(w, r, o, login)
		return
	}
	u, msg, err := s.ssoAccount(ctx, o, login)
	switch {
	case err != nil:
		s.fail(w, r, err)
		return
	case msg != "":
		s.loginError(w, r, o, msg)
		return
	}
	s.refreshSSOPicture(ctx, u.ID, login)
	if err := s.Store.TouchLogin(ctx, u.ID, s.now()); err != nil {
		s.Log.Warn("cannot record sign-in time", "user", u.Username, "err", err)
	}
	if err := s.startSession(w, r, u); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, login.pending.next)
}

// ssoLogin is a verified sign-in from the provider.
type ssoLogin struct {
	pending         pendingLogin
	claims          ssoClaims
	issuer, subject string
	accessToken     string
}

// verifyCallback checks the state, exchanges the code (with the PKCE
// verifier), verifies the ID token and nonce, and applies the allowed group.
// A non-empty message means the sign-in is refused, with that explanation.
func (s *Server) verifyCallback(r *http.Request, o OIDCSettings) (ssoLogin, string) {
	ctx := r.Context()
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		return ssoLogin{}, fmt.Sprintf("%s refused the sign-in: %s %s", o.Name(), e, q.Get("error_description"))
	}
	s.oidc.mu.Lock()
	p, ok := s.oidc.pending[q.Get("state")]
	delete(s.oidc.pending, q.Get("state"))
	s.oidc.mu.Unlock()
	if !ok || s.now().After(p.expires) {
		return ssoLogin{}, "That sign-in link expired or was already used. Try again."
	}

	provider, cfg, err := s.oauthConfig(ctx, r, o)
	if err != nil {
		return ssoLogin{}, err.Error()
	}
	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.verifier))
	if err != nil {
		s.Log.Warn("oidc code exchange failed", "err", err)
		return ssoLogin{}, "Could not complete the sign-in with " + o.Name() + ". Check the client ID and secret."
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := s.verifier(provider, o.ClientID, cfg.ClientSecret).Verify(ctx, raw)
	if err != nil {
		s.Log.Warn("oidc id token rejected", "err", err)
		return ssoLogin{}, "The sign-in token from " + o.Name() + " was not valid."
	}
	var c ssoClaims
	if err := idt.Claims(&c); err != nil || c.Nonce != p.nonce {
		return ssoLogin{}, "The sign-in token from " + o.Name() + " was not valid."
	}
	if c.Groups == nil && (o.AllowedGroup != "" || o.AdminGroup != "") {
		// Some providers only put groups in the userinfo response.
		if ui, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			var extra ssoClaims
			if ui.Claims(&extra) == nil {
				c.Groups = extra.Groups
			}
		}
	}
	s.Log.Info("sso sign-in", "user", c.username(), "groups", c.Groups)
	if o.AllowedGroup != "" && !inGroup(c.Groups, o.AllowedGroup) {
		s.Log.Warn("sso sign-in refused: not in allowed group", "user", c.username(), "group", o.AllowedGroup)
		return ssoLogin{}, fmt.Sprintf("Your %s account is not in the %q group.", o.Name(), o.AllowedGroup)
	}
	return ssoLogin{pending: p, claims: c, issuer: idt.Issuer, subject: idt.Subject, accessToken: tok.AccessToken}, ""
}

// inGroup reports whether group is in groups, ignoring case: providers may
// send a group's lower-case name while people type its display name.
func inGroup(groups []string, group string) bool {
	return slices.ContainsFunc(groups, func(g string) bool { return strings.EqualFold(g, group) })
}

// linkAccount links a verified identity to the signed-in user who started it.
func (s *Server) linkAccount(w http.ResponseWriter, r *http.Request, o OIDCSettings, l ssoLogin) {
	sess, err := s.loadSession(r)
	if err != nil || sess == nil || sess.User.ID != l.pending.linkUser {
		s.loginError(w, r, o, "Sign in again, then link your account.")
		return
	}
	if err := s.Store.LinkIdentity(r.Context(), l.pending.linkUser, l.issuer, l.subject, l.claims.Email, s.now()); err != nil {
		redirect(w, r, "/settings/account?flash="+url.QueryEscape(err.Error()))
		return
	}
	s.event(r.Context(), "sso_linked", map[string]any{"user": sess.User.Username, "sso_user": l.claims.username()})
	s.refreshSSOPicture(r.Context(), sess.User.ID, l)
	msg := o.Name() + " account linked."
	// A linked account signs in with the provider's own second factor.
	if on, err := s.Store.DisableTwoFactor(r.Context(), sess.User.ID); err != nil {
		s.Log.Error("cannot turn off two-factor sign-in", "err", err)
	} else if on {
		s.event(r.Context(), "two_factor_off", map[string]any{"user": sess.User.Username, "by": "sso link"})
		msg += " Two-factor sign-in is now off: use " + o.Name() + "'s."
	}
	redirect(w, r, "/settings/account?flash="+url.QueryEscape(msg))
}

// ssoAccount finds the account for a verified identity, applying the admin
// group, or creates one when auto-provisioning is on. A non-empty message
// means the sign-in is refused.
func (s *Server) ssoAccount(ctx context.Context, o OIDCSettings, l ssoLogin) (store.User, string, error) {
	inAdmin := o.AdminGroup != "" && inGroup(l.claims.Groups, o.AdminGroup)
	u, found, err := s.Store.UserByIdentity(ctx, l.issuer, l.subject)
	if err != nil {
		return store.User{}, "", err
	}
	switch {
	case found && u.Disabled:
		return store.User{}, "Your account is disabled.", nil
	case found:
		// Accounts created by single sign-on follow the admin group.
		if !u.HasLocal && o.AdminGroup != "" {
			role := map[bool]string{true: store.RoleAdmin, false: store.RoleViewer}[inAdmin]
			if role != u.Role && s.Store.SetUserRole(ctx, u.ID, role) == nil {
				u.Role = role
				s.event(ctx, "user_role", map[string]any{"user": u.Username, "role": role, "by": "sso groups"})
			}
		}
		return u, "", nil
	case o.AutoProvision:
		role := store.RoleViewer
		if inAdmin {
			role = store.RoleAdmin
		}
		u, err := s.Store.CreateSSOUser(ctx, l.claims.username(), role, l.issuer, l.subject, l.claims.Email, s.now())
		if err != nil {
			return store.User{}, "", err
		}
		s.event(ctx, "user_created", map[string]any{"user": u.Username, "role": role, "by": "sso"})
		return u, "", nil
	}
	return store.User{}, fmt.Sprintf("No Drawbridge account is linked to your %s account. Ask an admin to add you.", o.Name()), nil
}

func (s *Server) loginError(w http.ResponseWriter, r *http.Request, o OIDCSettings, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	s.page(w, r, "login", "Sign in", loginData{Error: msg, Next: "/", SSO: o.Enabled, SSOName: o.Name(), Local: s.localAllowed(o)})
}

// ---- settings ----

type ssoForm struct {
	OIDCSettings
	HasSecret   bool
	CallbackURL string
	Error       string
	Wizard      bool
	Linked      []store.Identity
}

func (s *Server) ssoForm(r *http.Request, o OIDCSettings) ssoForm {
	f := ssoForm{OIDCSettings: o, HasSecret: o.ClientSecret != "", CallbackURL: s.callbackURL(r)}
	f.ClientSecret = ""
	return f
}

func (s *Server) saveSSO(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := s.oidcSettings(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	next := OIDCSettings{
		Enabled:       r.PostFormValue("enabled") == "on",
		Issuer:        strings.TrimRight(strings.TrimSpace(r.PostFormValue("issuer")), "/"),
		ClientID:      strings.TrimSpace(r.PostFormValue("client_id")),
		ClientSecret:  o.ClientSecret,
		DisplayName:   strings.TrimSpace(r.PostFormValue("display_name")),
		AutoProvision: r.PostFormValue("auto_provision") == "on",
		AllowedGroup:  strings.TrimSpace(r.PostFormValue("allowed_group")),
		AdminGroup:    strings.TrimSpace(r.PostFormValue("admin_group")),
		SSOOnly:       r.PostFormValue("sso_only") == "on",
	}
	if v := r.PostFormValue("client_secret"); v != "" {
		if next.ClientSecret, err = s.Secrets.Seal(v); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if r.PostFormValue("clear_secret") == "on" {
		next.ClientSecret = ""
	}
	if next.Enabled {
		u, err := url.Parse(next.Issuer)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			invalid(w, "The issuer must be a URL such as https://id.example.com.")
			return
		}
		if next.ClientID == "" {
			invalid(w, "The client ID is required.")
			return
		}
	}
	if next.SSOOnly {
		me := sessionFrom(r).User
		ids, err := s.Store.Identities(ctx, me.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if len(ids) == 0 {
			invalid(w, "Link your own account to "+next.Name()+" and sign in with it once before turning off password sign-in.")
			return
		}
	}
	if err := s.Store.SetSetting(ctx, SettingOIDC, next); err != nil {
		s.fail(w, r, err)
		return
	}
	s.forgetProvider()
	s.event(ctx, "sso_settings", map[string]any{"enabled": next.Enabled, "sso_only": next.SSOOnly, "by": actorName(r)})
	if r.PostFormValue("from") == "setup" {
		redirect(w, r, "/setup?step=3")
		return
	}
	w.Header().Set("HX-Trigger", `{"toast":"Single sign-on settings saved."}`)
	f := s.ssoForm(r, next)
	s.partial(w, r, "settings", "sso-form", f)
}

// testSSO checks that the issuer's discovery document loads.
func (s *Server) testSSO(w http.ResponseWriter, r *http.Request) {
	issuer := strings.TrimRight(strings.TrimSpace(r.PostFormValue("issuer")), "/")
	s.forgetProvider()
	p, err := s.provider(r.Context(), issuer)
	msg := ""
	if err != nil {
		msg = "✗ " + err.Error()
	} else {
		msg = "✓ Found " + issuer + " (sign-in at " + p.Endpoint().AuthURL + ")"
	}
	s.partial(w, r, "settings", "sso-test", msg)
}

func (s *Server) unlinkSSO(w http.ResponseWriter, r *http.Request) {
	me := sessionFrom(r).User
	if err := s.Store.UnlinkIdentities(r.Context(), me.ID); err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "sso_unlinked", map[string]any{"user": me.Username})
	redirect(w, r, "/settings/account?flash="+url.QueryEscape("Single sign-on unlinked."))
}

var errSSOOnly = errors.New("password sign-in is turned off; use single sign-on")
