package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/session"
)

const principal = "activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com"

const wantCSP = "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

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
	stub  *stubAuthn
	store *session.Store
	cl    *stubClusters
	clock *time.Time
}

func newHarness(t *testing.T, secure bool) harness {
	t.Helper()
	cfg := &config.Config{CookieSecure: &secure, Sites: []config.Site{{Name: "site-1"}, {Name: "site-2"}, {Name: "site-3"}},
		Clusters: config.ClustersConfig{CacheTTL: 2 * time.Minute}}
	stub := &stubAuthn{result: okResult()}
	clock := time.Now()
	hs := &harness{}
	cl := &stubClusters{listing: defaultListing(), now: func() time.Time { return *hs.clock }}
	store := session.NewStore(30*time.Minute, 8*time.Hour, time.Now)
	srv, h, err := build(cfg, stub, cl, store, "v-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return *hs.clock }
	hs.h, hs.stub, hs.store, hs.cl, hs.clock = h, stub, store, cl, &clock
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
	for _, p := range []string{"/", "/home", "/diagnostics", "/healthz", "/version", "/static/style.css", "/nope"} {
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
	for _, want := range []string{"kagent is ready", "result-ready", "k8s-agent", "prod-east", "<button disabled>Chat (coming in Plan 3)</button>", `href="/home"`, "<th>Outcome</th><td>ready</td>"} {
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
