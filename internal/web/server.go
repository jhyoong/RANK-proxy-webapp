// Package web serves the portal UI: login form, per-site diagnostics page
// and logout. Plain HTML forms, no JavaScript.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/session"
)

const (
	cookieName  = "portal_session"
	maxFormSize = 4 << 10
	csp         = "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Authn is what the web layer needs from auth.Authenticator.
type Authn interface {
	Login(ctx context.Context, username, password string) auth.Result
	Logout(ctx context.Context, sites []auth.SiteResult) []auth.LogoutResult
}

// Clusters is what the web layer needs from clusters.Service.
type Clusters interface {
	List(ctx context.Context, sites []auth.SiteResult) *clusters.Listing
	Check(ctx context.Context, sites []auth.SiteResult, row clusters.Row, principal string) clusters.CheckResult
}

type server struct {
	cfg     *config.Config
	authn   Authn
	cl      Clusters
	now     func() time.Time
	store   *session.Store
	version string
	log     *slog.Logger
	pages   map[string]*template.Template
}

// New returns the portal's HTTP handler.
func New(cfg *config.Config, a Authn, c Clusters, store *session.Store, version string, log *slog.Logger) (http.Handler, error) {
	_, h, err := build(cfg, a, c, store, version, log)
	return h, err
}

func build(cfg *config.Config, a Authn, c Clusters, store *session.Store, version string, log *slog.Logger) (*server, http.Handler, error) {
	s := &server{cfg: cfg, authn: a, cl: c, now: time.Now, store: store, version: version, log: log, pages: map[string]*template.Template{}}
	funcs := template.FuncMap{"principal": principalOf, "esc": url.PathEscape, "deref": func(b *bool) bool { return *b }}
	for _, p := range []string{"login", "home", "diagnostics", "cluster", "loggedout"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, nil, err
		}
		s.pages[p] = t
	}
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /home", s.home)
	mux.HandleFunc("GET /diagnostics", s.diagnostics)
	mux.HandleFunc("POST /clusters/refresh", s.refresh)
	mux.HandleFunc("GET /clusters/{site}/{id}", s.cluster)
	mux.HandleFunc("POST /logout", s.logout)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(version + "\n")) })
	return s, securityHeaders(mux), nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func principalOf(ids []string) string {
	for _, id := range ids {
		if strings.HasPrefix(id, "activedirectory_user://") {
			return id
		}
	}
	return ""
}

type pageData struct {
	Version  string
	Error    string
	Username string
	Session  *session.Session
	Logout   []auth.LogoutResult
	Listing  *clusters.Listing
	Rows     []clusters.Row
	Query    string
	Row      clusters.Row
	Check    clusters.CheckResult
}

func (s *server) render(w http.ResponseWriter, code int, page string, d pageData) {
	d.Version = s.version
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := s.pages[page].Execute(w, d); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
	}
}

func (s *server) current(r *http.Request) (*session.Session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil, false
	}
	return s.store.Get(c.Value)
}

func (s *server) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// loginErrors maps the ?e= code set after a failed login to its message.
// Unknown codes are ignored.
var loginErrors = map[string]string{
	"failed":      "Login failed",
	"unavailable": "Rancher is unavailable, try again later",
	"badresponse": "Unexpected response from Rancher; see the portal logs",
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(r); ok {
		http.Redirect(w, r, "/home", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", pageData{Error: loginErrors[r.URL.Query().Get("e")]})
}

// failureCode picks the ?e= code for a failed login.
func failureCode(res auth.Result) string {
	allDown := len(res.Sites) > 0
	for _, st := range res.Sites {
		if st.Status == auth.StatusBadResponse {
			return "badresponse"
		}
		if st.Status != auth.StatusUnavailable {
			allDown = false
		}
	}
	if allDown {
		return "unavailable"
	}
	return "failed"
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	if err := r.ParseForm(); err != nil {
		s.render(w, http.StatusBadRequest, "login", pageData{Error: "Request too large"})
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	if username == "" || password == "" {
		s.render(w, http.StatusBadRequest, "login", pageData{Error: "Enter username and password", Username: username})
		return
	}
	res := s.authn.Login(r.Context(), username, password)
	if !res.OK {
		// POST-redirect-GET: a browser refresh must not re-send the password
		// (each resubmit would be another AD attempt).
		http.Redirect(w, r, "/?e="+failureCode(res), http.StatusSeeOther)
		return
	}
	sess := s.store.Create(session.Session{Username: username, Principal: res.PrincipalID, Sites: res.Sites, Warnings: res.Warnings})
	s.setCookie(w, sess.ID, 0)
	http.Redirect(w, r, "/home", http.StatusSeeOther)
}

// listing returns the session's cached cluster list, refetching it when
// missing or older than clusters.cache_ttl.
func (s *server) listing(r *http.Request, sess *session.Session) *clusters.Listing {
	if l := sess.Clusters; l != nil && s.now().Sub(l.Fetched) < s.cfg.Clusters.CacheTTL {
		return l
	}
	l := s.cl.List(r.Context(), sess.Sites)
	// A cancelled request's result may be partial; use it for this response only.
	if r.Context().Err() == nil {
		s.store.SetClusters(sess.ID, l)
	}
	return l
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	l := s.listing(r, sess)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	s.render(w, http.StatusOK, "home", pageData{Session: sess, Listing: l, Rows: l.Filter(q), Query: q})
}

func (s *server) diagnostics(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "diagnostics", pageData{Session: sess, Listing: s.listing(r, sess)})
}

func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	_ = r.ParseForm()
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.CSRF)) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	s.store.SetClusters(sess.ID, nil)
	http.Redirect(w, r, "/home", http.StatusSeeOther)
}

func (s *server) cluster(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	row, found := s.listing(r, sess).Find(r.PathValue("site"), r.PathValue("id"))
	if !found {
		http.Error(w, "cluster not found", http.StatusNotFound)
		return
	}
	res := s.cl.Check(r.Context(), sess.Sites, row, sess.Principal)
	s.render(w, http.StatusOK, "cluster", pageData{Session: sess, Row: row, Check: res})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	_ = r.ParseForm()
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.CSRF)) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	s.store.Delete(sess.ID)
	results := s.authn.Logout(r.Context(), sess.Sites)
	s.setCookie(w, "", -1)
	s.render(w, http.StatusOK, "loggedout", pageData{Session: sess, Logout: results})
}
