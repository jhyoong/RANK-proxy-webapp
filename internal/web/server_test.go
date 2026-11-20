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

type harness struct {
	h     http.Handler
	stub  *stubAuthn
	store *session.Store
}

func newHarness(t *testing.T, secure bool) harness {
	t.Helper()
	cfg := &config.Config{CookieSecure: &secure, Sites: []config.Site{{Name: "site-1"}, {Name: "site-2"}, {Name: "site-3"}}}
	stub := &stubAuthn{result: okResult()}
	store := session.NewStore(30*time.Minute, 8*time.Hour, time.Now)
	h, err := New(cfg, stub, store, "v-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return harness{h, stub, store}
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

func TestHome_RancherUserFromMe(t *testing.T) {
	hs := newHarness(t, true)
	for i := range hs.stub.result.Sites {
		hs.stub.result.Sites[i].Token.UserID = ""
	}
	c := hs.login(t)
	body := hs.do("GET", "/home", nil, c).Body.String()
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

func TestHome_ShowsSiteTable(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	rr := hs.do("GET", "/home", nil, c)
	body := rr.Body.String()
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	for _, want := range []string{"site-1", "site-2", "site-3", "token-abc12", "2026-11-20T17:00:00Z", "u-abc",
		"activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com", "connection refused", "unavailable", "v-test"} {
		if !strings.Contains(body, want) {
			t.Fatalf("home missing %q", want)
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

func TestHome_DoesNotLeakToken(t *testing.T) {
	hs := newHarness(t, true)
	c := hs.login(t)
	if body := hs.do("GET", "/home", nil, c).Body.String(); strings.Contains(body, "SECRETVALUE") {
		t.Fatal("bearer secret rendered")
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
	for _, p := range []string{"/", "/home", "/healthz", "/version", "/static/style.css", "/nope"} {
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
