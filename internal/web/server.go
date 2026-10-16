// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web serves Drawbridge's UI: server-rendered pages enhanced with
// HTMX, live updates over Server-Sent Events, no JavaScript build step.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/agents"
	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/docker"
	"github.com/zjcochran/wan-drawbridge/internal/hosts"
	"github.com/zjcochran/wan-drawbridge/internal/monitor"
	"github.com/zjcochran/wan-drawbridge/internal/notify"
	"github.com/zjcochran/wan-drawbridge/internal/secrets"
	"github.com/zjcochran/wan-drawbridge/internal/store"
	"github.com/zjcochran/wan-drawbridge/internal/syslog"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Monitor is what the UI needs from the check loop.
type Monitor interface {
	Status() monitor.Status
	Detection(ctx context.Context) (config.Detection, error)
	ApplyDetection(ctx context.Context, d config.Detection) error
	SetPaused(ctx context.Context, paused bool, actor string) error
	PauseFor(ctx context.Context, d time.Duration, actor string) error
	StartDrill(ctx context.Context, d time.Duration, actor string) error
	EndDrill(ctx context.Context, actor string) error
	CompleteSetup(ctx context.Context, actor string) error
	FailoverNow(ctx context.Context, actor string) error
	RestoreNow(ctx context.Context, actor string) error
	CheckNow(ctx context.Context) error
	Relearn(ctx context.Context, actor string) error
	PoliciesChanged()
	LoadBackup(ctx context.Context, replace func(context.Context) error) error
}

// Docker is what the UI needs from Docker: live container state.
type Docker interface {
	List(ctx context.Context) ([]docker.Container, error)
	Ping(ctx context.Context) (string, error)
}

// Syncer refreshes the container inventory.
type Syncer interface {
	Sync(ctx context.Context) error
}

// Server is the web UI. Fill in the exported fields and pass it to New.
type Server struct {
	Store   *store.Store
	Mon     Monitor
	Docker  Docker
	Hosts   *hosts.Registry // every host, for live container state
	Agents  *agents.Manager // agent hosts; nil: no agent API
	Sync    Syncer
	Hub     *Hub
	Log     *slog.Logger
	BaseURL string // external URL; https marks cookies Secure
	DBPath  string
	// TrustedProxies are reverse proxies whose X-Forwarded-* headers are believed.
	TrustedProxies []netip.Prefix
	Secrets        *secrets.Box // seals the OIDC client secret and notification URLs and tokens
	Notify         *notify.Notifier
	Syslog         *syslog.Writer // nil when the log is not sent to syslog
	BackupDir      string         // where scheduled backups are written
	MetricsToken   string         // enables /metrics with this bearer token
	Version        string
	SourceURL      string // where to get this build's source (AGPL section 13)
	ForceLocal     bool   // DRAWBRIDGE_LOCAL_LOGIN=force: passwords work even in SSO-only mode

	oidc *oidcFlows
	// codes holds sign-ins waiting for their two-factor code.
	codes *pendingCodes

	now     func() time.Time
	pages   map[string]*template.Template
	limiter *limiter
}

// New checks s, sets up its internal state and parses the templates.
func New(s Server) (*Server, error) {
	srv := &s
	srv.now = time.Now
	srv.limiter = newLimiter(5, 15*time.Minute)
	srv.oidc = &oidcFlows{pending: map[string]pendingLogin{}}
	srv.codes = &pendingCodes{m: map[string]*pendingCode{}}
	if srv.Secrets == nil {
		return nil, errors.New("web: Secrets is required")
	}
	if srv.Notify == nil {
		srv.Notify = &notify.Notifier{Store: srv.Store, Secrets: srv.Secrets, Log: srv.Log, Version: srv.Version}
	}
	if err := srv.parseTemplates(); err != nil {
		return nil, err
	}
	if srv.Agents != nil {
		srv.Agents.ClientIP = srv.clientIP
	}
	return srv, nil
}

func (s *Server) parseTemplates() error {
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	s.pages = map[string]*template.Template{}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "templates/"), ".html")
		if name == "layout" || name == "partials" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", f)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

// Handler returns the routed, protected handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	if s.Agents != nil {
		// Agents sign every request; sessions and CSRF do not apply.
		mux.Handle("/api/agent/v1/", s.Agents.Handler())
	}

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /login/code", s.loginCode)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /auth/oidc/login", s.oidcLogin)
	mux.HandleFunc("GET /auth/oidc/callback", s.oidcCallback)

	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup/admin", s.setupAdmin)
	mux.Handle("POST /setup/restore", limitBody(maxBackupUpload, http.HandlerFunc(s.setupRestore)))
	mux.Handle("POST /setup/detection", s.admin(s.setupDetection))
	mux.Handle("POST /setup/check", s.admin(s.setupCheck))
	mux.Handle("POST /setup/primary", s.admin(s.setupPrimary))
	mux.Handle("POST /setup/finish", s.admin(s.setupFinish))

	mux.Handle("GET /{$}", s.auth(s.dashboard))
	mux.Handle("GET /partials/status", s.auth(s.statusPartial))
	mux.Handle("GET /partials/timeline", s.auth(s.timelinePartial))
	mux.Handle("GET /sse", s.auth(s.Hub.ServeHTTP))
	mux.Handle("POST /actions/{action}", s.admin(s.action))

	mux.Handle("GET /stacks", s.auth(s.stacksPage))
	mux.Handle("GET /stacks/{host}/{project}/containers", s.auth(s.stackContainers))
	mux.Handle("POST /stacks/{host}/{project}/policy", s.admin(s.setStackPolicy))

	mux.Handle("GET /containers", s.auth(s.containersPage))
	mux.Handle("POST /containers/policy", s.admin(s.setContainerPolicy))
	mux.Handle("POST /containers/bulk", s.admin(s.bulkContainerPolicy))

	mux.Handle("GET /isps", s.auth(s.ispsPage))
	mux.Handle("POST /isps/{id}/role", s.admin(s.setISPRole))
	mux.Handle("POST /isps/{id}/rename", s.admin(s.renameISP))
	mux.Handle("POST /isps/{id}/metered", s.admin(s.setISPMetered))
	mux.Handle("POST /isps/relearn", s.admin(s.relearn))
	mux.Handle("POST /asns/{asn}/move", s.admin(s.moveASN))

	mux.Handle("GET /events", s.auth(s.eventsPage))
	mux.Handle("GET /events.csv", s.auth(s.eventsCSV))

	mux.Handle("GET /reports", s.auth(s.reportsPage))
	mux.Handle("GET /reports/outages/{id}", s.auth(s.outagePage))

	mux.Handle("GET /settings", s.auth(s.settingsPage(settingsTabs[0])))
	for _, t := range settingsTabs {
		if t.Admin {
			mux.Handle("GET /settings/"+t.ID, s.admin(s.settingsPage(t)))
		} else {
			mux.Handle("GET /settings/"+t.ID, s.auth(s.settingsPage(t)))
		}
	}
	mux.Handle("POST /settings/detection", s.admin(s.saveDetection))
	mux.Handle("POST /settings/test-schedule", s.admin(s.saveTestSchedule))
	mux.Handle("POST /settings/password", s.auth(s.changePassword))
	mux.Handle("POST /settings/appearance", s.auth(s.saveAppearance))
	mux.Handle("POST /settings/picture", limitBody(MaxPictureBytes+64<<10, s.auth(s.uploadPicture)))
	mux.Handle("POST /settings/picture/remove", s.auth(s.removePicture))
	mux.Handle("POST /settings/picture/sso", s.auth(s.useSSOPicture))
	mux.Handle("GET /users/{id}/picture", s.auth(s.userPicture))
	mux.Handle("POST /settings/sso", s.admin(s.saveSSO))
	mux.Handle("POST /settings/sso/test", s.admin(s.testSSO))
	mux.Handle("POST /settings/sso/unlink", s.auth(s.unlinkSSO))
	mux.Handle("POST /settings/2fa/start", s.auth(s.startTwoFactor))
	mux.Handle("POST /settings/2fa/enable", s.auth(s.enableTwoFactor))
	mux.Handle("POST /settings/2fa/codes", s.auth(s.newRecoveryCodes))
	mux.Handle("POST /settings/2fa/off", s.auth(s.disableTwoFactor))
	mux.Handle("POST /settings/api-tokens", s.admin(s.createAPIToken))
	mux.Handle("POST /settings/api-tokens/{id}/delete", s.admin(s.deleteAPIToken))
	mux.Handle("POST /settings/backup", s.admin(s.downloadBackup))
	mux.Handle("POST /settings/backup/schedule", s.admin(s.saveSchedule))
	mux.Handle("POST /settings/backup/run", s.admin(s.runBackupNow))
	mux.Handle("GET /settings/backups/{name}", s.admin(s.downloadStoredBackup))
	mux.Handle("POST /settings/notifications", s.admin(s.saveChannel))
	mux.Handle("POST /settings/notifications/delete", s.admin(s.deleteChannel))
	mux.Handle("POST /settings/notifications/test", s.admin(s.testChannel))
	mux.Handle("POST /settings/restore", limitBody(maxBackupUpload, s.admin(s.restoreBackup)))

	// Users moved into Settings; the old address still works.
	mux.Handle("GET /users", s.admin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings/users", http.StatusMovedPermanently)
	})))
	mux.Handle("POST /settings/container-filters", s.admin(s.saveContainerFilters))
	mux.Handle("POST /settings/hosts", s.admin(s.addHost))
	mux.Handle("POST /settings/hosts/manager-key", s.admin(s.rotateManagerKey))
	mux.Handle("POST /settings/hosts/{id}/{op}", s.admin(s.hostOp))
	mux.Handle("POST /settings/users", s.admin(s.createUser))
	mux.Handle("POST /settings/users/{id}/{op}", s.admin(s.userOp))

	var h http.Handler = mux
	h = s.requireSetup(h)
	cop := http.NewCrossOriginProtection()
	if s.BaseURL != "" {
		// A proxy that rewrites Host would otherwise make the browser's Origin look foreign.
		if err := cop.AddTrustedOrigin(s.BaseURL); err != nil {
			s.Log.Warn("DRAWBRIDGE_BASE_URL is not a valid origin", "url", s.BaseURL, "err", err)
		}
	}
	h = cop.Handler(h)
	return securityHeaders(h)
}

// cacheStatic lets browsers keep static files. Pages link them with
// ?v=<staticVersion>, which changes with their content, so those are kept
// for a year; a plain link is rechecked after an hour.
func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		h.ServeHTTP(w, r)
	})
}

// staticVersion is a hash of every static file, so an upgrade that changes
// any of them gives them new URLs and browsers drop their cached copies.
var staticVersion = sync.OnceValue(func() string {
	h := sha256.New()
	fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // embedded: cannot fail
		if err == nil && !d.IsDir() {
			b, _ := staticFS.ReadFile(p)
			fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
})

// staticURL is the versioned link to a static file.
func staticURL(name string) string { return "/static/" + name + "?v=" + staticVersion() }

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

// requireSetup sends every request to the wizard until an admin exists.
func (s *Server) requireSetup(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || p == "/metrics" || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/setup") {
			h.ServeHTTP(w, r)
			return
		}
		n, err := s.Store.CountUsers(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if n == 0 {
			redirect(w, r, "/setup")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---- sessions and auth ----

const sessionCookie = "drawbridge_session"

type ctxKey struct{}

func sessionFrom(r *http.Request) *store.Session {
	s, _ := r.Context().Value(ctxKey{}).(*store.Session)
	return s
}

func (s *Server) loadSession(r *http.Request) (*store.Session, error) {
	c, err := r.Cookie(sessionCookie)
	if errors.Is(err, http.ErrNoCookie) || (err == nil && c.Value == "") {
		return nil, nil // not signed in
	}
	sess, ok, err := s.Store.SessionByToken(r.Context(), c.Value, s.now())
	if err != nil || !ok {
		return nil, err
	}
	return &sess, nil
}

// auth requires a logged-in user; POSTs also need the session's CSRF token.
func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.loadSession(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if sess == nil {
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()))
			return
		}
		if r.Method == http.MethodPost && !validCSRF(r, sess.CSRF) {
			http.Error(w, "invalid or missing CSRF token; reload the page", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

// admin is auth plus the admin role.
func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !sessionFrom(r).User.IsAdmin() {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

func validCSRF(r *http.Request, want string) bool {
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.PostFormValue("_csrf")
	}
	return got != "" && got == want
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	token, err := s.Store.CreateSession(r.Context(), u.ID, s.clientIP(r), r.UserAgent(), s.now())
	if err != nil {
		return err
	}
	// Secure whenever the browser uses HTTPS; plain-HTTP setups need the cookie too.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set from s.secure(r)
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure(r),
		SameSite: http.SameSiteLaxMode, MaxAge: int(store.SessionTTL.Seconds()),
	})
	return nil
}

// limiter counts failed logins per key in a sliding window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{max: limit, window: window, fails: map[string][]time.Time{}}
}

func (l *limiter) blocked(now time.Time, keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if len(l.recent(now, k)) >= l.max {
			return true
		}
	}
	return false
}

func (l *limiter) fail(now time.Time, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		l.fails[k] = append(l.recent(now, k), now)
	}
}

func (l *limiter) reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}

func (l *limiter) recent(now time.Time, k string) []time.Time {
	ts := l.fails[k]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, k)
	}
	return ts
}

// ---- rendering ----

// Page is what every full page template gets.
type Page struct {
	Title   string
	Nav     string // which nav item is active
	Session *store.Session
	Status  monitor.Status
	Setup   bool // the setup wizard has been completed
	Flash   string
	Version string
	Source  string
	Data    any
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	setup, _ := s.Store.Flag(r.Context(), store.SettingSetupComplete)
	p := Page{
		Title: title, Nav: name, Session: sessionFrom(r), Status: s.Mon.Status(),
		Setup: setup, Flash: r.URL.Query().Get("flash"), Data: data,
		Version: s.Version, Source: s.SourceURL,
	}
	s.execute(w, r, name, "layout", p)
}

// partial renders one named template from a page's file.
func (s *Server) partial(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	s.execute(w, r, page, name, data)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	t, ok := s.pages[page]
	if !ok {
		s.fail(w, r, fmt.Errorf("no page %q", page))
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(buf.String()))
}

// event records an audit event. A failed write is logged; it does not fail
// the request that caused it.
func (s *Server) event(ctx context.Context, typ string, payload map[string]any) {
	if err := s.Store.AddEvent(ctx, s.now(), typ, payload); err != nil {
		s.Log.Error("cannot record event", "type", typ, "err", err)
	}
}

// fail logs an unexpected error and shows a generic message.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.Log.Error("web request failed", "path", r.URL.Path, "err", err)
	http.Error(w, "something went wrong: "+err.Error(), http.StatusInternalServerError)
}

// invalid reports a user error. HTMX shows it in the toast.
func invalid(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusUnprocessableEntity)
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Callers pass fixed paths or safeNext output, which only allows local paths.
	http.Redirect(w, r, to, http.StatusSeeOther) //nolint:gosec // G710: see above
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") != "" }

func actorName(r *http.Request) string {
	if s := sessionFrom(r); s != nil {
		return s.User.Username
	}
	return "unknown"
}
