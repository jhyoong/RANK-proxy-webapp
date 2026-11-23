// Package web serves the portal UI: login form, cluster list, per-cluster
// kagent check with a chat panel and conversation history, and logout. The
// scripts are /static/markdown.js and /static/chat.js, which streams one
// kagent reply at a time.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/history"
	"github.com/jhyoong/RANK-proxy-webapp/internal/ratelimit"
	"github.com/jhyoong/RANK-proxy-webapp/internal/session"
)

const (
	cookieName  = "portal_session"
	maxFormSize = 4 << 10
	csp         = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
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

// History is what the web layer needs from history.Service.
type History interface {
	List(ctx context.Context, sites []auth.SiteResult, row clusters.Row) ([]history.Conversation, error)
	Transcript(ctx context.Context, sites []auth.SiteResult, row clusters.Row, id string) (history.Transcript, error)
}

type server struct {
	cfg     *config.Config
	authn   Authn
	cl      Clusters
	ch      Chat
	hist    History
	now     func() time.Time
	store   *session.Store
	version string
	log     *slog.Logger
	pages   map[string]*template.Template
	userLim *ratelimit.Limiter
	ipLim   *ratelimit.Limiter
	trusted []netip.Prefix

	mu     sync.Mutex
	active map[string]bool // session IDs with a running chat reply
}

// New returns the portal's HTTP handler.
func New(cfg *config.Config, a Authn, c Clusters, ch Chat, h History, store *session.Store, version string, log *slog.Logger) (http.Handler, error) {
	_, hd, err := build(cfg, a, c, ch, h, store, version, log)
	return hd, err
}

func build(cfg *config.Config, a Authn, c Clusters, ch Chat, h History, store *session.Store, version string, log *slog.Logger) (*server, http.Handler, error) {
	s := &server{cfg: cfg, authn: a, cl: c, ch: ch, hist: h, now: time.Now, store: store, version: version, log: log,
		pages: map[string]*template.Template{}, active: map[string]bool{}}
	for _, p := range cfg.Login.TrustedProxies {
		pf, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, nil, err
		}
		s.trusted = append(s.trusted, pf)
	}
	s.userLim = ratelimit.New(cfg.Login.MaxAttemptsPerUser, cfg.Login.AttemptWindow, s.now)
	s.ipLim = ratelimit.New(cfg.Login.MaxAttemptsPerIP, cfg.Login.AttemptWindow, s.now)
	funcs := template.FuncMap{"principal": principalOf, "esc": url.PathEscape, "rfc3339": rfc3339}
	for _, p := range []string{"login", "home", "cluster", "loggedout"} {
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
	mux.HandleFunc("POST /clusters/refresh", s.refresh)
	mux.HandleFunc("GET /clusters/{site}/{id}", s.cluster)
	mux.HandleFunc("POST /clusters/{site}/{id}/chat", s.chat)
	mux.HandleFunc("GET /clusters/{site}/{id}/conversations/{cid}", s.transcript)
	mux.HandleFunc("POST /logout", s.logout)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(version + "\n")) })
	// Defence in depth on top of the per-form CSRF tokens: reject unsafe
	// cross-origin browser requests. Requests with no Sec-Fetch-Site or Origin
	// header (curl, tests) pass; safe methods always pass.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("cross-origin request blocked", "event", "cross_origin_blocked",
			"method", r.Method, "path", r.URL.Path, "ip", clientIP(r, s.trusted).String())
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
	}))
	return s, securityHeaders(cop.Handler(mux), cfg.SecureCookies()), nil
}

// bodyReadTimeout bounds how long a client may take to send a request body.
// It is a var so tests can shorten it.
var bodyReadTimeout = 10 * time.Second

// logoutTimeout bounds token revocation on logout.
const logoutTimeout = 30 * time.Second

// setBodyDeadline sets a read deadline of bodyReadTimeout from now on the
// connection, so a slow client cannot hold the handler while sending a body.
// Socket deadlines use the wall clock, not s.now. Writers that don't support
// deadlines (httptest.ResponseRecorder) are ignored.
func (s *server) setBodyDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
}

func securityHeaders(next http.Handler, secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// rfc3339 formats t for a data-updated attribute; the zero time is "".
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
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
	UI       config.UIConfig
	Help     [][]Token // UI.LoginHelp split into paragraphs and links
	Expired  bool      // the login page shows the session-expired notice
	Error    string
	Username string
	Session  *session.Session
	Logout   []auth.LogoutResult
	Listing  *clusters.Listing
	Rows     []clusters.Row
	Query    string
	// Cluster list: filter chips (logged-in sites in config order; empty
	// unless several sites are configured), the active chip, banner texts,
	// the shown row count and "N min ago" for the list's age.
	Sites       []string
	Site        string
	SiteNotices []string
	Count       int
	UpdatedAgo  string
	Row         clusters.Row
	Check       clusters.CheckResult
	// Cluster page: the sidebar's conversations, why they are missing
	// ("unavailable" or "failed"), the open conversation (?c=), the chat's
	// A2A context (Current, or a fresh UUID), the textarea cap and the
	// reply cap in milliseconds.
	Conversations []history.Conversation
	HistoryError  string
	Current       string
	ContextID     string
	MaxMessage    int
	MaxDurationMS int64
}

func (s *server) render(w http.ResponseWriter, code int, page string, d pageData) {
	d.Version = s.version
	d.UI = s.cfg.UI
	d.Help = helpParagraphs(s.cfg.UI.LoginHelp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := s.pages[page].ExecuteTemplate(w, page+".html", d); err != nil {
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
		HttpOnly: true, Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteStrictMode,
	})
}

// loginErrors maps the ?e= code set after a failed login to its message.
// Unknown codes are ignored.
var loginErrors = map[string]string{
	"failed":      "Login failed",
	"unavailable": "Rancher is unavailable, try again later",
	"badresponse": "Unexpected response from Rancher; see the portal logs",
	"limited":     "Too many login attempts; try again later",
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(r); ok {
		http.Redirect(w, r, "/home", http.StatusSeeOther)
		return
	}
	q := r.URL.Query()
	s.render(w, http.StatusOK, "login", pageData{Error: loginErrors[q.Get("e")], Expired: q.Get("expired") == "1"})
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

// anyTimeout reports whether any site's login timed out.
func anyTimeout(res auth.Result) bool {
	for _, st := range res.Sites {
		if st.Timeout {
			return true
		}
	}
	return false
}

// ipLimitKey is the per-IP limiter key: the address for IPv4, the /64 for
// IPv6 (one host usually holds a whole /64, so per-address buckets are free
// to rotate through).
func ipLimitKey(ip netip.Addr) string {
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	s.setBodyDeadline(w)
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
	// Count the attempt before calling Rancher so parallel requests cannot
	// exceed the limit. The IP is checked first so a blocked IP cannot burn
	// a victim's per-user attempts.
	ip := clientIP(r, s.trusted)
	ipKey := ipLimitKey(ip)
	key := session.UserKey(username)
	if !s.ipLim.Reserve(ipKey) {
		s.log.Warn("login rate limited", "event", "login_rate_limited", "username", key, "ip", ip.String(), "scope", "ip")
		http.Redirect(w, r, "/?e=limited", http.StatusSeeOther)
		return
	}
	if !s.userLim.Reserve(key) {
		s.log.Warn("login rate limited", "event", "login_rate_limited", "username", key, "ip", ip.String(), "scope", "user")
		http.Redirect(w, r, "/?e=limited", http.StatusSeeOther)
		return
	}
	// A client that hangs up must not cancel the Rancher call: AD may already
	// have counted the bind, so the attempt has to stay counted. The per-site
	// login.timeout still bounds the call.
	res := s.authn.Login(context.WithoutCancel(r.Context()), username, password)
	if res.OK {
		// A success proves the caller is not guessing.
		s.userLim.Reset(key)
		s.ipLim.Release(ipKey)
	} else {
		code := failureCode(res)
		// Give the attempt back only when Rancher was plainly unreachable: not
		// after a timeout (the bind may have reached AD) and not when the
		// client went away mid-call.
		if code == "unavailable" && !anyTimeout(res) && r.Context().Err() == nil {
			s.userLim.Release(key)
		}
		// POST-redirect-GET: a browser refresh must not re-send the password
		// (each resubmit would be another AD attempt).
		http.Redirect(w, r, "/?e="+code, http.StatusSeeOther)
		return
	}
	if old, ok := s.current(r); ok {
		s.store.End(old.ID, "relogin")
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

// siteNotices explains, in end-user words, why a site's clusters are missing.
func siteNotices(sites []auth.SiteResult, l *clusters.Listing) []string {
	var out []string
	for _, st := range sites {
		switch st.Status {
		case auth.StatusUnavailable, auth.StatusBadResponse:
			out = append(out, st.Site+" couldn't be reached when you signed in. Its clusters aren't shown. Sign in again to retry.")
		case auth.StatusRejected:
			out = append(out, st.Site+" didn't accept your sign-in. Its clusters aren't shown.")
		}
	}
	if l != nil {
		for _, sl := range l.Sites {
			if sl.Status == "error" {
				out = append(out, "Couldn't load clusters from "+sl.Site+". Try Refresh.")
			}
		}
	}
	return out
}

// ago renders an age as "just now", "N min ago" or "N h ago".
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	default:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	l := s.listing(r, sess)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// Chips list the sites the user is logged in to; with one site there is
	// nothing to filter.
	var chips []string
	if len(s.cfg.Sites) > 1 {
		for _, cs := range s.cfg.Sites {
			for _, st := range sess.Sites {
				if st.Site == cs.Name && st.Status == auth.StatusOK {
					chips = append(chips, cs.Name)
				}
			}
		}
	}
	site := r.URL.Query().Get("site")
	if !slices.Contains(chips, site) {
		site = ""
	}
	rows := l.Filter(q)
	if site != "" {
		var in []clusters.Row
		for _, row := range rows {
			if row.Site == site {
				in = append(in, row)
			}
		}
		rows = in
	}
	s.render(w, http.StatusOK, "home", pageData{Session: sess, Listing: l, Rows: rows, Query: q, Sites: chips, Site: site,
		SiteNotices: siteNotices(sess.Sites, l), Count: len(rows), UpdatedAgo: ago(s.now().Sub(l.Fetched))})
}

func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.setBodyDeadline(w)
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
	// A bad or repeated ?c= is dropped rather than shown as an error.
	cs, hasC := r.URL.Query()["c"]
	if hasC && (len(cs) != 1 || !a2a.ValidUUID(cs[0])) {
		http.Redirect(w, r, "/clusters/"+url.PathEscape(r.PathValue("site"))+"/"+url.PathEscape(r.PathValue("id")), http.StatusSeeOther)
		return
	}
	row, found := s.listing(r, sess).Find(r.PathValue("site"), r.PathValue("id"))
	if !found {
		http.Error(w, "cluster not found", http.StatusNotFound)
		return
	}
	res := s.cl.Check(r.Context(), sess.Sites, row, sess.Principal)
	if res.Outcome == clusters.OutcomeExpired {
		s.expireRedirect(w, r, sess)
		return
	}
	d := pageData{Session: sess, Row: row, Check: res}
	if res.Outcome != clusters.OutcomeReady {
		s.render(w, http.StatusOK, "cluster", d)
		return
	}
	convs, err := s.hist.List(r.Context(), sess.Sites, row)
	switch {
	case errors.Is(err, history.ErrExpired):
		s.expireRedirect(w, r, sess)
		return
	case errors.Is(err, history.ErrForbidden):
		d.HistoryError = "unavailable"
	case err != nil:
		s.log.Warn("conversation list failed", "event", "history_failed", "site", row.Site, "cluster", row.ID, "err", err)
		d.HistoryError = "failed"
	}
	d.Conversations = convs
	if hasC {
		d.Current = cs[0]
	}
	d.ContextID = d.Current
	if d.ContextID == "" {
		d.ContextID = a2a.NewUUID()
	}
	d.MaxMessage = s.cfg.Chat.MaxMessageBytes
	d.MaxDurationMS = s.cfg.Chat.MaxDuration.Milliseconds()
	s.render(w, http.StatusOK, "cluster", d)
}

// expire ends sess like /logout: the session is removed at once and its
// Rancher tokens are revoked in the background.
func (s *server) expire(sess *session.Session) {
	if _, ok := s.store.Delete(sess.ID); !ok {
		return // already ended by a concurrent request
	}
	s.log.Info("session expired", "event", "session_expired", "username", sess.Username, "principal", sess.Principal)
	// Not tied to any request: a closed tab must not orphan the tokens.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), logoutTimeout)
		defer cancel()
		s.authn.Logout(ctx, sess.Sites)
	}()
}

// expireRedirect ends sess, clears the cookie and sends the browser to the
// login page with the session-expired notice.
func (s *server) expireRedirect(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	s.expire(sess)
	s.setCookie(w, "", -1)
	http.Redirect(w, r, "/?expired=1", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.setBodyDeadline(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	_ = r.ParseForm()
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.CSRF)) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	s.store.Delete(sess.ID)
	// The session is already gone, so a closed tab must not cancel the
	// revocation and orphan the tokens.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), logoutTimeout)
	defer cancel()
	results := s.authn.Logout(ctx, sess.Sites)
	s.setCookie(w, "", -1)
	s.render(w, http.StatusOK, "loggedout", pageData{Session: sess, Logout: results})
}
