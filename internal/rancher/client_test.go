package rancher_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

const principal = "activedirectory_user://CN=Test User (Admin), OU=Example Admins,DC=example,DC=com"

func newFake() *ranchertest.Fake {
	return &ranchertest.Fake{Users: map[string]ranchertest.FakeUser{
		"jdoe": {Password: "pw", ID: "u-abc", PrincipalIDs: []string{"local://u-abc", principal}},
	}}
}

func newClient(t *testing.T, f *ranchertest.Fake, timeout time.Duration) *rancher.Client {
	t.Helper()
	u, ca := ranchertest.NewTLS(t, f)
	c, err := rancher.NewClient(config.Site{Name: "site-1", URL: u, CAFile: ca}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLogin_Success(t *testing.T) {
	f := newFake()
	c := newClient(t, f, 5*time.Second)
	tok, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Bearer == "" || !strings.HasPrefix(tok.Bearer, tok.ID+":") || tok.ExpiresAt == "" {
		t.Fatalf("bad token: %+v", tok)
	}
	body := f.LastLoginBody()
	if body["ttl"] != float64(3600000) || body["type"] != "activeDirectoryProvider" || body["responseType"] != "json" {
		t.Fatalf("bad login body: %v", body)
	}
	if f.LastLoginContentType() != "application/json" {
		t.Fatalf("content-type = %q", f.LastLoginContentType())
	}
}

func TestLogin_Rejected(t *testing.T) {
	c := newClient(t, newFake(), 5*time.Second)
	_, err := c.Login(context.Background(), "jdoe", "wrong", time.Hour)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("want StatusError 401, got %v", err)
	}
}

func TestLogin_UsernamePassedVerbatim(t *testing.T) {
	f := newFake()
	c := newClient(t, f, 5*time.Second)
	for _, u := range []string{`ORG\jdoe`, "Test User (Admin)", "jdoe@example.com", "Tân"} {
		_, _ = c.Login(context.Background(), u, "x", time.Hour)
		if got := f.LastLoginBody()["username"]; got != u {
			t.Fatalf("username %q arrived as %q", u, got)
		}
	}
}

func TestMe_ReturnsPrincipals(t *testing.T) {
	c := newClient(t, newFake(), 5*time.Second)
	tok, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.Me(context.Background(), tok.Bearer)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u-abc" || u.Username != "jdoe" || len(u.PrincipalIDs) != 2 || u.PrincipalIDs[1] != principal {
		t.Fatalf("bad user: %+v", u)
	}
}

func TestLogout_RevokesToken(t *testing.T) {
	c := newClient(t, newFake(), 5*time.Second)
	tok, _ := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	if err := c.Logout(context.Background(), tok.Bearer); err != nil {
		t.Fatal(err)
	}
	_, err := c.Me(context.Background(), tok.Bearer)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("want 401 after logout, got %v", err)
	}
}

func TestNewClient_UntrustedCA(t *testing.T) {
	u, _ := ranchertest.NewTLS(t, newFake())
	_, otherCA := ranchertest.NewTLS(t, newFake())
	c, err := rancher.NewClient(config.Site{Name: "s", URL: u, CAFile: otherCA}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Login(context.Background(), "jdoe", "pw", time.Hour)
	var se *rancher.StatusError
	if err == nil || errors.As(err, &se) {
		t.Fatalf("want transport error, got %v", err)
	}
}

func TestLogin_Timeout(t *testing.T) {
	f := newFake()
	f.Delay = 2 * time.Second
	c := newClient(t, f, 100*time.Millisecond)
	start := time.Now()
	_, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	var se *rancher.StatusError
	if err == nil || errors.As(err, &se) || time.Since(start) > time.Second {
		t.Fatalf("want fast transport error, got %v after %v", err, time.Since(start))
	}
}

func TestStatusError_BodyTruncated(t *testing.T) {
	f := newFake()
	f.LoginStatus = 500
	f.LoginBody = strings.Repeat("x", 1000)
	c := newClient(t, f, 5*time.Second)
	_, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 500 || len(se.Body) > 200 {
		t.Fatalf("got %v", err)
	}
}

func TestLogin_BadResponseBody(t *testing.T) {
	for _, body := range []string{"<html>login</html>", "{}"} {
		f := newFake()
		f.LoginStatus = 200
		f.LoginBody = body
		c := newClient(t, f, 5*time.Second)
		_, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
		if !errors.Is(err, rancher.ErrBadResponse) {
			t.Fatalf("body %q: want ErrBadResponse, got %v", body, err)
		}
	}
}

func TestLogin_DoesNotFollowRedirects(t *testing.T) {
	target := newFake()
	targetURL, _ := ranchertest.NewTLS(t, target)
	f := newFake()
	f.Redirect = targetURL
	c := newClient(t, f, 5*time.Second)
	_, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 307 {
		t.Fatalf("want StatusError 307, got %v", err)
	}
	if target.LoginCalls() != 0 {
		t.Fatal("redirect was followed; password re-sent")
	}
}

func TestLogin_NoUserIDInResponse(t *testing.T) {
	c := newClient(t, newFake(), 5*time.Second)
	tok, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	if err != nil || tok.UserID != "" {
		t.Fatalf("fake should follow documented shape without userId: %+v %v", tok, err)
	}
}
