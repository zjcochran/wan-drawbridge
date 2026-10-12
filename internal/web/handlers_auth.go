// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/zjcochran/wan-drawbridge/internal/config"
	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// ---- login ----

type loginData struct {
	Error, Username, Next string
	SSO                   bool // single sign-on is on
	SSOName               string
	Local                 bool // password sign-in is allowed
}

func (s *Server) loginData(r *http.Request, user, next string) (loginData, error) {
	o, err := s.oidcSettings(r.Context())
	return loginData{Username: user, Next: safeNext(next), SSO: o.Enabled, SSOName: o.Name(), Local: s.localAllowed(o)}, err
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if sess, _ := s.loadSession(r); sess != nil {
		redirect(w, r, "/")
		return
	}
	data, err := s.loginData(r, "", r.URL.Query().Get("next"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.page(w, r, "login", "Sign in", data)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.PostFormValue("username"))
	data, err := s.loginData(r, user, r.PostFormValue("next"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !data.Local {
		w.WriteHeader(http.StatusForbidden)
		data.Error = errSSOOnly.Error() + "."
		s.page(w, r, "login", "Sign in", data)
		return
	}
	keys := []string{"ip:" + s.clientIP(r), "user:" + strings.ToLower(user)}
	now := s.now()

	if s.limiter.blocked(now, keys...) {
		w.WriteHeader(http.StatusTooManyRequests)
		data.Error = "Too many failed sign-ins. Try again in 15 minutes."
		s.page(w, r, "login", "Sign in", data)
		return
	}
	u, err := s.Store.Authenticate(r.Context(), user, r.PostFormValue("password"), now)
	if errors.Is(err, store.ErrBadLogin) {
		s.limiter.fail(now, keys...)
		s.Log.Warn("failed sign-in", "user", user, "ip", s.clientIP(r))
		w.WriteHeader(http.StatusUnauthorized)
		data.Error = "Wrong username or password."
		s.page(w, r, "login", "Sign in", data)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.limiter.reset(keys...)
	if err := s.startSession(w, r, u); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, data.Next)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.Store.DeleteSession(r.Context(), c.Value); err != nil {
			s.Log.Error("cannot delete session", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set from s.secure(r)
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode,
	})
	redirect(w, r, "/login")
}

// safeNext only allows local paths, so the login form cannot redirect off-site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

// ---- setup wizard ----

type setupData struct {
	SSO       ssoForm
	Step      int
	Steps     []string
	Error     string
	Docker    string // engine version, or the error
	DockerOK  bool
	Count     int // containers seen
	Detection detectionForm
	ISPName   string // current ISP
	ISP       int
	Stacks    []stackRow
}

var setupSteps = []string{"Admin account", "Single sign-on", "Docker", "WAN detection", "Primary ISP", "What to stop", "Review"}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := s.Store.CountUsers(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n == 0 {
		s.page(w, r, "setup", "Set up Drawbridge", setupData{Step: 1, Steps: setupSteps})
		return
	}
	// Past step 1 the wizard needs an admin session.
	sess, err := s.loadSession(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if sess == nil {
		redirect(w, r, "/login?next=/setup")
		return
	}
	if !sess.User.IsAdmin() {
		http.Error(w, "admins only", http.StatusForbidden)
		return
	}
	r = r.WithContext(withSession(ctx, sess))

	step := atoi(r.URL.Query().Get("step"), 2)
	step = max(2, min(step, len(setupSteps)))
	data := setupData{Step: step, Steps: setupSteps}
	switch step {
	case 2:
		o, err := s.oidcSettings(ctx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.SSO = s.ssoForm(r, o)
		data.SSO.Wizard = true
	case 3:
		v, err := s.Docker.Ping(ctx)
		data.Docker, data.DockerOK = v, err == nil
		if err != nil {
			data.Docker = err.Error()
		} else if cs, err := s.Docker.List(ctx); err == nil {
			data.Count = len(cs)
			if err := s.Sync.Sync(ctx); err != nil {
				s.Log.Warn("inventory sync failed", "err", err)
			}
		}
	case 4:
		d, err := s.Mon.Detection(ctx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Detection = formFromDetection(d)
	case 5:
		st := s.Mon.Status()
		data.ISP = st.ISP
		if names, err := s.Store.ISPNames(ctx); err == nil {
			data.ISPName = names[st.ISP]
		}
	case 6:
		if data.Stacks, err = s.stackRows(r); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.page(w, r, "setup", "Set up Drawbridge", data)
}

func (s *Server) setupAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := s.Store.CountUsers(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if n > 0 {
		http.Error(w, "an admin already exists", http.StatusForbidden)
		return
	}
	data := setupData{Step: 1, Steps: setupSteps}
	if r.PostFormValue("password") != r.PostFormValue("confirm") {
		data.Error = "The passwords do not match."
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.page(w, r, "setup", "Set up Drawbridge", data)
		return
	}
	u, err := s.Store.CreateUser(ctx, r.PostFormValue("username"), r.PostFormValue("password"), store.RoleAdmin, s.now())
	if err != nil {
		data.Error = err.Error()
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.page(w, r, "setup", "Set up Drawbridge", data)
		return
	}
	s.event(ctx, "user_created", map[string]any{"user": u.Username, "role": u.Role, "by": "setup"})
	if err := s.startSession(w, r, u); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/setup?step=2")
}

func (s *Server) setupDetection(w http.ResponseWriter, r *http.Request) {
	d, err := s.Mon.Detection(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := applyDetectionForm(r, &d); err != nil {
		invalid(w, err.Error())
		return
	}
	if err := s.Mon.ApplyDetection(r.Context(), d); err != nil {
		invalid(w, err.Error())
		return
	}
	redirect(w, r, "/setup?step=5")
}

// setupCheck runs a check now and returns the live result fragment.
func (s *Server) setupCheck(w http.ResponseWriter, r *http.Request) {
	if err := s.Mon.CheckNow(r.Context()); err != nil {
		s.fail(w, r, err)
		return
	}
	names, _ := s.Store.ISPNames(r.Context())
	st := s.Mon.Status()
	s.partial(w, r, "setup", "check-result", map[string]any{"Status": st, "Name": names[st.ISP]})
}

func (s *Server) setupPrimary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.PostFormValue("choice") == "pin" {
		id := atoi(r.PostFormValue("isp"), 0)
		if id == 0 {
			invalid(w, "no ISP has been seen yet; run a check first")
			return
		}
		if err := s.Store.SetRole(ctx, id, store.RolePrimary, true); err != nil {
			invalid(w, err.Error())
			return
		}
		s.event(ctx, "primary_pinned", map[string]any{"isp": id, "by": actorName(r)})
	}
	redirect(w, r, "/setup?step=6")
}

func (s *Server) setupFinish(w http.ResponseWriter, r *http.Request) {
	if err := s.Mon.CompleteSetup(r.Context(), actorName(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/?flash=Setup+complete.+Automation+is+on.")
}

// ---- detection settings form, shared by the wizard and Settings ----

type detectionForm struct {
	CheckInterval, Timeout, LearningPeriod, Retention string
	FailoverAfter, FailbackAfter, FailbackMaxHold     string
	Quorum                                            int
	IPProviders                                       string
	Limits                                            any
}

func formFromDetection(d config.Detection) detectionForm {
	return detectionForm{
		CheckInterval: shortDuration(d.CheckInterval), Timeout: shortDuration(d.Timeout),
		LearningPeriod: shortDuration(d.LearningPeriod), Retention: shortDuration(d.Retention),
		FailoverAfter: shortDuration(d.FailoverAfter), FailbackAfter: shortDuration(d.FailbackAfter),
		FailbackMaxHold: shortDuration(d.FailbackMaxHold), Quorum: d.Quorum,
		IPProviders: strings.Join(d.IPProviders, "\n"),
		Limits:      config.Limits,
	}
}

func applyDetectionForm(r *http.Request, d *config.Detection) error {
	var errs []error
	dur := func(field string, dst *timeDuration) {
		v, err := parseDuration(r.PostFormValue(field))
		if err != nil {
			errs = append(errs, err)
			return
		}
		*dst = v
	}
	num := func(field string, dst *int) {
		v := atoi(r.PostFormValue(field), -1)
		if v < 0 {
			errs = append(errs, errors.New(field+" must be a number"))
			return
		}
		*dst = v
	}
	dur("check_interval", &d.CheckInterval)
	dur("timeout", &d.Timeout)
	dur("learning_period", &d.LearningPeriod)
	dur("retention", &d.Retention)
	dur("failover_after", &d.FailoverAfter)
	dur("failback_after", &d.FailbackAfter)
	dur("failback_max_hold", &d.FailbackMaxHold)
	num("quorum", &d.Quorum)
	d.IPProviders = config.SplitList(r.PostFormValue("ip_providers"))
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return d.Validate()
}
