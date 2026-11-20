// Package ranchertest provides a fake Rancher site that implements just the
// endpoints the portal uses, returning the login shape documented for
// v2.13/v2.14 (token, id, expiresAt, type; no userId): /v1-public/login, /v3/users?me=true and
// /v3/tokens?action=logout. It backs unit tests and cmd/fakerancher.
package ranchertest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// FakeUser is a user the fake accepts at login.
type FakeUser struct {
	Password     string
	ID           string
	PrincipalIDs []string
}

// Fake is a fake Rancher site. Set fields before serving; the knobs may be
// changed later only under the caller's own synchronisation.
type Fake struct {
	Users        map[string]FakeUser // keyed by username
	LoginStatus  int                 // if non-zero, login always returns this code
	LoginBody    string              // body sent with LoginStatus
	MeStatus     int                 // if non-zero, /v3/users?me=true always returns this code
	Delay        time.Duration       // sleep before every response
	Redirect     string              // if set, every request gets a 307 to Redirect+path
	RevokeLag    int                 // after logout, the token still works for this many /v3/users calls
	IgnoreLogout bool                // logout returns 200 but the token stays valid
	Log          func(format string, args ...any)

	mu         sync.Mutex
	tokens     map[string]string // bearer -> username
	lag        map[string]int    // bearer -> remaining successful calls after logout
	loginCalls int
	lastBody   map[string]any
	lastCT     string
}

// Handler returns the fake's HTTP handler.
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1-public/login", f.login)
	mux.HandleFunc("GET /v3/users", f.me)
	mux.HandleFunc("POST /v3/tokens", f.logout)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.Delay > 0 {
			select {
			case <-time.After(f.Delay):
			case <-r.Context().Done():
				return
			}
		}
		if f.Redirect != "" {
			http.Redirect(w, r, f.Redirect+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// LoginCalls returns how many login requests the fake received.
func (f *Fake) LoginCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCalls
}

// LastLoginBody returns the decoded body of the most recent login request.
func (f *Fake) LastLoginBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

// LastLoginContentType returns the Content-Type of the most recent login request.
func (f *Fake) LastLoginContentType() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCT
}

func (f *Fake) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) login(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	err := json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.loginCalls++
	f.lastBody = body
	f.lastCT = r.Header.Get("Content-Type")
	f.mu.Unlock()
	if f.LoginStatus != 0 {
		f.logf("login -> %d (forced)", f.LoginStatus)
		http.Error(w, f.LoginBody, f.LoginStatus)
		return
	}
	if err != nil || body["type"] != "activeDirectoryProvider" || body["responseType"] != "json" {
		f.logf("login -> 400 (bad body)")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username, _ := body["username"].(string)
	password, _ := body["password"].(string)
	u, ok := f.Users[username]
	if !ok || u.Password != password {
		f.logf("login user=%q -> 401", username)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"type": "error", "status": "401", "message": "must authenticate"})
		return
	}
	ttl, _ := body["ttl"].(float64)
	name := "token-" + randHex(3)[:5]
	bearer := name + ":" + randHex(24)
	f.mu.Lock()
	if f.tokens == nil {
		f.tokens = map[string]string{}
	}
	f.tokens[bearer] = username
	f.mu.Unlock()
	f.logf("login user=%q -> 200 %s", username, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"type":      "token",
		"id":        name,
		"token":     bearer,
		"expiresAt": time.Now().Add(time.Duration(ttl) * time.Millisecond).UTC().Format(time.RFC3339),
	})
}

func (f *Fake) userFor(r *http.Request) (string, bool) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.tokens[bearer]
	if n, lagging := f.lag[bearer]; ok && lagging {
		if n <= 0 {
			delete(f.tokens, bearer)
			delete(f.lag, bearer)
			return "", false
		}
		f.lag[bearer] = n - 1
	}
	return name, ok
}

func (f *Fake) me(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("me") != "true" {
		http.Error(w, "only me=true supported", http.StatusBadRequest)
		return
	}
	if f.MeStatus != 0 {
		http.Error(w, "forced", f.MeStatus)
		return
	}
	name, ok := f.userFor(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	u := f.Users[name]
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "collection",
		"data": []map[string]any{{"id": u.ID, "username": name, "principalIds": u.PrincipalIDs}},
	})
}

func (f *Fake) logout(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("action") != "logout" {
		http.Error(w, "only action=logout supported", http.StatusBadRequest)
		return
	}
	if _, ok := f.userFor(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	switch {
	case f.IgnoreLogout:
	case f.RevokeLag > 0:
		if f.lag == nil {
			f.lag = map[string]int{}
		}
		f.lag[bearer] = f.RevokeLag
	default:
		delete(f.tokens, bearer)
	}
	f.mu.Unlock()
	f.logf("logout -> 200")
	w.WriteHeader(http.StatusOK)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartTLS serves f over TLS with a fresh self-signed certificate (valid for
// 127.0.0.1 and localhost), so each fake site has its own CA. If addr is
// empty a random local port is used.
func StartTLS(f *Fake, addr string) (*httptest.Server, error) {
	cert, err := selfSigned()
	if err != nil {
		return nil, err
	}
	srv := httptest.NewUnstartedServer(f.Handler())
	if addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		srv.Listener.Close()
		srv.Listener = ln
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	return srv, nil
}

// WriteCA writes the server's certificate as a PEM file.
func WriteCA(srv *httptest.Server, path string) error {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return os.WriteFile(path, pemBytes, 0o600)
}

// NewTLS serves f over TLS for the duration of the test and returns the
// server URL and the path of a PEM file holding its certificate.
func NewTLS(t testing.TB, f *Fake) (url, caFile string) {
	t.Helper()
	srv, err := StartTLS(f, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := WriteCA(srv, caFile); err != nil {
		t.Fatal(err)
	}
	return srv.URL, caFile
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "fake-rancher"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
