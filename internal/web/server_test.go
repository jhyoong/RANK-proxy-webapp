package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/chat"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/history"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/ratelimit"
	"github.com/jhyoong/RANK-proxy-webapp/internal/session"
)

const principal = "activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com"

const wantCSP = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

type stubAuthn struct {
	result      auth.Result
	loginCalls  int
	mu          sync.Mutex
	delay       time.Duration
	logoutSites []auth.SiteResult
	logoutCalls int
	logoutAll   [][]auth.SiteResult
	// onLogin, if set, runs inside Login with its context.
	onLogin func(ctx context.Context)
	// State of the last Logout call's context, taken when it was called.
	logoutCtxErr   error
	logoutDeadline time.Time
	logoutHasDL    bool
	// failRevoke makes Logout report the first site's token as not revoked.
	failRevoke bool
}

func (s *stubAuthn) Login(ctx context.Context, _, _ string) auth.Result {
	s.mu.Lock()
	s.loginCalls++
	res, d, hook := s.result, s.delay, s.onLogin
	s.mu.Unlock()
	time.Sleep(d)
	if hook != nil {
		hook(ctx)
	}
	return res
}

func (s *stubAuthn) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginCalls
}
func (s *stubAuthn) Logout(ctx context.Context, sites []auth.SiteResult) []auth.LogoutResult {
	s.mu.Lock()
	s.logoutCalls++
	s.logoutCtxErr = ctx.Err()
	s.logoutDeadline, s.logoutHasDL = ctx.Deadline()
	s.logoutSites = sites
	s.logoutAll = append(s.logoutAll, sites)
	s.mu.Unlock()
	var out []auth.LogoutResult
	for i, st := range sites {
		out = append(out, auth.LogoutResult{Site: st.Site, LoggedOut: true, Revoked: !(s.failRevoke && i == 0)})
	}
	return out
}

func okResult() auth.Result {
	u := &rancher.User{ID: "u-abc", Username: "jdoe", PrincipalIDs: []string{"local://u-abc", principal}}
	site := func(n string) auth.SiteResult {
		return auth.SiteResult{Site: n, Status: auth.StatusOK, HTTPCode: 200, User: u, Latency: 40 * time.Millisecond,
			Token: rancher.Token{Bearer: "token-abc12:SECRETVALUE", ID: "token-abc12", UserID: "u-abc", ExpiresAt: "2026-11-20T17:00:00Z"}}
	}
	return auth.Result{OK: true, PrincipalID: principal, Sites: []auth.SiteResult{site("site-1"), site("site-2"),
		{Site: "site-3", Status: auth.StatusUnavailable, Error: "connection refused"}}}
}

type stubClusters struct {
	listing    *clusters.Listing
	check      clusters.CheckResult
	listCalls  int
	checkCalls int
	now        func() time.Time
}

func (s *stubClusters) List(_ context.Context, _ []auth.SiteResult) *clusters.Listing {
	s.listCalls++
	l := *s.listing
	l.Fetched = s.now()
	return &l
}

func (s *stubClusters) Check(_ context.Context, _ []auth.SiteResult, _ clusters.Row, _ string) clusters.CheckResult {
	s.checkCalls++
	return s.check
}

func defaultListing() *clusters.Listing {
	return &clusters.Listing{
		Sites: []clusters.SiteList{
			{Site: "site-1", Status: "ok", HTTPCode: 200, Count: 2, Latency: 30 * time.Millisecond},
			{Site: "site-2", Status: "ok", HTTPCode: 200, Count: 1, Latency: 20 * time.Millisecond},
			{Site: "site-3", Status: "skipped"},
		},
		Rows: []clusters.Row{
			{Site: "site-1", Cluster: rancher.Cluster{ID: "c-abc12", DisplayName: "prod-east", State: "active", Connected: true,
				Conditions: []rancher.Condition{{Type: "Ready", Status: "True", Reason: "AllGood"}, {Type: "Connected", Status: "True"}}}},
			{Site: "site-1", Cluster: rancher.Cluster{ID: "c-down1", DisplayName: "dev-west", State: "provisioning", Connected: false}},
			{Site: "site-2", Cluster: rancher.Cluster{ID: "c-xyz99", DisplayName: "prod-south", State: "active", Connected: true}},
		},
	}
}

// stubHistory returns the settable results and records the calls made.
type stubHistory struct {
	mu      sync.Mutex
	convs   []history.Conversation
	listErr error
	tr      history.Transcript
	trErr   error
	calls   []string // "list" or "transcript:<id>"
}

func (s *stubHistory) List(_ context.Context, _ []auth.SiteResult, _ clusters.Row) ([]history.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "list")
	return s.convs, s.listErr
}

func (s *stubHistory) Transcript(_ context.Context, _ []auth.SiteResult, _ clusters.Row, id string) (history.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "transcript:"+id)
	return s.tr, s.trErr
}

func (s *stubHistory) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type harness struct {
	h     http.Handler
	srv   *server
	stub  *stubAuthn
	store *session.Store
	cl    *stubClusters
	ch    *stubChat
	hist  *stubHistory
	clock *time.Time
}

func newHarness(t *testing.T, secure bool) harness {
	t.Helper()
	cfg := &config.Config{CookieSecure: &secure, Sites: []config.Site{{Name: "site-1", URL: "https://r1.example"}, {Name: "site-2", URL: "https://r2.example"}, {Name: "site-3"}},
		Login:    config.LoginConfig{MaxAttemptsPerUser: 3, MaxAttemptsPerIP: 20, AttemptWindow: 15 * time.Minute},
		Clusters: config.ClustersConfig{CacheTTL: 2 * time.Minute}, Chat: config.ChatConfig{MaxMessageBytes: 8192, MaxDuration: 30 * time.Minute}}
	stub := &stubAuthn{result: okResult()}
	clock := time.Now()
	hs := &harness{}
	cl := &stubClusters{listing: defaultListing(), now: func() time.Time { return *hs.clock }}
	store := session.NewStore(30*time.Minute, 8*time.Hour, 0, time.Now)
	ch := &stubChat{started: make(chan struct{}, 8), ctxDone: make(chan struct{}, 8), slowErr: make(chan error, 1)}
	hist := &stubHistory{}
	srv, h, err := build(cfg, stub, cl, ch, hist, store, "v-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return *hs.clock }
	hs.h, hs.srv, hs.stub, hs.store, hs.cl, hs.ch, hs.hist, hs.clock = h, srv, stub, store, cl, ch, hist, &clock
	return *hs
}

func (hs harness) do(method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	hs.h.ServeHTTP(rr, req)
	return rr
}

func (hs harness) login(t *testing.T) *http.Cookie {
	t.Helper()
	rr := hs.do("POST", "/login", url.Values{"username": {"jdoe"}, "password": {"pw"}}, nil)
	for _, c := range rr.Result().Cookies() {
		if c.Name == "portal_session" {
			return c
		}
	}
	t.Fatalf("no session cookie, status %d", rr.Code)
	return nil
}

func TestLogin_SuccessSetsCookie(t *testing.T) {
	hs := newHarness(t, true)
	rr := hs.do("POST", "/login", url.Values{"username": {"jdoe"}, "password": {"pw"}}, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/home" {
		t.Fatalf("status %d location %q", rr.Code, rr.Header().Get("Location"))
	}
	sc := rr.Header().Get("Set-Cookie")
	for _, want := range []string{"portal_session=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Fatalf("Set-Cookie %q missing %q", sc, want)
		}
	}
	if strings.Contains(sc, "Max-Age") || strings.Contains(sc, "Expires") {
		t.Fatalf("cookie must be session-only: %q", sc)
	}
}

func (hs harness) loginFailsWith(t *testing.T, code, text string) {
	t.Helper()
	rr := hs.do("POST", "/login", url.Values{"username": {"jdoe"}, "password": {"pw"}}, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/?e="+code || rr.Header().Get("Set-Cookie") != "" {
		t.Fatalf("status %d location %q cookie %q", rr.Code, rr.Header().Get("Location"), rr.Header().Get("Set-Cookie"))
	}
	page := hs.do("GET", "/?e="+code, nil, nil)
	if !strings.Contains(page.Body.String(), text) {
		t.Fatalf("page for %s missing %q", code, text)
	}
}

// A failed login redirects (POST-redirect-GET) so a browser refresh cannot
// re-send the password and cost another AD attempt.
func TestLogin_Failed(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = auth.Result{Sites: []auth.SiteResult{{Site: "site-1", Status: auth.StatusRejected, HTTPCode: 401},
		{Site: "site-2", Status: auth.StatusSkipped}, {Site: "site-3", Status: auth.StatusSkipped}}}
	hs.loginFailsWith(t, "failed", "Login failed")
}

func TestLogin_AllUnavailable(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = auth.Result{Sites: []auth.SiteResult{{Site: "site-1", Status: auth.StatusUnavailable},
		{Site: "site-2", Status: auth.StatusUnavailable}, {Site: "site-3", Status: auth.StatusUnavailable}}}
	hs.loginFailsWith(t, "unavailable", "Rancher is unavailable")
}

func TestLogin_BadResponse(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = auth.Result{Sites: []auth.SiteResult{{Site: "site-1", Status: auth.StatusBadResponse},
		{Site: "site-2", Status: auth.StatusSkipped}, {Site: "site-3", Status: auth.StatusSkipped}}}
	hs.loginFailsWith(t, "badresponse", "Unexpected response from Rancher")
}

func TestIndex_UnknownErrorCodeIgnored(t *testing.T) {
	hs := newHarness(t, true)
	body := hs.do("GET", "/?e=%3Cscript%3E", nil, nil).Body.String()
	if strings.Contains(body, "script") || strings.Contains(body, `class="error"`) {
		t.Fatal("unknown error code rendered")
	}
}

func TestLogin_EmptyFieldsNoCall(t *testing.T) {
	hs := newHarness(t, true)
	for _, f := range []url.Values{{"username": {"jdoe"}}, {"password": {"pw"}}, {"username": {"  "}, "password": {"pw"}}} {
		rr := hs.do("POST", "/login", f, nil)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "Enter username and password") {
			t.Fatalf("status %d", rr.Code)
		}
	}
	if hs.stub.loginCalls != 0 {
		t.Fatalf("login called %d times", hs.stub.loginCalls)
	}
}

func TestLogin_OversizedForm(t *testing.T) {
	hs := newHarness(t, true)
	rr := hs.do("POST", "/login", url.Values{"username": {"jdoe"}, "password": {strings.Repeat("x", 5000)}}, nil)
	if rr.Code != http.StatusBadRequest || hs.stub.loginCalls != 0 {
		t.Fatalf("status %d calls %d", rr.Code, hs.stub.loginCalls)
	}
}

func TestIndex(t *testing.T) {
	hs := newHarness(t, true)
	rr := hs.do("GET", "/", nil, nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `name="password"`) {
		t.Fatalf("status %d", rr.Code)
	}
	c := hs.login(t)
	rr = hs.do("GET", "/", nil, c)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/home" {
		t.Fatalf("logged-in index: %d", rr.Code)
	}
}

func TestHome_NoWarningsShown(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result.Warnings = []string{"principal ID differs between sites"}
	c := hs.login(t)
	body := hs.do("GET", "/home", nil, c).Body.String()
	if strings.Contains(body, "principal ID differs") {
		t.Fatal("session warning rendered")
	}
}

func TestHome_Columns(t *testing.T) {
	hs := newHarness(t, true)
	body := hs.do("GET", "/home", nil, hs.login(t)).Body.String()
	if !strings.Contains(body, "prod-east") {
		t.Fatal("display name missing")
	}
	for _, id := range []string{"c-abc12", "c-down1", "c-xyz99"} {
		if strings.Contains(body, ">"+id+"<") || strings.Contains(body, "<code>") {
			t.Fatalf("ID %s shown", id)
		}
	}
}

func TestHome_RancherLinks(t *testing.T) {
	hs := newHarness(t, true)
	body := hs.do("GET", "/home", nil, hs.login(t)).Body.String()
	for _, href := range []string{
		"https://r1.example/dashboard/c/c-abc12/explorer",
		"https://r1.example/dashboard/c/c-down1/explorer",
		"https://r2.example/dashboard/c/c-xyz99/explorer",
	} {
		if !strings.Contains(body, `href="`+href+`" target="_blank" rel="noopener noreferrer"`) {
			t.Fatalf("Rancher link %s missing", href)
		}
	}
}

func TestHome_SiteFilter(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	body := hs.do("GET", "/home?site=site-2", nil, c).Body.String()
	if !strings.Contains(body, "prod-south") || strings.Contains(body, "prod-east") || strings.Contains(body, "dev-west") {
		t.Fatal("site filter wrong")
	}
	body = hs.do("GET", "/home?site=bogus", nil, c).Body.String()
	for _, n := range []string{"prod-south", "prod-east", "dev-west"} {
		if !strings.Contains(body, n) {
			t.Fatalf("unknown site must be ignored; missing %s", n)
		}
	}
}

func TestHome_ChipsHiddenSingleSite(t *testing.T) {
	hs := newHarness(t, true)
	hs.srv.cfg.Sites = hs.srv.cfg.Sites[:1]
	hs.stub.result.Sites = hs.stub.result.Sites[:1]
	body := hs.do("GET", "/home", nil, hs.login(t)).Body.String()
	if strings.Contains(body, `class="chip"`) || strings.Contains(body, `class="chip `) {
		t.Fatal("chips shown with a single site")
	}
}

func TestHome_ChipsFourSites(t *testing.T) {
	hs := newHarness(t, true)
	hs.srv.cfg.Sites = []config.Site{{Name: "site-1"}, {Name: "site-2"}, {Name: "site-3"}, {Name: "site-4"}}
	r := okResult()
	r.Sites = append(r.Sites[:2], auth.SiteResult{Site: "site-3", Status: auth.StatusOK}, auth.SiteResult{Site: "site-4", Status: auth.StatusOK})
	hs.stub.result = r
	c := hs.login(t)
	body := hs.do("GET", "/home?site=site-2&q=a+b", nil, c).Body.String()
	if n := strings.Count(body, `class="chip"`); n != 5 {
		t.Fatalf("chips %d, want 5", n)
	}
	if strings.Count(body, `aria-current="true"`) != 1 || !strings.Contains(body, `href="/home?site=site-3&amp;q=a%20b"`) {
		t.Fatalf("active chip or href wrong: %s", body)
	}
}

func TestHome_SiteNotices(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result.Sites[1] = auth.SiteResult{Site: "site-2", Status: auth.StatusUnavailable}
	body := hs.do("GET", "/home", nil, hs.login(t)).Body.String()
	if !strings.Contains(body, "site-2 couldn&#39;t be reached when you signed in. Its clusters aren&#39;t shown. Sign in again to retry.") {
		t.Fatal("unavailable notice missing")
	}
	hs = newHarness(t, true)
	hs.stub.result.Sites[1] = auth.SiteResult{Site: "site-2", Status: auth.StatusRejected}
	body = hs.do("GET", "/home", nil, hs.login(t)).Body.String()
	if !strings.Contains(body, "site-2 didn&#39;t accept your sign-in.") {
		t.Fatal("rejected notice missing")
	}
}

func TestHome_EmptyQuery(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	if body := hs.do("GET", "/home?q=zzz", nil, c).Body.String(); !strings.Contains(body, `No clusters match &#34;zzz&#34;`) {
		t.Fatalf("empty-query text missing")
	}
	hs = newHarness(t, true)
	hs.cl.listing.Rows = nil
	c = hs.login(t)
	if body := hs.do("GET", "/home", nil, c).Body.String(); !strings.Contains(body, "No clusters available") {
		t.Fatal("empty text missing")
	}
}

func TestHome_UpdatedAgo(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	hs.do("GET", "/home", nil, c)
	*hs.clock = hs.clock.Add(90 * time.Second)
	if body := hs.do("GET", "/home", nil, c).Body.String(); !strings.Contains(body, "Updated 1 min ago") {
		t.Fatal("updated text missing")
	}
}

func TestAgo(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{-time.Second, "just now"}, {30 * time.Second, "just now"}, {time.Minute, "1 min ago"}, {119 * time.Second, "1 min ago"},
		{5 * time.Minute, "5 min ago"}, {59 * time.Minute, "59 min ago"}, {time.Hour, "1 h ago"}, {3*time.Hour + 20*time.Minute, "3 h ago"}} {
		if got := ago(c.d); got != c.want {
			t.Errorf("ago(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestPages_DoNotLeakToken(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady, Message: "kagent is ready"}
	c := hs.login(t)
	for _, p := range []string{"/home", "/clusters/site-1/c-abc12"} {
		if body := hs.do("GET", p, nil, c).Body.String(); strings.Contains(body, "SECRETVALUE") || strings.Contains(body, "token-abc12:") {
			t.Fatalf("%s: bearer secret rendered", p)
		}
	}
}

func TestHome_StaleCookieRedirects(t *testing.T) {
	hs := newHarness(t, true)
	rr := hs.do("GET", "/home", nil, &http.Cookie{Name: "portal_session", Value: "bogus"})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("status %d", rr.Code)
	}
	if rr := hs.do("GET", "/home", nil, nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("no cookie: %d", rr.Code)
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfFrom(t *testing.T, hs harness, c *http.Cookie) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(hs.do("GET", "/home", nil, c).Body.String())
	if m == nil {
		t.Fatal("no csrf field on home")
	}
	return m[1]
}

func TestLogout_RequiresCSRF(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	for _, f := range []url.Values{{"csrf": {"wrong"}}, {}} {
		if rr := hs.do("POST", "/logout", f, c); rr.Code != http.StatusForbidden {
			t.Fatalf("status %d", rr.Code)
		}
	}
	if hs.stub.logoutCalls != 0 {
		t.Fatal("logout called")
	}
	if rr := hs.do("GET", "/home", nil, c); rr.Code != 200 {
		t.Fatal("session should survive a bad CSRF")
	}
}

func TestLogout_Success(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	rr := hs.do("POST", "/logout", url.Values{"csrf": {csrfFrom(t, hs, c)}}, c)
	if rr.Code != 200 || hs.stub.logoutCalls != 1 || len(hs.stub.logoutSites) != 3 {
		t.Fatalf("status %d calls %d", rr.Code, hs.stub.logoutCalls)
	}
	if hs.stub.logoutSites[0].Token.Bearer != "token-abc12:SECRETVALUE" {
		t.Fatal("logout did not get the session's tokens")
	}
	if sc := rr.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") {
		t.Fatalf("cookie not expired: %q", sc)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "You've signed out") && !strings.Contains(body, "You&#39;ve signed out") {
		t.Fatalf("logged-out page missing heading: %s", body)
	}
	if strings.Contains(body, "<table") || strings.Contains(body, "token-abc12") || strings.Contains(body, "couldn") {
		t.Fatalf("logged-out page shows diagnostics: %s", body)
	}
	if rr := hs.do("GET", "/home", nil, c); rr.Code != http.StatusSeeOther {
		t.Fatal("session still valid after logout")
	}
}

func TestLogout_NoSession(t *testing.T) {
	hs := newHarness(t, true)
	if rr := hs.do("POST", "/logout", url.Values{"csrf": {"x"}}, nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	for _, p := range []string{"/", "/home", "/diagnostics", "/healthz", "/version", "/static/style.css", "/static/chat.js", "/nope"} {
		rr := hs.do("GET", p, nil, c)
		h := rr.Header()
		if h.Get("Content-Security-Policy") != wantCSP || h.Get("Cache-Control") != "no-store" ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: headers %v", p, h)
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("Cross-Origin-Opener-Policy") != "same-origin" ||
			h.Get("Strict-Transport-Security") != "max-age=31536000" {
			t.Fatalf("%s: headers %v", p, h)
		}
	}
}

func TestSecurityHeaders_NoHSTSWhenInsecure(t *testing.T) {
	hs := newHarness(t, false)
	rr := hs.do("GET", "/healthz", nil, nil)
	if v := rr.Header().Get("Strict-Transport-Security"); v != "" {
		t.Fatalf("HSTS %q", v)
	}
	if rr.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("X-Frame-Options missing")
	}
}

func TestChat_LongReplyNotCutByReadDeadline(t *testing.T) {
	old := bodyReadTimeout
	bodyReadTimeout = 100 * time.Millisecond
	defer func() { bodyReadTimeout = old }()
	hs := newHarness(t, true)
	c := hs.login(t)
	ts := httptest.NewServer(hs.h)
	defer ts.Close()
	resp := liveChat(t, context.Background(), ts.URL, hs, c, "slow")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "event: done\n") {
		t.Fatalf("reply cut: %q", b)
	}
	if err := <-hs.ch.slowErr; err != nil {
		t.Fatalf("reply context cancelled: %v", err)
	}
}

func TestStaticAndHealth(t *testing.T) {
	hs := newHarness(t, true)
	if rr := hs.do("GET", "/static/style.css", nil, nil); rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("css %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	if rr := hs.do("GET", "/static/chat.js", nil, nil); rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("js %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	if rr := hs.do("GET", "/healthz", nil, nil); rr.Code != 200 {
		t.Fatal("healthz")
	}
	if rr := hs.do("GET", "/version", nil, nil); !strings.Contains(rr.Body.String(), "v-test") {
		t.Fatal("version")
	}
}

func TestStatic_Markdown(t *testing.T) {
	hs := newHarness(t, true)
	rr := hs.do("GET", "/static/markdown.js", nil, nil)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("markdown.js %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(rr.Body.String(), "window.Markdown") {
		t.Fatal("markdown.js body missing API export")
	}
}

func TestCookieSecureFalse(t *testing.T) {
	hs := newHarness(t, false)
	rr := hs.do("POST", "/login", url.Values{"username": {"jdoe"}, "password": {"pw"}}, nil)
	if strings.Contains(rr.Header().Get("Set-Cookie"), "Secure") {
		t.Fatal("Secure set when cookie_secure=false")
	}
}

func TestHome_ListsClusters(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	rr := hs.do("GET", "/home", nil, c)
	body := rr.Body.String()
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	for _, want := range []string{"prod-east", "dev-west", "prod-south", "Active", "Disconnected",
		`href="/clusters/site-1/c-abc12"`, "Refresh"} {
		if !strings.Contains(body, want) {
			t.Fatalf("home missing %q", want)
		}
	}
	if strings.Contains(body, "/clusters/site-1/c-down1") {
		t.Fatal("disconnected row must not link")
	}
	if !strings.Contains(body, `class="disconnected"`) {
		t.Fatal("disconnected class missing")
	}
}

func TestHome_CachesList(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	hs.do("GET", "/home", nil, c)
	hs.do("GET", "/home", nil, c)
	if hs.cl.listCalls != 1 {
		t.Fatalf("list calls %d, want 1", hs.cl.listCalls)
	}
	*hs.clock = hs.clock.Add(3 * time.Minute)
	hs.do("GET", "/home", nil, c)
	if hs.cl.listCalls != 2 {
		t.Fatalf("list calls %d, want 2 after TTL", hs.cl.listCalls)
	}
}

func TestRefresh_RequiresCSRF(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	hs.do("GET", "/home", nil, c)
	for _, f := range []url.Values{{"csrf": {"wrong"}}, {}} {
		if rr := hs.do("POST", "/clusters/refresh", f, c); rr.Code != http.StatusForbidden {
			t.Fatalf("status %d", rr.Code)
		}
	}
	rr := hs.do("POST", "/clusters/refresh", url.Values{"csrf": {csrfFrom(t, hs, c)}}, c)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/home" {
		t.Fatalf("status %d loc %q", rr.Code, rr.Header().Get("Location"))
	}
	before := hs.cl.listCalls
	hs.do("GET", "/home", nil, c)
	if hs.cl.listCalls != before+1 {
		t.Fatalf("list calls %d -> %d, want refetch", before, hs.cl.listCalls)
	}
}

func TestRefresh_NoSession(t *testing.T) {
	hs := newHarness(t, true)
	if rr := hs.do("POST", "/clusters/refresh", url.Values{"csrf": {"x"}}, nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestHome_SiteErrorBanner(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.listing.Sites[1] = clusters.SiteList{Site: "site-2", Status: "error", HTTPCode: 401, Error: "token expired or revoked: log in again"}
	hs.cl.listing.Rows = hs.cl.listing.Rows[:2]
	c := hs.login(t)
	rr := hs.do("GET", "/home", nil, c)
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "Couldn&#39;t load clusters from site-2. Try Refresh.") || !strings.Contains(body, "banner-info") ||
		!strings.Contains(body, "prod-east") {
		t.Fatalf("status %d, banner/rows missing", rr.Code)
	}
}

func TestHome_Filter(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	body := hs.do("GET", "/home?q=prod", nil, c).Body.String()
	if !strings.Contains(body, "prod-east") || !strings.Contains(body, "prod-south") || strings.Contains(body, "dev-west") {
		t.Fatal("filter result wrong")
	}
	if !strings.Contains(body, `value="prod"`) {
		t.Fatal("filter input lost value")
	}
}

func TestHome_EscapesClusterName(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.listing.Rows[0].DisplayName = "<b>x</b>"
	c := hs.login(t)
	body := hs.do("GET", "/home", nil, c).Body.String()
	if strings.Contains(body, "<b>x</b>") || !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;") {
		t.Fatal("display name not escaped")
	}
	if !strings.Contains(body, `href="/clusters/site-1/c-abc12"`) {
		t.Fatal("link must use the ID")
	}
}

func TestCluster_Ready(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady, Message: "kagent is ready",
		Card: &rancher.Card{Name: "k8s-agent", Description: "does things", Version: "1.2.3"}}
	c := hs.login(t)
	rr := hs.do("GET", "/clusters/site-1/c-abc12", nil, c)
	body := rr.Body.String()
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	for _, want := range []string{"prod-east", `id="chat"`, `href="/home"`, "Ask kagent about prod-east"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cluster page missing %q", want)
		}
	}
	for _, bad := range []string{"Diagnostics", "SSAR", "k8s-agent", "kagent is ready"} {
		if strings.Contains(body, bad) {
			t.Fatalf("cluster page shows %q", bad)
		}
	}
	if hs.cl.checkCalls != 1 {
		t.Fatalf("check calls %d", hs.cl.checkCalls)
	}
}

func TestHome_CancelledListNotCached(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/home", nil).WithContext(ctx)
	req.AddCookie(c)
	hs.h.ServeHTTP(httptest.NewRecorder(), req)
	before := hs.cl.listCalls
	if before == 0 {
		t.Fatal("list not called")
	}
	hs.do("GET", "/home", nil, c)
	if hs.cl.listCalls != before+1 {
		t.Fatalf("list calls %d, want %d (cancelled result was cached)", hs.cl.listCalls, before+1)
	}
}

func TestCluster_UnknownIDNoRancherCall(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	for _, p := range []string{"/clusters/site-1/c-nope", "/clusters/site-1/local", "/clusters/nosite/c-abc12"} {
		if rr := hs.do("GET", p, nil, c); rr.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", p, rr.Code)
		}
	}
	if hs.cl.checkCalls != 0 {
		t.Fatalf("check calls %d", hs.cl.checkCalls)
	}
}

func TestCluster_NoSessionRedirects(t *testing.T) {
	hs := newHarness(t, true)
	rr := hs.do("GET", "/clusters/site-1/c-abc12", nil, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestCluster_StaleCacheRelists(t *testing.T) {
	hs := newHarness(t, true)
	full := hs.cl.listing
	hs.cl.listing = &clusters.Listing{Sites: full.Sites}
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady, Message: "kagent is ready"}
	c := hs.login(t)
	hs.do("GET", "/home", nil, c)
	if hs.cl.listCalls != 1 {
		t.Fatalf("list calls %d", hs.cl.listCalls)
	}
	hs.cl.listing = full
	*hs.clock = hs.clock.Add(3 * time.Minute)
	rr := hs.do("GET", "/clusters/site-1/c-abc12", nil, c)
	if rr.Code != 200 || hs.cl.listCalls != 2 {
		t.Fatalf("status %d list calls %d", rr.Code, hs.cl.listCalls)
	}
}

type chatCall struct {
	row                        clusters.Row
	principal, text, contextID string
	resume                     *chat.Resume
}

// stubChat records calls and emits script. A text of "block" waits for the
// request context to end before emitting done.
type stubChat struct {
	mu      sync.Mutex
	calls   []chatCall
	script  []chat.Out
	started chan struct{}
	ctxDone chan struct{}
	slowErr chan error
}

func (s *stubChat) Send(ctx context.Context, _ []auth.SiteResult, row clusters.Row, principal, text, contextID string, resume *chat.Resume, emit func(chat.Out)) {
	s.mu.Lock()
	s.calls = append(s.calls, chatCall{row, principal, text, contextID, resume})
	script := s.script
	s.mu.Unlock()
	s.started <- struct{}{}
	for _, o := range script {
		emit(o)
	}
	if text == "slow" {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
		}
		s.slowErr <- ctx.Err()
	}
	if text == "block" {
		<-ctx.Done()
		s.ctxDone <- struct{}{}
	}
	emit(chat.Out{Type: "done", Data: chat.Done{State: "completed", Events: len(script)}})
}

func (s *stubChat) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

const testCtxID = "123e4567-e89b-42d3-a456-426614174000"

func chatBody(text, ctxID string) string {
	b, _ := json.Marshal(map[string]string{"text": text, "context_id": ctxID})
	return string(b)
}

func (hs harness) csrfOf(t *testing.T, c *http.Cookie) string {
	t.Helper()
	sess, ok := hs.store.Get(c.Value)
	if !ok {
		t.Fatal("no session")
	}
	return sess.CSRF
}

func (hs harness) postChat(path, body, ctype, csrf string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if c != nil {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	hs.h.ServeHTTP(rr, req)
	return rr
}

func TestChat_StreamsEvents(t *testing.T) {
	hs := newHarness(t, true)
	hs.ch.script = []chat.Out{{Type: "progress", Data: chat.Progress{Text: "thinking"}}, {Type: "answer", Data: chat.Answer{Text: "hello"}}}
	c := hs.login(t)
	rr := hs.postChat("/clusters/site-1/c-abc12/chat", chatBody("why?", testCtxID), "application/json; charset=utf-8", hs.csrfOf(t, c), c)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "text/event-stream" || rr.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("status %d headers %v", rr.Code, rr.Header())
	}
	if rr.Header().Get("Content-Security-Policy") != wantCSP {
		t.Fatal("security headers missing on stream")
	}
	body := rr.Body.String()
	a, d := strings.Index(body, "event: answer\n"), strings.Index(body, "event: done\n")
	if a < 0 || d < a || !strings.Contains(body, `data: {"text":"hello"}`) {
		t.Fatalf("body %q", body)
	}
	if len(hs.ch.calls) != 1 {
		t.Fatalf("calls %d", len(hs.ch.calls))
	}
	call := hs.ch.calls[0]
	if call.row.Site != "site-1" || call.row.ID != "c-abc12" || call.principal != principal || call.text != "why?" || call.contextID != testCtxID {
		t.Fatalf("call %+v", call)
	}
}

func resumeBody(taskID string, answers any) string {
	b, _ := json.Marshal(map[string]any{"context_id": testCtxID, "task_id": taskID, "answers": answers})
	return string(b)
}

func TestChat_Resume(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	rr := hs.postChat("/clusters/site-1/c-abc12/chat", resumeBody("task-1", [][]string{{"prod"}, {"a", "b"}}), "application/json", hs.csrfOf(t, c), c)
	if rr.Code != 200 || len(hs.ch.calls) != 1 {
		t.Fatalf("status %d calls %d", rr.Code, len(hs.ch.calls))
	}
	r := hs.ch.calls[0].resume
	if r == nil || r.TaskID != "task-1" || !reflect.DeepEqual(r.Answers, [][]string{{"prod"}, {"a", "b"}}) {
		t.Fatalf("resume %+v", r)
	}
	if hs.ch.calls[0].contextID != testCtxID {
		t.Fatalf("call %+v", hs.ch.calls[0])
	}
}

func TestChat_Skip(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	body := `{"context_id":"` + testCtxID + `","task_id":"task-1","skip":true}`
	rr := hs.postChat("/clusters/site-1/c-abc12/chat", body, "application/json", hs.csrfOf(t, c), c)
	if rr.Code != 200 || len(hs.ch.calls) != 1 {
		t.Fatalf("status %d calls %d", rr.Code, len(hs.ch.calls))
	}
	if r := hs.ch.calls[0].resume; r == nil || !r.Skip || r.TaskID != "task-1" || r.Answers != nil {
		t.Fatalf("resume %+v", r)
	}
}

func TestChat_ResumeRejects(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	csrf := hs.csrfOf(t, c)
	many := make([][]string, 51)
	for i := range many {
		many[i] = []string{"x"}
	}
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"task without answers", resumeBody("task-1", nil), 400},
		{"answers without task", resumeBody("", [][]string{{"x"}}), 400},
		{"bad task id", resumeBody("../x", [][]string{{"x"}}), 400},
		{"empty answers", resumeBody("task-1", [][]string{}), 400},
		{"unanswered question", resumeBody("task-1", [][]string{{"x"}, {" "}}), 400},
		{"too many questions", resumeBody("task-1", many), 400},
		{"answers too long", resumeBody("task-1", [][]string{{strings.Repeat("x", 8193)}}), 413},
		{"bad context id", `{"context_id":"abc","task_id":"task-1","answers":[["x"]]}`, 400},
		{"skip with answers", `{"context_id":"` + testCtxID + `","task_id":"task-1","skip":true,"answers":[["x"]]}`, 400},
		{"skip without task", `{"context_id":"` + testCtxID + `","skip":true}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rr := hs.postChat("/clusters/site-1/c-abc12/chat", tc.body, "application/json", csrf, c); rr.Code != tc.want {
				t.Fatalf("status %d, want %d", rr.Code, tc.want)
			}
		})
	}
	if n := hs.ch.count(); n != 0 {
		t.Fatalf("Send called %d times", n)
	}
}

func TestChat_Rejects(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	csrf := hs.csrfOf(t, c)
	good := chatBody("hi", testCtxID)
	const p = "/clusters/site-1/c-abc12/chat"
	for _, tc := range []struct {
		name, path, body, ctype, csrf string
		cookie                        *http.Cookie
		want                          int
	}{
		{"no cookie", p, good, "application/json", csrf, nil, 401},
		{"bad cookie", p, good, "application/json", csrf, &http.Cookie{Name: "portal_session", Value: "bogus"}, 401},
		{"bad csrf", p, good, "application/json", "wrong", c, 403},
		{"no csrf", p, good, "application/json", "", c, 403},
		{"text/plain", p, good, "text/plain", csrf, c, 415},
		{"no content type", p, good, "", csrf, c, 415},
		{"text too long", p, chatBody(strings.Repeat("x", 8193), testCtxID), "application/json", csrf, c, 413},
		{"body too large", p, chatBody(strings.Repeat("x", 9300), testCtxID), "application/json", csrf, c, 413},
		{"whitespace", p, chatBody(" \n\t ", testCtxID), "application/json", csrf, c, 400},
		{"bad context id", p, chatBody("hi", "abc"), "application/json", csrf, c, 400},
		{"bad json", p, `{"text":`, "application/json", csrf, c, 400},
		{"unknown cluster", "/clusters/site-1/c-nope/chat", good, "application/json", csrf, c, 404},
		{"local", "/clusters/site-1/local/chat", good, "application/json", csrf, c, 404},
		{"unknown site", "/clusters/nosite/c-abc12/chat", good, "application/json", csrf, c, 404},
		{"disconnected", "/clusters/site-1/c-down1/chat", good, "application/json", csrf, c, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rr := hs.postChat(tc.path, tc.body, tc.ctype, tc.csrf, tc.cookie); rr.Code != tc.want {
				t.Fatalf("status %d, want %d", rr.Code, tc.want)
			}
		})
	}
	if n := hs.ch.count(); n != 0 {
		t.Fatalf("Send called %d times", n)
	}
}

// liveChat posts to a real server so the response streams.
func liveChat(t *testing.T, ctx context.Context, base string, hs harness, c *http.Cookie, text string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/clusters/site-1/c-abc12/chat", strings.NewReader(chatBody(text, testCtxID)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", hs.csrfOf(t, c))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func (hs harness) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hs.srv.mu.Lock()
		n := len(hs.srv.active)
		hs.srv.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("reply slot not freed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestChat_OneReplyPerSession(t *testing.T) {
	hs := newHarness(t, true)
	ts := httptest.NewServer(hs.h)
	defer ts.Close()
	c1, c2 := hs.login(t), hs.login(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := liveChat(t, ctx, ts.URL, hs, c1, "block")
	defer first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("first %d", first.StatusCode)
	}
	waitFor(t, hs.ch.started, "first Send")

	second := liveChat(t, context.Background(), ts.URL, hs, c1, "hi")
	second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second %d, want 429", second.StatusCode)
	}
	other := liveChat(t, context.Background(), ts.URL, hs, c2, "hi")
	b, _ := io.ReadAll(other.Body)
	other.Body.Close()
	if other.StatusCode != 200 || !strings.Contains(string(b), "event: done") {
		t.Fatalf("other session %d %q", other.StatusCode, b)
	}
	if n := hs.ch.count(); n != 2 {
		t.Fatalf("Send calls %d, want 2", n)
	}
}

func TestChat_CancelFreesSlot(t *testing.T) {
	hs := newHarness(t, true)
	ts := httptest.NewServer(hs.h)
	defer ts.Close()
	c := hs.login(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := liveChat(t, ctx, ts.URL, hs, c, "block")
	waitFor(t, hs.ch.started, "first Send")
	cancel()
	first.Body.Close()
	waitFor(t, hs.ch.ctxDone, "Send to see ctx.Done()")
	hs.waitIdle(t)

	next := liveChat(t, context.Background(), ts.URL, hs, c, "hi")
	next.Body.Close()
	if next.StatusCode != 200 {
		t.Fatalf("after cancel %d, want 200", next.StatusCode)
	}
}

func TestChat_401KeepsSession(t *testing.T) {
	hs := newHarness(t, true)
	hs.ch.script = []chat.Out{{Type: "error", Data: chat.Error{Message: "Your session on site-1 has expired: log in again"}}}
	c := hs.login(t)
	rr := hs.postChat("/clusters/site-1/c-abc12/chat", chatBody("hi", testCtxID), "application/json", hs.csrfOf(t, c), c)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "has expired") {
		t.Fatalf("status %d body %q", rr.Code, rr.Body.String())
	}
	if rr := hs.do("GET", "/home", nil, c); rr.Code != 200 {
		t.Fatalf("home after expired chat: %d", rr.Code)
	}
}

func TestWriteEvent_FramingSafe(t *testing.T) {
	var b strings.Builder
	if err := writeEvent(&b, chat.Out{Type: "answer", Data: chat.Answer{Text: "a\n\nevent: done\n<script>"}}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "event: answer\ndata: ") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("framing %q", out)
	}
	if strings.Count(out, "data:") != 1 || strings.Count(out, "\n") != 3 || strings.Contains(out, "<") {
		t.Fatalf("unsafe output %q", out)
	}
	b.Reset()
	if err := writeEvent(&b, chat.Out{Type: "ping"}); err != nil || b.String() != ": ping\n\n" {
		t.Fatalf("ping %q %v", b.String(), err)
	}
}

var (
	ctxIDRe        = regexp.MustCompile(`data-context-id="([^"]+)"`)
	dataCSRFRe     = regexp.MustCompile(`data-csrf="([^"]+)"`)
	inlineScriptRe = regexp.MustCompile(`<script[^>]*>[^<]+</script>`)
)

func TestCluster_ReadyHasChatPanel(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady, Message: "kagent is ready"}
	c := hs.login(t)
	var ids []string
	for i := 0; i < 2; i++ {
		body := hs.do("GET", "/clusters/site-1/c-abc12", nil, c).Body.String()
		for _, want := range []string{`id="chat"`, `data-chat-url="/clusters/site-1/c-abc12/chat"`, `id="log"`, `<textarea id="msg" maxlength="8192"`,
			`id="send"`, `id="stop"`, `id="count"`, `href="/clusters/site-1/c-abc12">+ New conversation</a>`, `data-cluster-name="prod-east"`,
			`data-max-duration-ms="1800000"`, `<script src="/static/markdown.js" defer></script>`, `<script src="/static/chat.js" defer></script>`} {
			if !strings.Contains(body, want) {
				t.Fatalf("page missing %q", want)
			}
		}
		if m := dataCSRFRe.FindStringSubmatch(body); m == nil || m[1] != hs.csrfOf(t, c) {
			t.Fatalf("data-csrf %v", m)
		}
		m := ctxIDRe.FindStringSubmatch(body)
		if m == nil || !a2a.ValidUUID(m[1]) {
			t.Fatalf("context id %v", m)
		}
		ids = append(ids, m[1])
		if inlineScriptRe.MatchString(body) {
			t.Fatal("inline script content")
		}
		if strings.Contains(body, "kagent isn't available") {
			t.Fatal("ready page shows the unavailable card")
		}
		if strings.Index(body, "markdown.js") > strings.Index(body, "chat.js") {
			t.Fatal("markdown.js must load before chat.js")
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("context id reused across loads")
	}
}

func TestChatJS_NoInnerHTML(t *testing.T) {
	src, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "document.write"} {
		if strings.Contains(string(src), bad) {
			t.Fatalf("chat.js contains %q", bad)
		}
	}
}

func failResult() auth.Result {
	return auth.Result{Sites: []auth.SiteResult{{Site: "site-1", Status: auth.StatusRejected}}}
}

func loginForm(user string) url.Values {
	return url.Values{"username": {user}, "password": {"pw-secret-value"}}
}

func TestLogin_UserLimit(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = failResult()
	for i := 0; i < 3; i++ {
		rr := hs.do("POST", "/login", loginForm("jdoe"), nil)
		if rr.Header().Get("Location") != "/?e=failed" {
			t.Fatalf("attempt %d: %q", i, rr.Header().Get("Location"))
		}
	}
	rr := hs.do("POST", "/login", loginForm("jdoe"), nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/?e=limited" || hs.stub.calls() != 3 {
		t.Fatalf("4th: %d %q calls %d", rr.Code, rr.Header().Get("Location"), hs.stub.calls())
	}
	if !strings.Contains(hs.do("GET", "/?e=limited", nil, nil).Body.String(), "Too many login attempts; try again later") {
		t.Fatal("message missing")
	}
}

func TestLogin_UsernameCaseFolded(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = failResult()
	for _, u := range []string{"JDoe", "jdoe", " jdoe "} {
		hs.do("POST", "/login", loginForm(u), nil)
	}
	rr := hs.do("POST", "/login", loginForm("JDOE"), nil)
	if rr.Header().Get("Location") != "/?e=limited" || hs.stub.calls() != 3 {
		t.Fatalf("%q calls %d", rr.Header().Get("Location"), hs.stub.calls())
	}
}

func TestLogin_IPLimit(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = failResult()
	for i := 0; i < 20; i++ {
		hs.do("POST", "/login", loginForm("user"+strings.Repeat("x", i)), nil)
	}
	rr := hs.do("POST", "/login", loginForm("another"), nil)
	if rr.Header().Get("Location") != "/?e=limited" || hs.stub.calls() != 20 {
		t.Fatalf("%q calls %d", rr.Header().Get("Location"), hs.stub.calls())
	}
}

func TestLogin_SuccessResetsUser(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = failResult()
	hs.do("POST", "/login", loginForm("jdoe"), nil)
	hs.do("POST", "/login", loginForm("jdoe"), nil)
	hs.stub.result = okResult()
	if rr := hs.do("POST", "/login", loginForm("jdoe"), nil); rr.Header().Get("Location") != "/home" {
		t.Fatalf("success: %q", rr.Header().Get("Location"))
	}
	hs.stub.result = failResult()
	for i := 0; i < 3; i++ {
		if rr := hs.do("POST", "/login", loginForm("jdoe"), nil); rr.Header().Get("Location") != "/?e=failed" {
			t.Fatalf("after reset %d: %q", i, rr.Header().Get("Location"))
		}
	}
}

func TestLogin_UnavailableRefundsUserAttempt(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = auth.Result{Sites: []auth.SiteResult{{Site: "site-1", Status: auth.StatusUnavailable}, {Site: "site-2", Status: auth.StatusUnavailable}}}
	for i := 0; i < 5; i++ {
		if rr := hs.do("POST", "/login", loginForm("jdoe"), nil); rr.Header().Get("Location") != "/?e=unavailable" {
			t.Fatalf("%d: %q", i, rr.Header().Get("Location"))
		}
	}
	if hs.stub.calls() != 5 {
		t.Fatalf("calls %d", hs.stub.calls())
	}
}

func TestLogin_ParallelAttemptsCapped(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = failResult()
	hs.stub.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hs.do("POST", "/login", loginForm("jdoe"), nil)
		}()
	}
	wg.Wait()
	if n := hs.stub.calls(); n > 3 {
		t.Fatalf("calls %d", n)
	}
}

func TestLogin_EmptyFieldsNotCounted(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = failResult()
	for i := 0; i < 10; i++ {
		hs.do("POST", "/login", url.Values{"username": {"jdoe"}}, nil)
	}
	for i := 0; i < 3; i++ {
		if rr := hs.do("POST", "/login", loginForm("jdoe"), nil); rr.Header().Get("Location") != "/?e=failed" {
			t.Fatalf("%d: %q", i, rr.Header().Get("Location"))
		}
	}
}

func TestLogin_RateLimitLogged(t *testing.T) {
	hs := newHarness(t, true)
	var buf syncBuf
	hs.srv.log = slog.New(slog.NewJSONHandler(&buf, nil))
	hs.stub.result = failResult()
	for i := 0; i < 4; i++ {
		hs.do("POST", "/login", loginForm("jdoe"), nil)
	}
	out := buf.String()
	if strings.Count(out, `"event":"login_rate_limited"`) != 1 || !strings.Contains(out, `"scope":"user"`) ||
		!strings.Contains(out, `"username":"jdoe"`) || !strings.Contains(out, `"ip":"192.0.2.1"`) {
		t.Fatalf("log: %s", out)
	}
	if strings.Contains(out, "pw-secret-value") {
		t.Fatal("password logged")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRevoke_CallsLogoutPerSession(t *testing.T) {
	stub := &stubAuthn{}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	mk := func(name string) *session.Session {
		r := okResult()
		r.Sites[0].Token.Bearer = "bearer-" + name + ":SECRETVALUE"
		return &session.Session{Username: name, Principal: principal, Sites: r.Sites}
	}
	var ended []session.Ended
	for i := range 20 {
		ended = append(ended, session.Ended{Session: mk("user" + strings.Repeat("x", i)), Reason: "evicted"})
	}
	Revoke(context.Background(), stub, ended, log)
	if stub.logoutCalls != 20 || len(stub.logoutAll) != 20 {
		t.Fatalf("logout calls = %d", stub.logoutCalls)
	}
	for _, sites := range stub.logoutAll {
		if len(sites) != 3 {
			t.Fatalf("sites = %d", len(sites))
		}
	}
	out := buf.String()
	if strings.Count(out, `"event":"session_ended"`) != 20 || !strings.Contains(out, `"reason":"evicted"`) {
		t.Fatalf("log: %s", out)
	}
	if strings.Contains(out, "SECRETVALUE") || strings.Contains(out, "bearer-") {
		t.Fatalf("bearer leaked: %s", out)
	}
}

func TestLogin_ReloginEndsOldSession(t *testing.T) {
	hs := newHarness(t, true)
	first := hs.login(t)
	rr := hs.do("POST", "/login", url.Values{"username": {"jdoe"}, "password": {"pw"}}, first)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/home" {
		t.Fatalf("status %d", rr.Code)
	}
	if rr := hs.do("GET", "/home", nil, first); rr.Code == 200 {
		t.Fatal("old cookie still opens /home")
	}
	got := hs.store.Sweep()
	if len(got) != 1 || got[0].Session.ID != first.Value || got[0].Reason != "relogin" {
		t.Fatalf("sweep %+v", got)
	}
}

func (hs harness) doHdr(method, path string, form url.Values, cookie *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	hs.h.ServeHTTP(rr, req)
	return rr
}

var coForm = url.Values{"username": {"jdoe"}, "password": {"pw"}}

func TestCookie_SameSiteStrict(t *testing.T) {
	hs := newHarness(t, false)
	c := hs.login(t)
	if c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("SameSite = %v", c.SameSite)
	}
}

func TestCrossOrigin_LoginBlocked(t *testing.T) {
	hs := newHarness(t, false)
	rr := hs.doHdr("POST", "/login", coForm, nil, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rr.Code != http.StatusForbidden || hs.stub.loginCalls != 0 {
		t.Fatalf("status %d calls %d", rr.Code, hs.stub.loginCalls)
	}
	if !strings.Contains(rr.Body.String(), "cross-origin request blocked") {
		t.Fatalf("body %q", rr.Body.String())
	}
}

func TestCrossOrigin_OriginMismatchBlocked(t *testing.T) {
	hs := newHarness(t, false)
	rr := hs.doHdr("POST", "/logout", nil, nil, map[string]string{"Origin": "https://evil.example", "Host": "portal.example"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestCrossOrigin_SameOriginAllowed(t *testing.T) {
	hs := newHarness(t, false)
	rr := hs.doHdr("POST", "/login", coForm, nil, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/home" {
		t.Fatalf("status %d location %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestCrossOrigin_NoHeadersAllowed(t *testing.T) {
	hs := newHarness(t, false)
	rr := hs.do("POST", "/login", coForm, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/home" {
		t.Fatalf("status %d location %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestCrossOrigin_GetUnaffected(t *testing.T) {
	hs := newHarness(t, false)
	c := hs.login(t)
	rr := hs.doHdr("GET", "/home", nil, c, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestCrossOrigin_Logged(t *testing.T) {
	hs := newHarness(t, false)
	var buf syncBuf
	hs.srv.log = slog.New(slog.NewJSONHandler(&buf, nil))
	hs.doHdr("POST", "/login", coForm, nil, map[string]string{"Sec-Fetch-Site": "cross-site"})
	out := buf.String()
	if strings.Count(out, `"event":"cross_origin_blocked"`) != 1 || !strings.Contains(out, `"method":"POST"`) ||
		!strings.Contains(out, `"path":"/login"`) || !strings.Contains(out, `"ip":"192.0.2.1"`) || strings.Contains(out, `"pw"`) {
		t.Fatalf("log: %s", out)
	}
}

func allUnavailable(timeout bool) auth.Result {
	return auth.Result{Sites: []auth.SiteResult{
		{Site: "site-1", Status: auth.StatusUnavailable, Timeout: timeout},
		{Site: "site-2", Status: auth.StatusUnavailable}}}
}

// doCtx posts the login form with the given request context and RemoteAddr
// ("" keeps httptest's default).
func (hs harness) doCtx(ctx context.Context, remote string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, "POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if remote != "" {
		req.RemoteAddr = remote
	}
	rr := httptest.NewRecorder()
	hs.h.ServeHTTP(rr, req)
	return rr
}

func TestLogin_ClientAbortNoRefund(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = allUnavailable(false)
	var cancel context.CancelFunc
	var loginCtxErrs []error
	hs.stub.onLogin = func(ctx context.Context) {
		cancel() // the client goes away while Rancher is being asked
		loginCtxErrs = append(loginCtxErrs, ctx.Err())
	}
	for i := 0; i < 3; i++ {
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		hs.doCtx(ctx, "", loginForm("jdoe"))
		c()
	}
	for i, err := range loginCtxErrs {
		if err != nil {
			t.Fatalf("attempt %d: Login context cancelled with the request: %v", i, err)
		}
	}
	hs.stub.onLogin = nil
	rr := hs.do("POST", "/login", loginForm("jdoe"), nil)
	if rr.Header().Get("Location") != "/?e=limited" || hs.stub.calls() != 3 {
		t.Fatalf("4th: %q calls %d", rr.Header().Get("Location"), hs.stub.calls())
	}
}

func TestLogin_TimeoutNoRefund(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result = allUnavailable(true)
	for i := 0; i < 3; i++ {
		if rr := hs.do("POST", "/login", loginForm("jdoe"), nil); rr.Header().Get("Location") != "/?e=unavailable" {
			t.Fatalf("%d: %q", i, rr.Header().Get("Location"))
		}
	}
	rr := hs.do("POST", "/login", loginForm("jdoe"), nil)
	if rr.Header().Get("Location") != "/?e=limited" || hs.stub.calls() != 3 {
		t.Fatalf("4th: %q calls %d", rr.Header().Get("Location"), hs.stub.calls())
	}
}

func TestLogin_DomainFormsShareBucket(t *testing.T) {
	hs := newHarness(t, true)
	var buf syncBuf
	hs.srv.log = slog.New(slog.NewJSONHandler(&buf, nil))
	hs.stub.result = failResult()
	for _, u := range []string{"jdoe", `CORP\jdoe`, "jdoe@corp.example"} {
		hs.do("POST", "/login", loginForm(u), nil)
	}
	for _, u := range []string{" JDoe ", `CORP\JDoe`, "JDOE@corp.example"} {
		rr := hs.do("POST", "/login", loginForm(u), nil)
		if rr.Header().Get("Location") != "/?e=limited" {
			t.Fatalf("%q: %q", u, rr.Header().Get("Location"))
		}
	}
	if hs.stub.calls() != 3 {
		t.Fatalf("calls %d", hs.stub.calls())
	}
	if strings.Count(buf.String(), `"username":"jdoe"`) != 3 {
		t.Fatalf("log should use the limiter key: %s", buf.String())
	}
}

func TestLogin_SuccessReleasesIP(t *testing.T) {
	hs := newHarness(t, true)
	hs.srv.ipLim = ratelimit.New(3, 15*time.Minute, hs.srv.now)
	for i := 0; i < 4; i++ {
		rr := hs.do("POST", "/login", loginForm(fmt.Sprintf("user%d", i)), nil)
		if rr.Header().Get("Location") != "/home" {
			t.Fatalf("login %d: %q", i, rr.Header().Get("Location"))
		}
	}
	// Failures still count against the IP.
	hs.stub.result = failResult()
	for i := 0; i < 3; i++ {
		hs.do("POST", "/login", loginForm(fmt.Sprintf("bad%d", i)), nil)
	}
	if rr := hs.do("POST", "/login", loginForm("bad9"), nil); rr.Header().Get("Location") != "/?e=limited" {
		t.Fatalf("after failures: %q", rr.Header().Get("Location"))
	}
}

func TestLogin_IPv6SharesSlash64(t *testing.T) {
	hs := newHarness(t, true)
	hs.srv.ipLim = ratelimit.New(2, 15*time.Minute, hs.srv.now)
	hs.stub.result = failResult()
	bg := context.Background()
	hs.doCtx(bg, "[2001:db8::1]:1234", loginForm("a"))
	hs.doCtx(bg, "[2001:db8::2]:1234", loginForm("b"))
	if rr := hs.doCtx(bg, "[2001:db8::ffff:1]:1234", loginForm("c")); rr.Header().Get("Location") != "/?e=limited" {
		t.Fatalf("same /64: %q", rr.Header().Get("Location"))
	}
	if rr := hs.doCtx(bg, "[2001:db8:0:1::1]:1234", loginForm("d")); rr.Header().Get("Location") != "/?e=failed" {
		t.Fatalf("other /64: %q", rr.Header().Get("Location"))
	}
	if rr := hs.doCtx(bg, "198.51.100.7:1234", loginForm("e")); rr.Header().Get("Location") != "/?e=failed" {
		t.Fatalf("ipv4: %q", rr.Header().Get("Location"))
	}
}

func TestLogout_RevokeSurvivesClientDisconnect(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	form := url.Values{"csrf": {csrfFrom(t, hs, c)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, "POST", "/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	hs.h.ServeHTTP(httptest.NewRecorder(), req)
	if hs.stub.logoutCalls != 1 {
		t.Fatalf("logout calls %d", hs.stub.logoutCalls)
	}
	if err := hs.stub.logoutCtxErr; err != nil {
		t.Fatalf("logout context cancelled with the request: %v", err)
	}
	if dl := hs.stub.logoutDeadline; !hs.stub.logoutHasDL || time.Until(dl) > 30*time.Second {
		t.Fatalf("logout context deadline %v %v", dl, hs.stub.logoutHasDL)
	}
}

func TestDiagnostics_Gone(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	if rr := hs.do("GET", "/diagnostics", nil, c); rr.Code != http.StatusNotFound {
		t.Fatalf("status %d", rr.Code)
	}
	if body := hs.do("GET", "/home", nil, c).Body.String(); strings.Contains(body, "/diagnostics") {
		t.Fatal("home still links to diagnostics")
	}
}

func TestLogin_ShowsUIConfig(t *testing.T) {
	hs := newHarness(t, true)
	hs.srv.cfg.UI = config.UIConfig{Banner: "Maintenance <b>Sat</b>", BannerLevel: "warning",
		LoginHelp: "See https://wiki.example/x\n\nMail ops@example.com", SupportEmail: "platform@example.com"}
	body := hs.do("GET", "/", nil, nil).Body.String()
	for _, want := range []string{"banner-warning", "Maintenance &lt;b&gt;Sat&lt;/b&gt;", `href="https://wiki.example/x"`,
		`href="mailto:ops@example.com"`, `href="mailto:platform@example.com"`, "Sign in", "AI assistant for Rancher-managed clusters"} {
		if !strings.Contains(body, want) {
			t.Errorf("login missing %q", want)
		}
	}
	if strings.Contains(body, "chat test") || strings.Contains(body, "Contact your platform team") {
		t.Fatal("test-harness tag or fallback footer rendered")
	}
}

var bannerClassRe = regexp.MustCompile(`class="[^"]*\bbanner\b`)

func TestLogin_NoUIConfig(t *testing.T) {
	hs := newHarness(t, true)
	body := hs.do("GET", "/", nil, nil).Body.String()
	if bannerClassRe.MatchString(body) {
		t.Fatal("banner rendered without config")
	}
	if !strings.Contains(body, "Need help? Contact your platform team.") {
		t.Fatal("fallback footer missing")
	}
	if strings.Contains(body, "Your session has expired") {
		t.Fatal("expired notice without ?expired=1")
	}
}

func TestIndex_ExpiredNotice(t *testing.T) {
	hs := newHarness(t, true)
	body := hs.do("GET", "/?expired=1", nil, nil).Body.String()
	if !strings.Contains(body, "Your session has expired. Please sign in again.") {
		t.Fatal("expired notice missing")
	}
}

func TestLogout_PartialRevoke(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.failRevoke = true
	c := hs.login(t)
	body := hs.do("POST", "/logout", url.Values{"csrf": {csrfFrom(t, hs, c)}}, c).Body.String()
	if !strings.Contains(body, "Some sessions couldn") || !strings.Contains(body, "be closed and will expire on their own.") {
		t.Fatalf("partial revoke line missing: %s", body)
	}
}

func TestPages_NoInlineStyleOrScript(t *testing.T) {
	hs := newHarness(t, true)
	hs.srv.cfg.UI = config.UIConfig{Banner: "b", BannerLevel: "info", LoginHelp: "h", SupportEmail: "p@example.com"}
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady, Message: "kagent is ready"}
	pages := []string{hs.do("GET", "/", nil, nil).Body.String(), hs.do("GET", "/?expired=1", nil, nil).Body.String()}
	c := hs.login(t)
	pages = append(pages, hs.do("GET", "/home", nil, c).Body.String(), hs.do("GET", "/clusters/site-1/c-abc12", nil, c).Body.String())
	pages = append(pages, hs.do("POST", "/logout", url.Values{"csrf": {hs.csrfOf(t, c)}}, c).Body.String())
	for i, body := range pages {
		if strings.Contains(body, "style=") || inlineScriptRe.MatchString(body) || strings.Contains(body, "<style") {
			t.Fatalf("page %d has inline style or script", i)
		}
	}
}

func TestShell_UserMenu(t *testing.T) {
	hs := newHarness(t, true)
	hs.srv.cfg.UI.SupportEmail = "platform@example.com"
	c := hs.login(t)
	body := hs.do("GET", "/home", nil, c).Body.String()
	for _, want := range []string{"kagent portal", `<details class="user-menu">`, "jdoe", `href="mailto:platform@example.com"`, "v-test",
		`action="/logout"`, `name="csrf" value="` + hs.csrfOf(t, c) + `"`, "Sign out"} {
		if !strings.Contains(body, want) {
			t.Errorf("shell missing %q", want)
		}
	}
}

func TestHelpParagraphs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want [][]Token
	}{
		{"", nil},
		{"a\n\nb", [][]Token{{{Text: "a"}}, {{Text: "b"}}}},
		{"a\nb\n\n\n  \nc", [][]Token{{{Text: "a\nb"}}, {{Text: "c"}}}},
		{"go to https://x.example/a?b=1.", [][]Token{{{Text: "go to "}, {Text: "https://x.example/a?b=1", Href: "https://x.example/a?b=1"}, {Text: "."}}}},
		{"(see http://x.example/p), ok", [][]Token{{{Text: "(see "}, {Text: "http://x.example/p", Href: "http://x.example/p"}, {Text: "), ok"}}}},
		{"javascript:alert(1)", [][]Token{{{Text: "javascript:alert(1)"}}}},
		{"x@y.example", [][]Token{{{Text: "x@y.example", Href: "mailto:x@y.example"}}}},
		{"mail ops@example.com.", [][]Token{{{Text: "mail "}, {Text: "ops@example.com", Href: "mailto:ops@example.com"}, {Text: "."}}}},
	} {
		if got := helpParagraphs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("helpParagraphs(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

const (
	convA = "11111111-1111-4111-8111-111111111111"
	convB = "22222222-2222-4222-8222-222222222222"
)

func (hs harness) logoutCount() int {
	hs.stub.mu.Lock()
	defer hs.stub.mu.Unlock()
	return hs.stub.logoutCalls
}

// waitLogout polls until the stub's Logout has been called n times.
func (hs harness) waitLogout(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hs.logoutCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("logout calls %d, want %d", hs.logoutCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertExpired checks a 303 to /?expired=1 that clears the cookie, drops
// the session and revokes its tokens.
func (hs harness) assertExpired(t *testing.T, rr *httptest.ResponseRecorder, c *http.Cookie, before int) {
	t.Helper()
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/?expired=1" {
		t.Fatalf("status %d location %q", rr.Code, rr.Header().Get("Location"))
	}
	cleared := false
	for _, ck := range rr.Result().Cookies() {
		cleared = cleared || (ck.Name == cookieName && ck.Value == "" && ck.MaxAge < 0)
	}
	if !cleared {
		t.Fatalf("cookie not cleared: %q", rr.Header().Get("Set-Cookie"))
	}
	if n := hs.store.Len(); n != before-1 {
		t.Fatalf("store len %d, want %d", n, before-1)
	}
	if _, ok := hs.store.Get(c.Value); ok {
		t.Fatal("session still present")
	}
	hs.waitLogout(t, 1)
}

func TestCluster_Unavailable(t *testing.T) {
	for _, oc := range []clusters.Outcome{clusters.OutcomeNoAccess, clusters.OutcomeNotInstalled, clusters.OutcomeNotRunning,
		clusters.OutcomeUnreachable, clusters.OutcomeDenied, clusters.OutcomeBadCard, clusters.OutcomeUnexpected, clusters.OutcomeCheckFailed} {
		t.Run(string(oc), func(t *testing.T) {
			hs := newHarness(t, true)
			hs.srv.cfg.UI.SupportEmail = "platform@example.com"
			hs.cl.check = clusters.CheckResult{Outcome: oc, Message: "Unexpected response (HTTP 500)", SSARReason: "SSAR said no", Error: "HTTP 500"}
			c := hs.login(t)
			rr := hs.do("GET", "/clusters/site-1/c-abc12", nil, c)
			body := rr.Body.String()
			if rr.Code != 200 {
				t.Fatalf("status %d", rr.Code)
			}
			for _, want := range []string{"kagent isn't available on this cluster",
				"It may not be installed, may not be running, or your account may not have access to it.",
				`Contact the platform team at <a href="mailto:platform@example.com">platform@example.com</a>`,
				"Reference: " + string(oc) + " · prod-east", "← Back to clusters", `href="/home"`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
			for _, bad := range []string{"SSAR", "HTTP", `id="chat"`, "chat.js"} {
				if strings.Contains(body, bad) {
					t.Errorf("body contains %q", bad)
				}
			}
			if n := hs.hist.callCount(); n != 0 {
				t.Fatalf("history called %d times", n)
			}
		})
	}
}

func TestCluster_UnavailableNoSupportEmail(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeNotInstalled}
	c := hs.login(t)
	body := hs.do("GET", "/clusters/site-1/c-abc12", nil, c).Body.String()
	if !strings.Contains(body, "Contact your platform team.") || strings.Contains(body, "Contact the platform team at") {
		t.Fatal("fallback contact line wrong")
	}
}

func TestCluster_ExpiredEndsSession(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeExpired}
	c := hs.login(t)
	before := hs.store.Len()
	rr := hs.do("GET", "/clusters/site-1/c-abc12", nil, c)
	hs.assertExpired(t, rr, c, before)
	if n := hs.hist.callCount(); n != 0 {
		t.Fatalf("history called %d times", n)
	}
}

func TestCluster_ReadyRendersSidebar(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady}
	upd := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	hs.hist.convs = []history.Conversation{{ID: convA, Title: "Pods <not> ready", Updated: upd}, {ID: convB, Title: "Ingress 502"}}
	c := hs.login(t)

	body := hs.do("GET", "/clusters/site-1/c-abc12?c="+convB, nil, c).Body.String()
	for _, want := range []string{`<ul id="convs">`, "Conversations on this cluster are shared with everyone who can use kagent here.",
		`<a href="?c=` + convA + `" data-updated="2026-11-20T10:30:00Z">Pods &lt;not&gt; ready</a>`,
		`<a href="?c=` + convB + `" data-updated="" aria-current="page">Ingress 502</a>`,
		`data-transcript-url="/clusters/site-1/c-abc12/conversations/` + convB + `"`,
		`data-context-id="` + convB + `"`, `id="sidebar-toggle"`, `<li aria-current="page">prod-east</li>`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Count(body, `aria-current="page"`) != 2 { // crumb + current conversation
		t.Fatalf("aria-current count %d", strings.Count(body, `aria-current="page"`))
	}

	body = hs.do("GET", "/clusters/site-1/c-abc12", nil, c).Body.String()
	if strings.Contains(body, "data-transcript-url") {
		t.Fatal("transcript url without ?c=")
	}
	if m := ctxIDRe.FindStringSubmatch(body); m == nil || !a2a.ValidUUID(m[1]) || m[1] == convA || m[1] == convB {
		t.Fatalf("context id %v", m)
	}
	if n := hs.hist.callCount(); n != 2 {
		t.Fatalf("history calls %d", n)
	}
}

func TestCluster_BadC(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady}
	c := hs.login(t)
	for _, q := range []string{"?c=not-a-uuid", "?c=" + convA + "&c=" + convA, "?c="} {
		rr := hs.do("GET", "/clusters/site-1/c-abc12"+q, nil, c)
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/clusters/site-1/c-abc12" {
			t.Fatalf("%s: status %d location %q", q, rr.Code, rr.Header().Get("Location"))
		}
	}
	if hs.cl.checkCalls != 0 {
		t.Fatalf("check calls %d", hs.cl.checkCalls)
	}
}

func TestCluster_HistoryForbidden(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady}
	hs.hist.listErr = history.ErrForbidden
	c := hs.login(t)
	body := hs.do("GET", "/clusters/site-1/c-abc12", nil, c).Body.String()
	if !strings.Contains(body, "Conversation history isn't available for your account") || !strings.Contains(body, `id="chat"`) {
		t.Fatal("forbidden notice or chat missing")
	}
	if strings.Contains(body, "Retry") {
		t.Fatal("retry link on forbidden")
	}
}

func TestCluster_HistoryFailed(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady}
	hs.hist.listErr = fmt.Errorf("boom")
	c := hs.login(t)
	body := hs.do("GET", "/clusters/site-1/c-abc12?c="+convA, nil, c).Body.String()
	for _, want := range []string{"Couldn't load conversation history", `href="/clusters/site-1/c-abc12?c=` + convA + `">Retry</a>`, `id="chat"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if strings.Contains(body, "boom") {
		t.Fatal("error detail leaked")
	}
}

func TestCluster_HistoryExpired(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady}
	hs.hist.listErr = history.ErrExpired
	c := hs.login(t)
	before := hs.store.Len()
	rr := hs.do("GET", "/clusters/site-1/c-abc12", nil, c)
	hs.assertExpired(t, rr, c, before)
}

func TestTranscript_Endpoint(t *testing.T) {
	path := "/clusters/site-1/c-abc12/conversations/" + convA
	for _, tc := range []struct {
		name  string
		err   error
		want  int
		body  string
		ended bool
	}{
		{"ok", nil, 200, `"turns"`, false},
		{"not found", history.ErrNotFound, 404, `{"error":"not_found"}`, false},
		{"forbidden", history.ErrForbidden, 403, "", false},
		{"expired", history.ErrExpired, 401, "", true},
		{"other", fmt.Errorf("boom"), 502, `{"error":"failed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t, true)
			hs.hist.trErr = tc.err
			c := hs.login(t)
			rr := hs.do("GET", path, nil, c)
			if rr.Code != tc.want || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("status %d content-type %q", rr.Code, rr.Header().Get("Content-Type"))
			}
			if !strings.Contains(rr.Body.String(), tc.body) || strings.Contains(rr.Body.String(), "boom") {
				t.Fatalf("body %q", rr.Body.String())
			}
			if _, ok := hs.store.Get(c.Value); ok == tc.ended {
				t.Fatalf("session present %v", ok)
			}
			if tc.ended {
				hs.waitLogout(t, 1)
			}
			hs.hist.mu.Lock()
			calls := slices.Clone(hs.hist.calls)
			hs.hist.mu.Unlock()
			if !reflect.DeepEqual(calls, []string{"transcript:" + convA}) {
				t.Fatalf("history calls %v", calls)
			}
			if hs.cl.checkCalls != 0 {
				t.Fatalf("check calls %d", hs.cl.checkCalls)
			}
		})
	}
}

func TestTranscript_Rejects(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	for _, tc := range []struct {
		name, path string
		cookie     *http.Cookie
		want       int
	}{
		{"no session", "/clusters/site-1/c-abc12/conversations/" + convA, nil, 401},
		{"bad cid", "/clusters/site-1/c-abc12/conversations/not-a-uuid", c, 404},
		{"unknown cluster", "/clusters/site-1/c-nope/conversations/" + convA, c, 404},
	} {
		rr := hs.do("GET", tc.path, nil, tc.cookie)
		if rr.Code != tc.want || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: status %d content-type %q", tc.name, rr.Code, rr.Header().Get("Content-Type"))
		}
	}
	if n := hs.hist.callCount(); n != 0 {
		t.Fatalf("history called %d times", n)
	}
}

func TestChat_ExpiredCodeEndsSession(t *testing.T) {
	hs := newHarness(t, true)
	hs.ch.script = []chat.Out{{Type: "error", Data: chat.Error{Message: "Your session has expired", Code: "expired"}}}
	c := hs.login(t)
	rr := hs.postChat("/clusters/site-1/c-abc12/chat", chatBody("hi", testCtxID), "application/json", hs.csrfOf(t, c), c)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"code":"expired"`) || !strings.Contains(rr.Body.String(), "event: done") {
		t.Fatalf("status %d body %q", rr.Code, rr.Body.String())
	}
	if _, ok := hs.store.Get(c.Value); ok {
		t.Fatal("session still present")
	}
	hs.waitLogout(t, 1)
	time.Sleep(20 * time.Millisecond)
	if n := hs.logoutCount(); n != 1 {
		t.Fatalf("logout calls %d", n)
	}
}
