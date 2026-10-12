// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, an authorize endpoint
// that approves at once, and a token endpoint that checks PKCE.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	signer jose.Signer
	key    *rsa.PrivateKey

	mu     sync.Mutex
	user   map[string]any // claims for the next sign-in
	codes  map[string]authReq
	secret string
}

type authReq struct{ nonce, challenge, redirect string }

func newIdP(t *testing.T) *fakeIdP {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{t: t, signer: signer, key: key, codes: map[string]authReq{}, secret: "s3cret",
		user: map[string]any{"sub": "user-1", "preferred_username": "alice", "email": "alice@example.com"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks",
			"userinfo_endpoint": p.srv.URL + "/userinfo", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		code := fmt.Sprint(time.Now().UnixNano())
		p.mu.Lock()
		p.codes[code] = authReq{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, secret, _ := r.BasicAuth()
		if id == "" {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		p.mu.Lock()
		req, ok := p.codes[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code"))
		claims := map[string]any{}
		for k, v := range p.user {
			claims[k] = v
		}
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || id != "drawbridge" || secret != p.secret ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims["iss"], claims["aud"], claims["nonce"] = p.srv.URL, "drawbridge", req.nonce
		claims["iat"], claims["exp"] = time.Now().Unix(), time.Now().Add(time.Hour).Unix()
		payload, _ := json.Marshal(claims)
		jws, _ := p.signer.Sign(payload)
		tok, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": tok})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) as(claims map[string]any) {
	p.mu.Lock()
	p.user = claims
	p.mu.Unlock()
}

// configureSSO turns single sign-on on through the settings page.
func configureSSO(t *testing.T, c *client, idp *fakeIdP, extra url.Values) {
	t.Helper()
	form := url.Values{"enabled": {"on"}, "issuer": {idp.srv.URL}, "client_id": {"drawbridge"},
		"client_secret": {idp.secret}, "auto_provision": {"on"}}
	for k, v := range extra {
		form[k] = v
	}
	if resp, body := c.htmx("/settings/sso", form); resp.StatusCode != http.StatusOK {
		t.Fatalf("save sso: %d %s", resp.StatusCode, body)
	}
}

// ssoLogin follows a full sign-in: Drawbridge → provider → Drawbridge callback.
// It returns the callback's response.
func (c *client) ssoLogin(path string) (*http.Response, string) {
	c.e.t.Helper()
	resp, body := c.do("GET", path, nil)
	if resp.StatusCode != http.StatusFound {
		return resp, body // refused before reaching the provider
	}
	atIdP, err := c.http.Get(resp.Header.Get("Location"))
	if err != nil {
		c.e.t.Fatal(err)
	}
	atIdP.Body.Close()
	cb, err := url.Parse(atIdP.Header.Get("Location"))
	if err != nil || atIdP.StatusCode != http.StatusFound {
		c.e.t.Fatalf("provider did not redirect back: %d", atIdP.StatusCode)
	}
	return c.do("GET", cb.RequestURI(), nil)
}

func TestSSOAutoProvisionAndGroups(t *testing.T) {
	e := newEnv(t)
	idp := newIdP(t)
	admin := e.admin()
	configureSSO(t, admin, idp, url.Values{"admin_group": {"drawbridge-admins"}})

	// The secret is stored sealed, never in clear text.
	var raw map[string]any
	e.st.GetSetting(context.Background(), SettingOIDC, &raw)
	if s, _ := raw["client_secret"].(string); s == "" || strings.Contains(s, idp.secret) {
		t.Fatalf("client secret stored as %q", s)
	}

	// A plain member becomes a viewer.
	c := e.client()
	resp, _ := c.ssoLogin("/auth/oidc/login?next=/stacks")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/stacks" {
		t.Fatalf("sso login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	u, ok, _ := e.st.UserByIdentity(context.Background(), idp.srv.URL, "user-1")
	if !ok || u.Username != "alice" || u.Role != store.RoleViewer || u.HasLocal {
		t.Fatalf("provisioned %+v", u)
	}
	if r, _ := c.get("/stacks"); r.StatusCode != http.StatusOK {
		t.Fatalf("session not working: %d", r.StatusCode)
	}

	// Joining the admin group promotes them on the next sign-in.
	idp.as(map[string]any{"sub": "user-1", "preferred_username": "alice", "groups": []string{"drawbridge-admins"}})
	e.client().ssoLogin("/auth/oidc/login")
	if u, _, _ := e.st.UserByIdentity(context.Background(), idp.srv.URL, "user-1"); u.Role != store.RoleAdmin {
		t.Fatalf("role after joining admin group = %s", u.Role)
	}
}

func TestSSOAllowedGroupAndNoProvisioning(t *testing.T) {
	e := newEnv(t)
	idp := newIdP(t)
	admin := e.admin()
	configureSSO(t, admin, idp, url.Values{"allowed_group": {"staff"}})
	resp, body := e.client().ssoLogin("/auth/oidc/login")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "not in the &#34;staff&#34; group") {
		t.Fatalf("outsider: %d", resp.StatusCode)
	}

	configureSSO(t, admin, idp, url.Values{"auto_provision": {""}})
	resp, body = e.client().ssoLogin("/auth/oidc/login")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "No Drawbridge account is linked") {
		t.Fatalf("no provisioning: %d", resp.StatusCode)
	}
}

func TestSSOLinkThenSSOOnly(t *testing.T) {
	e := newEnv(t)
	idp := newIdP(t)
	admin := e.admin()
	configureSSO(t, admin, idp, nil)

	// SSO-only is refused until the admin has linked their own account.
	resp, body := admin.htmx("/settings/sso", url.Values{"enabled": {"on"}, "issuer": {idp.srv.URL},
		"client_id": {"drawbridge"}, "sso_only": {"on"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Link your own account") {
		t.Fatalf("sso-only without a link: %d", resp.StatusCode)
	}

	idp.as(map[string]any{"sub": "alice-sub", "preferred_username": "alice"})
	resp, _ = admin.ssoLogin("/auth/oidc/login?link=1")
	if !strings.HasPrefix(resp.Header.Get("Location"), "/settings?flash=") {
		t.Fatalf("link: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	u, ok, _ := e.st.UserByIdentity(context.Background(), idp.srv.URL, "alice-sub")
	if !ok || u.Username != "admin" {
		t.Fatalf("link went to %+v", u)
	}

	configureSSO(t, admin, idp, url.Values{"sso_only": {"on"}})
	pw := url.Values{"username": {"admin"}, "password": {"test-password-123"}}
	if resp, _ := e.client().do("POST", "/login", pw); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("password sign-in in SSO-only mode: %d", resp.StatusCode)
	}
	if _, body := e.client().do("GET", "/login", nil); strings.Contains(body, `name="password"`) {
		t.Fatal("password form shown in SSO-only mode")
	}
	// The linked account signs in as the existing admin, not a new user.
	resp, _ = e.client().ssoLogin("/auth/oidc/login")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("sso sign-in: %d", resp.StatusCode)
	}
	if n, _ := e.st.CountUsers(context.Background()); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}

	// Break glass.
	e.ui.ForceLocal = true
	if resp, _ := e.client().do("POST", "/login", pw); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("forced local sign-in: %d", resp.StatusCode)
	}
}

func TestSSORejectsBadState(t *testing.T) {
	e := newEnv(t)
	idp := newIdP(t)
	configureSSO(t, e.admin(), idp, nil)
	resp, body := e.client().do("GET", "/auth/oidc/callback?code=x&state=forged", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "expired or was already used") {
		t.Fatalf("forged state: %d", resp.StatusCode)
	}
}

func TestSSOWrongSecretFails(t *testing.T) {
	e := newEnv(t)
	idp := newIdP(t)
	configureSSO(t, e.admin(), idp, url.Values{"client_secret": {"wrong"}})
	resp, body := e.client().ssoLogin("/auth/oidc/login")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Check the client ID and secret") {
		t.Fatalf("wrong secret: %d", resp.StatusCode)
	}
}

func TestSSOTestConnection(t *testing.T) {
	e := newEnv(t)
	idp := newIdP(t)
	c := e.admin()
	if _, body := c.htmx("/settings/sso/test", url.Values{"issuer": {idp.srv.URL}}); !strings.Contains(body, "✓ Found") {
		t.Fatalf("test ok: %s", body)
	}
	broken := httptest.NewServer(http.NotFoundHandler()) // an issuer with no discovery document
	defer broken.Close()
	if _, body := c.htmx("/settings/sso/test", url.Values{"issuer": {broken.URL}}); !strings.Contains(body, "✗") {
		t.Fatalf("test fail: %s", body)
	}
}

func TestSSONonceIsChecked(t *testing.T) {
	e := newEnv(t)
	idp := newIdP(t)
	configureSSO(t, e.admin(), idp, nil)
	c := e.client()
	// Start a login, then replay the authorize step with a different nonce.
	resp, _ := c.do("GET", "/auth/oidc/login", nil)
	u, _ := url.Parse(resp.Header.Get("Location"))
	q := u.Query()
	q.Set("nonce", "attacker")
	u.RawQuery = q.Encode()
	r2, _ := c.http.Get(u.String())
	cb, _ := url.Parse(r2.Header.Get("Location"))
	resp, body := c.do("GET", cb.RequestURI(), nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "was not valid") {
		t.Fatalf("token with the wrong nonce accepted: %d", resp.StatusCode)
	}
}
