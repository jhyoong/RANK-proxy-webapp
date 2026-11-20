package auth_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

const principal = "activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func user(pw, p string) ranchertest.FakeUser {
	return ranchertest.FakeUser{Password: pw, ID: "u-1", PrincipalIDs: []string{"local://u-1", p}}
}

func fakes() []*ranchertest.Fake {
	var fs []*ranchertest.Fake
	for range 3 {
		fs = append(fs, &ranchertest.Fake{Users: map[string]ranchertest.FakeUser{"jdoe": user("pw", principal)}})
	}
	return fs
}

type env struct {
	a    *auth.Authenticator
	logs *syncBuf
}

// setup starts each fake (or uses an explicit URL for nil entries) and builds an Authenticator.
func setup(t *testing.T, fs []*ranchertest.Fake, timeout time.Duration, urls ...string) env {
	t.Helper()
	var clients []*rancher.Client
	for i, f := range fs {
		site := config.Site{Name: []string{"site-1", "site-2", "site-3"}[i]}
		if f == nil {
			site.URL = urls[0]
			urls = urls[1:]
		} else {
			site.URL, site.CAFile = ranchertest.NewTLS(t, f)
		}
		c, err := rancher.NewClient(site, timeout)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	return env{auth.New(clients, time.Hour, log), logs}
}

func statuses(r auth.Result) []auth.Status {
	var s []auth.Status
	for _, sr := range r.Sites {
		s = append(s, sr.Status)
	}
	return s
}

func eq(a []auth.Status, b ...auth.Status) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLogin_AllSitesOK(t *testing.T) {
	e := setup(t, fakes(), 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if !r.OK || !eq(statuses(r), auth.StatusOK, auth.StatusOK, auth.StatusOK) || r.PrincipalID != principal || len(r.Warnings) != 0 {
		t.Fatalf("got %+v", r)
	}
	for _, s := range r.Sites {
		if s.User == nil || s.Token.ID == "" || s.HTTPCode != 200 {
			t.Fatalf("site %+v", s)
		}
	}
}

func TestLogin_WrongPasswordOnlySite1Tried(t *testing.T) {
	fs := fakes()
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "bad")
	if r.OK || !eq(statuses(r), auth.StatusRejected, auth.StatusSkipped, auth.StatusSkipped) {
		t.Fatalf("got %v", statuses(r))
	}
	if r.Sites[0].HTTPCode != 401 || fs[1].LoginCalls() != 0 || fs[2].LoginCalls() != 0 {
		t.Fatalf("code=%d calls=%d,%d", r.Sites[0].HTTPCode, fs[1].LoginCalls(), fs[2].LoginCalls())
	}
}

func TestLogin_Site1Non401ClientErrorStops(t *testing.T) {
	for _, code := range []int{400, 403, 422, 500} {
		fs := fakes()
		fs[0].LoginStatus = code
		e := setup(t, fs, 5*time.Second)
		r := e.a.Login(context.Background(), "jdoe", "pw")
		if r.OK || r.Sites[0].Status != auth.StatusRejected || fs[1].LoginCalls() != 0 || fs[2].LoginCalls() != 0 {
			t.Fatalf("code %d: %v calls=%d,%d", code, statuses(r), fs[1].LoginCalls(), fs[2].LoginCalls())
		}
	}
}

func TestLogin_Site1DownFallsBack(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := strings.Replace(closed.URL, "http://", "https://", 1)
	closed.Close()
	t.Run("closed", func(t *testing.T) {
		fs := fakes()
		e := setup(t, []*ranchertest.Fake{nil, fs[1], fs[2]}, 5*time.Second, closedURL)
		r := e.a.Login(context.Background(), "jdoe", "pw")
		if !r.OK || !eq(statuses(r), auth.StatusUnavailable, auth.StatusOK, auth.StatusOK) {
			t.Fatalf("got %v", statuses(r))
		}
	})
	for _, code := range []int{502, 503, 504} {
		fs := fakes()
		fs[0].LoginStatus = code
		e := setup(t, fs, 5*time.Second)
		r := e.a.Login(context.Background(), "jdoe", "pw")
		if !r.OK || !eq(statuses(r), auth.StatusUnavailable, auth.StatusOK, auth.StatusOK) || r.Sites[0].HTTPCode != code {
			t.Fatalf("code %d: %v", code, statuses(r))
		}
		if fs[1].LoginCalls() != 1 || fs[2].LoginCalls() != 1 {
			t.Fatalf("calls %d,%d", fs[1].LoginCalls(), fs[2].LoginCalls())
		}
	}
}

func TestLogin_AllUnavailable(t *testing.T) {
	fs := fakes()
	for _, f := range fs {
		f.LoginStatus = 503
	}
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if r.OK || !eq(statuses(r), auth.StatusUnavailable, auth.StatusUnavailable, auth.StatusUnavailable) {
		t.Fatalf("got %v", statuses(r))
	}
}

func TestLogin_Site2RejectsIsDrift(t *testing.T) {
	fs := fakes()
	fs[1].Users["jdoe"] = user("other", principal)
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if !r.OK || !eq(statuses(r), auth.StatusOK, auth.StatusRejected, auth.StatusOK) {
		t.Fatalf("got %v", statuses(r))
	}
	if len(r.Warnings) == 0 || !strings.Contains(strings.Join(r.Warnings, " "), "site-2") {
		t.Fatalf("warnings %v", r.Warnings)
	}
	if !strings.Contains(e.logs.String(), "site_credential_drift") {
		t.Fatalf("logs: %s", e.logs.String())
	}
}

func TestLogin_HangingSiteTimesOut(t *testing.T) {
	fs := fakes()
	fs[2].Delay = 3 * time.Second
	e := setup(t, fs, 300*time.Millisecond)
	start := time.Now()
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if time.Since(start) > 1300*time.Millisecond {
		t.Fatalf("took %v", time.Since(start))
	}
	if !r.OK || !eq(statuses(r), auth.StatusOK, auth.StatusOK, auth.StatusUnavailable) {
		t.Fatalf("got %v", statuses(r))
	}
}

func TestLogin_PrincipalMismatchWarns(t *testing.T) {
	fs := fakes()
	fs[1].Users["jdoe"] = user("pw", strings.Replace(principal, "CN=", "cn=", 1))
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if r.PrincipalID != principal || !strings.Contains(strings.Join(r.Warnings, " "), "principal ID differs") {
		t.Fatalf("principal %q warnings %v", r.PrincipalID, r.Warnings)
	}
	if !strings.Contains(e.logs.String(), "principal_mismatch") {
		t.Fatal("no principal_mismatch log")
	}
}

func TestLogin_NoADPrincipalWarns(t *testing.T) {
	fs := fakes()
	for _, f := range fs {
		f.Users["jdoe"] = ranchertest.FakeUser{Password: "pw", ID: "u-1", PrincipalIDs: []string{"local://u-1"}}
	}
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if !r.OK || r.PrincipalID != "" || !strings.Contains(strings.Join(r.Warnings, " "), "no activedirectory_user principal") {
		t.Fatalf("got %+v", r)
	}
}

func TestLogin_MeFailureStillOK(t *testing.T) {
	fs := fakes()
	fs[1].MeStatus = 403
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if !r.OK || r.Sites[1].Status != auth.StatusOK || r.Sites[1].User != nil || r.Sites[1].MeError == "" {
		t.Fatalf("got %+v", r.Sites[1])
	}
}

func TestLogin_LogsDoNotContainSecrets(t *testing.T) {
	e := setup(t, fakes(), 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "s3cretPassw0rd")
	_ = r
	r = e.a.Login(context.Background(), "jdoe", "pw")
	logs := e.logs.String()
	if strings.Contains(logs, "s3cretPassw0rd") || strings.Contains(logs, `"pw"`) {
		t.Fatal("password in logs")
	}
	for _, s := range r.Sites {
		_, secret, _ := strings.Cut(s.Token.Bearer, ":")
		if secret == "" || strings.Contains(logs, secret) {
			t.Fatalf("bearer secret in logs or empty: %q", secret)
		}
	}
	if !strings.Contains(logs, "token_id") {
		t.Fatal("token_id missing from logs")
	}
}

func TestLogout_AllRevoked(t *testing.T) {
	fs := fakes()
	fs[1].Users["jdoe"] = user("other", principal)
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	out := e.a.Logout(context.Background(), r.Sites)
	if len(out) != 2 {
		t.Fatalf("want results only for ok sites, got %+v", out)
	}
	for _, lr := range out {
		if !lr.LoggedOut || !lr.Revoked || lr.Error != "" {
			t.Fatalf("got %+v", lr)
		}
	}
	if !strings.Contains(e.logs.String(), `"event":"logout"`) {
		t.Fatal("no logout log")
	}
}

func TestLogin_Site1BadResponseStops(t *testing.T) {
	fs := fakes()
	fs[0].LoginStatus = 200
	fs[0].LoginBody = "<html>not json</html>"
	e := setup(t, fs, 5*time.Second)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	if r.OK || r.Sites[0].Status != auth.StatusBadResponse || fs[1].LoginCalls() != 0 || fs[2].LoginCalls() != 0 {
		t.Fatalf("got %v calls=%d,%d", statuses(r), fs[1].LoginCalls(), fs[2].LoginCalls())
	}
}

func TestLogin_NonUnauthorizedBodyLogged(t *testing.T) {
	fs := fakes()
	fs[0].LoginStatus = 500
	fs[0].LoginBody = "boom-detail"
	e := setup(t, fs, 5*time.Second)
	e.a.Login(context.Background(), "jdoe", "pw")
	if !strings.Contains(e.logs.String(), "boom-detail") {
		t.Fatalf("500 body not logged: %s", e.logs.String())
	}
}

func TestLogin_UnauthorizedBodyNotLogged(t *testing.T) {
	fs := fakes()
	fs[0].LoginStatus = 401
	fs[0].LoginBody = "nope-detail"
	e := setup(t, fs, 5*time.Second)
	e.a.Login(context.Background(), "jdoe", "pw")
	if strings.Contains(e.logs.String(), "nope-detail") {
		t.Fatal("401 body logged")
	}
}

func TestLogout_RetriesRevocationCheck(t *testing.T) {
	fs := fakes()
	for _, f := range fs {
		f.RevokeLag = 2
	}
	e := setup(t, fs, 5*time.Second)
	e.a.SetRevokeRetry(5, 10*time.Millisecond)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	for _, lr := range e.a.Logout(context.Background(), r.Sites) {
		if !lr.Revoked || lr.Attempts != 3 || lr.Error != "" {
			t.Fatalf("got %+v", lr)
		}
	}
}

func TestLogout_TokenStillAccepted(t *testing.T) {
	fs := fakes()
	fs[0].IgnoreLogout = true
	e := setup(t, fs, 5*time.Second)
	e.a.SetRevokeRetry(3, 10*time.Millisecond)
	r := e.a.Login(context.Background(), "jdoe", "pw")
	out := e.a.Logout(context.Background(), r.Sites)
	if out[0].Revoked || out[0].Attempts != 3 || !strings.Contains(out[0].Error, "still accepted") {
		t.Fatalf("got %+v", out[0])
	}
}
