package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/chat"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/session"
)

const principal = "activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com"

const wantCSP = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

type stubAuthn struct {
	result      auth.Result
	loginCalls  int
	logoutSites []auth.SiteResult
	logoutCalls int
}

func (s *stubAuthn) Login(_ context.Context, _, _ string) auth.Result {
	s.loginCalls++
	return s.result
}
func (s *stubAuthn) Logout(_ context.Context, sites []auth.SiteResult) []auth.LogoutResult {
	s.logoutCalls++
	s.logoutSites = sites
	var out []auth.LogoutResult
	for _, st := range sites {
		out = append(out, auth.LogoutResult{Site: st.Site, LoggedOut: true, Revoked: true})
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

type harness struct {
	h     http.Handler
	srv   *server
	stub  *stubAuthn
	store *session.Store
	cl    *stubClusters
	ch    *stubChat
	clock *time.Time
}

func newHarness(t *testing.T, secure bool) harness {
	t.Helper()
	cfg := &config.Config{CookieSecure: &secure, Sites: []config.Site{{Name: "site-1"}, {Name: "site-2"}, {Name: "site-3"}},
		Clusters: config.ClustersConfig{CacheTTL: 2 * time.Minute}, Chat: config.ChatConfig{MaxMessageBytes: 8192}}
	stub := &stubAuthn{result: okResult()}
	clock := time.Now()
	hs := &harness{}
	cl := &stubClusters{listing: defaultListing(), now: func() time.Time { return *hs.clock }}
	store := session.NewStore(30*time.Minute, 8*time.Hour, time.Now)
	ch := &stubChat{started: make(chan struct{}, 8), ctxDone: make(chan struct{}, 8)}
	srv, h, err := build(cfg, stub, cl, ch, store, "v-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return *hs.clock }
	hs.h, hs.srv, hs.stub, hs.store, hs.cl, hs.ch, hs.clock = h, srv, stub, store, cl, ch, &clock
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
	for _, want := range []string{"portal_session=", "HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
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

func TestDiagnostics_RancherUserFromMe(t *testing.T) {
	hs := newHarness(t, true)
	for i := range hs.stub.result.Sites {
		hs.stub.result.Sites[i].Token.UserID = ""
	}
	c := hs.login(t)
	body := hs.do("GET", "/diagnostics", nil, c).Body.String()
	if !strings.Contains(body, "u-abc") || !strings.Contains(body, "jdoe") {
		t.Fatal("Rancher user id/username from users?me=true not shown")
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

func TestDiagnostics_ShowsLoginAndClusterTables(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	rr := hs.do("GET", "/diagnostics", nil, c)
	body := rr.Body.String()
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	for _, want := range []string{"site-1", "site-2", "site-3", "token-abc12", "2026-11-20T17:00:00Z", "u-abc",
		"activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com", "connection refused", "unavailable", "v-test",
		"c-abc12", "prod-east", "provisioning", "Ready: True AllGood", "Connected: True", "Cluster list"} {
		if !strings.Contains(body, want) {
			t.Fatalf("diagnostics missing %q", want)
		}
	}
}

func TestHome_EscapesHTML(t *testing.T) {
	hs := newHarness(t, true)
	hs.stub.result.Warnings = []string{"<script>alert(1)</script>"}
	c := hs.login(t)
	body := hs.do("GET", "/home", nil, c).Body.String()
	if strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("warning not escaped")
	}
}

func TestPages_DoNotLeakToken(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady, Message: "kagent is ready"}
	c := hs.login(t)
	for _, p := range []string{"/home", "/diagnostics", "/clusters/site-1/c-abc12"} {
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
	if !strings.Contains(rr.Body.String(), "Revoked") {
		t.Fatal("logged-out page missing results")
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
	for _, want := range []string{"prod-east", "dev-west", "prod-south", "c-abc12", "site-1: 2 clusters", "site-2: 1",
		`href="/clusters/site-1/c-abc12"`, "Diagnostics", "Refresh"} {
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
	if rr.Code != 200 || !strings.Contains(body, "token expired or revoked: log in again") || !strings.Contains(body, `class="warning"`) ||
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
	for _, want := range []string{"kagent is ready", "result-ready", "k8s-agent", "prod-east", `id="chat"`, `href="/home"`, "<th>Outcome</th><td>ready</td>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cluster page missing %q", want)
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
}

// stubChat records calls and emits script. A text of "block" waits for the
// request context to end before emitting done.
type stubChat struct {
	mu      sync.Mutex
	calls   []chatCall
	script  []chat.Out
	started chan struct{}
	ctxDone chan struct{}
}

func (s *stubChat) Send(ctx context.Context, _ []auth.SiteResult, row clusters.Row, principal, text, contextID string, emit func(chat.Out)) {
	s.mu.Lock()
	s.calls = append(s.calls, chatCall{row, principal, text, contextID})
	script := s.script
	s.mu.Unlock()
	s.started <- struct{}{}
	for _, o := range script {
		emit(o)
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
			`id="send"`, `id="stop" disabled`, `id="new" href="/clusters/site-1/c-abc12"`, `<script src="/static/chat.js" defer></script>`, "chat test"} {
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
		if strings.Contains(body, "Chat unavailable") {
			t.Fatal("ready page shows unavailable button")
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("context id reused across loads")
	}
}

func TestCluster_NotReadyNoChat(t *testing.T) {
	hs := newHarness(t, true)
	hs.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeNotRunning, Message: "kagent is installed but not running"}
	c := hs.login(t)
	body := hs.do("GET", "/clusters/site-1/c-abc12", nil, c).Body.String()
	if !strings.Contains(body, "<button disabled>Chat unavailable: kagent is installed but not running</button>") {
		t.Fatal("unavailable button missing")
	}
	if strings.Contains(body, `id="chat"`) || strings.Contains(body, "chat.js") {
		t.Fatal("chat panel on a not-ready page")
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
