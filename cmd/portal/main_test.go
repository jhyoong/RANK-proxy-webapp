package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

func TestRun_StartsAndServesHealthz(t *testing.T) {
	u, ca := ranchertest.NewTLS(t, &ranchertest.Fake{})
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "listen: 127.0.0.1:0\nkagent:\n  agent_name: test-agent\nsites:\n  - name: site-1\n    url: " + u + "\n    ca_file: " + ca + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", cfgPath}, io.Discard, ready) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "ok") {
		t.Fatalf("healthz %d %q", resp.StatusCode, b)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not shut down")
	}
}

func TestRun_BadConfig(t *testing.T) {
	err := run(context.Background(), []string{"-config", "/nonexistent.yaml"}, io.Discard, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_ShutdownRevokesSessions(t *testing.T) {
	f := &ranchertest.Fake{Users: map[string]ranchertest.FakeUser{
		"jdoe": {Password: "pw", ID: "u-1", PrincipalIDs: []string{"local://u-1", "activedirectory_user://CN=Jdoe"}},
	}}
	u, ca := ranchertest.NewTLS(t, f)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "listen: 127.0.0.1:0\ncookie_secure: false\nkagent:\n  agent_name: test-agent\nsites:\n  - name: site-1\n    url: " + u + "\n    ca_file: " + ca + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", cfgPath}, io.Discard, ready) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm("http://"+addr+"/login", url.Values{"username": {"jdoe"}, "password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/home" {
		t.Fatalf("login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if f.LogoutCalls() != 0 {
		t.Fatal("logout before shutdown")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("did not shut down")
	}
	if f.LogoutCalls() != 1 {
		t.Fatalf("logout calls = %d, want 1", f.LogoutCalls())
	}
}

func TestRun_ServerLimits(t *testing.T) {
	u, ca := ranchertest.NewTLS(t, &ranchertest.Fake{})
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "listen: 127.0.0.1:0\nkagent:\n  agent_name: test-agent\nsites:\n  - name: site-1\n    url: " + u + "\n    ca_file: " + ca + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", cfgPath}, io.Discard, ready) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	// Go allows 4096 bytes of slack over MaxHeaderBytes.
	req, _ := http.NewRequest("GET", "http://"+addr+"/healthz", nil)
	req.Header.Set("X-Big", strings.Repeat("a", 80<<10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status %d, want 431", resp.StatusCode)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no shutdown")
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// startupLog runs the portal with extra config appended, waits until it is
// listening, stops it and returns what it logged.
func startupLog(t *testing.T, extra string) string {
	t.Helper()
	u, ca := ranchertest.NewTLS(t, &ranchertest.Fake{})
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "listen: 127.0.0.1:0\nkagent:\n  agent_name: test-agent\nsites:\n  - name: site-1\n    url: " + u + "\n    ca_file: " + ca + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out lockedBuf
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", cfgPath}, &out, ready) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not shut down")
	}
	return out.String()
}

func TestRun_WarnsWhenTrustedProxiesEmpty(t *testing.T) {
	const warn = "login.trusted_proxies is empty"
	if out := startupLog(t, ""); !strings.Contains(out, warn) || !strings.Contains(out, `"event":"config_warning"`) ||
		!strings.Contains(out, `"level":"WARN"`) {
		t.Fatalf("no warning: %s", out)
	}
	if out := startupLog(t, "cookie_secure: false\n"); strings.Contains(out, warn) {
		t.Fatalf("warned with insecure cookies: %s", out)
	}
	if out := startupLog(t, "login:\n  trusted_proxies: [\"10.0.0.0/8\"]\n"); strings.Contains(out, warn) {
		t.Fatalf("warned with trusted proxies set: %s", out)
	}
}
