package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

func TestRun_StartsAndServesHealthz(t *testing.T) {
	u, ca := ranchertest.NewTLS(t, &ranchertest.Fake{})
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "listen: 127.0.0.1:0\nsites:\n  - name: site-1\n    url: " + u + "\n    ca_file: " + ca + "\n"
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
