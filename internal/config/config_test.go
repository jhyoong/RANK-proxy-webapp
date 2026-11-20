package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCA(t *testing.T, dir string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	ca := writeCA(t, dir)
	bad := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(bad, []byte("not a cert"), 0o600)
	body = strings.ReplaceAll(body, "CAFILE", ca)
	body = strings.ReplaceAll(body, "BADFILE", bad)
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
sites:
  - name: site-1
    url: https://r1.example
    ca_file: CAFILE
`

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || !c.SecureCookies() || c.Session.IdleTimeout != 30*time.Minute ||
		c.Session.AbsoluteTimeout != 8*time.Hour || c.Login.Timeout != 15*time.Second {
		t.Fatalf("bad defaults: %+v", c)
	}
	if len(c.Sites) != 1 || c.Sites[0].Name != "site-1" || c.Sites[0].URL != "https://r1.example" {
		t.Fatalf("bad sites: %+v", c.Sites)
	}
}

func TestLoad_KagentDefaults(t *testing.T) {
	c, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kagent.Namespace != "kagent" || c.Kagent.Service != "http:kagent-controller:8083" ||
		c.Kagent.AgentNamespace != "kagent" || c.Kagent.AgentName != "rps-agent" ||
		c.Clusters.CacheTTL != 2*time.Minute || c.API.Timeout != 15*time.Second {
		t.Fatalf("bad defaults: %+v %+v %+v", c.Kagent, c.Clusters, c.API)
	}
}

func TestLoad_Durations(t *testing.T) {
	c, err := Load(writeConfig(t, "session:\n  idle_timeout: 2m\n"+minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Session.IdleTimeout != 2*time.Minute {
		t.Fatalf("idle = %v", c.Session.IdleTimeout)
	}
}

func TestLoad_CookieSecureFalse(t *testing.T) {
	c, err := Load(writeConfig(t, "cookie_secure: false\n"+minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.SecureCookies() {
		t.Fatal("expected insecure cookies")
	}
}

func TestLoad_TrimsTrailingSlash(t *testing.T) {
	c, err := Load(writeConfig(t, strings.Replace(minimal, "r1.example", "r1.example/", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sites[0].URL != "https://r1.example" {
		t.Fatalf("url = %q", c.Sites[0].URL)
	}
}

func TestLoad_EmptyCAFileAllowed(t *testing.T) {
	_, err := Load(writeConfig(t, "sites:\n  - name: a\n    url: https://a.example\n"))
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoad_Rejects(t *testing.T) {
	site := func(name, url, ca string) string {
		return "  - name: " + name + "\n    url: " + url + "\n    ca_file: " + ca + "\n"
	}
	cases := []struct{ name, body, want string }{
		{"no sites", "listen: \":8080\"\n", "sites"},
		{"dup names", "sites:\n" + site("a", "https://a.example", "CAFILE") + site("a", "https://b.example", "CAFILE"), "duplicate"},
		{"empty name", "sites:\n" + site("\"\"", "https://a.example", "CAFILE"), "name"},
		{"http url", "sites:\n" + site("a", "http://a.example", "CAFILE"), "https"},
		{"url path", "sites:\n" + site("a", "https://a.example/rancher", "CAFILE"), "path"},
		{"ca missing", "sites:\n" + site("a", "https://a.example", "/nonexistent/ca.pem"), "ca_file"},
		{"ca no pem", "sites:\n" + site("a", "https://a.example", "BADFILE"), "ca_file"},
		{"idle > absolute", "session:\n  idle_timeout: 9h\n  absolute_timeout: 8h\n" + minimal, "idle_timeout"},
		{"half tls", "tls:\n  cert_file: /x.pem\n" + minimal, "tls"},
		{"unknown key", "bogus: 1\n" + minimal, "bogus"},
		{"agent_name slash", "kagent:\n  agent_name: a/b\n" + minimal, "agent_name"},
		{"namespace slash", "kagent:\n  namespace: a/b\n" + minimal, "namespace"},
		{"service query", "kagent:\n  service: a?b\n" + minimal, "service"},
		{"agent_namespace hash", "kagent:\n  agent_namespace: a#b\n" + minimal, "agent_namespace"},
		{"negative cache_ttl", "clusters:\n  cache_ttl: -1s\n" + minimal, "cache_ttl"},
		{"negative api timeout", "api:\n  timeout: -1s\n" + minimal, "api.timeout"},
		{"unknown clusters key", "clusters:\n  include_local: true\n" + minimal, "include_local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoad_ExampleOnlyFailsOnCA(t *testing.T) {
	_, err := Load("../../config.example.yaml")
	if err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("expected only a ca_file error, got %v", err)
	}
}
