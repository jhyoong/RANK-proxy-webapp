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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

	Clusters   []FakeCluster // served by GET /v1/management.cattle.io.clusters
	ListStatus int           // if non-zero, the cluster list returns this code
	ListBody   string        // body sent with ListStatus
	PageSize   int           // if >0, the cluster list paginates with this many items per page
	LoopPages  bool          // if true, every list page returns a continue token
	// TopLevelContinue emits the continue token at the top level of the
	// collection (as rancher/apiserver does) instead of under pagination.
	TopLevelContinue bool

	mu         sync.Mutex
	tokens     map[string]string // bearer -> username
	lag        map[string]int    // bearer -> remaining successful calls after logout
	loginCalls int
	lastBody   map[string]any
	lastCT     string
	listCalls  int
	lastSSAR   map[string]any
	lastCard   string
	ssarCalls  int
	cardCalls  int
}

// Condition is a Rancher status condition. It is defined here, not imported
// from package rancher, because rancher's tests import this package.
type Condition struct{ Type, Status, Reason string }

// FakeCluster is a cluster the fake lists.
type FakeCluster struct {
	ID, DisplayName, State string
	Conditions             []Condition
	Deny                   map[string]bool // username -> SSAR allowed=false
	CardStatus             int             // 0 -> 200 with a valid card
	CardBody               string          // overrides the card body
	CardDelay              time.Duration   // sleep before answering the card
	SSARStatus             int             // if non-zero, the access review returns this code
}

// Handler returns the fake's HTTP handler.
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1-public/login", f.login)
	mux.HandleFunc("GET /v3/users", f.me)
	mux.HandleFunc("POST /v3/tokens", f.logout)
	mux.HandleFunc("GET /v1/management.cattle.io.clusters", f.listClusters)
	mux.HandleFunc("/k8s/clusters/", f.k8sProxy)
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

// ListCalls returns how many cluster list requests the fake received.
func (f *Fake) ListCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// SSARCalls returns how many access review requests reached a known cluster.
func (f *Fake) SSARCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ssarCalls
}

// CardCalls returns how many agent card requests reached a known cluster.
func (f *Fake) CardCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cardCalls
}

// LastSSAR returns the decoded body of the most recent access review request.
func (f *Fake) LastSSAR() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSSAR
}

// LastCardPath returns the escaped request path of the most recent agent card request.
func (f *Fake) LastCardPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCard
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

func (f *Fake) listClusters(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	if f.ListStatus != 0 {
		w.WriteHeader(f.ListStatus)
		_, _ = w.Write([]byte(f.ListBody))
		return
	}
	if _, ok := f.userFor(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	start := 0
	if tok := q.Get("continue"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 || n > len(f.Clusters) {
			http.Error(w, "bad continue token", http.StatusBadRequest)
			return
		}
		start = n
	}
	size := f.PageSize
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && (size == 0 || l < size) {
		size = l
	}
	end := len(f.Clusters)
	if size > 0 && start+size < end {
		end = start + size
	}
	data := make([]map[string]any, 0, end-start)
	for _, c := range f.Clusters[start:end] {
		conds := make([]map[string]string, 0, len(c.Conditions))
		for _, cd := range c.Conditions {
			conds = append(conds, map[string]string{"type": cd.Type, "status": cd.Status, "reason": cd.Reason})
		}
		data = append(data, map[string]any{
			"id":       c.ID,
			"metadata": map[string]any{"name": c.ID, "state": map[string]any{"name": c.State}},
			"spec":     map[string]any{"displayName": c.DisplayName},
			"status":   map[string]any{"conditions": conds},
		})
	}
	pg := map[string]any{}
	if end < len(f.Clusters) {
		pg["continue"] = strconv.Itoa(end)
	} else if f.LoopPages {
		pg["continue"] = strconv.Itoa(start)
	}
	if _, ok := pg["continue"]; ok {
		// Steve also sends a next URL; the client must ignore it.
		pg["next"] = "https://unreachable.invalid/v1/management.cattle.io.clusters?continue=x"
	}
	out := map[string]any{"data": data, "pagination": pg}
	if f.TopLevelContinue {
		if tok, ok := pg["continue"]; ok {
			delete(pg, "continue")
			out["continue"] = tok
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// k8sProxy serves /k8s/clusters/{id}/... It matches on the escaped path by
// hand so that an ID containing %2F is neither cleaned nor split by the mux.
func (f *Fake) k8sProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/k8s/clusters/")
	idEsc, tail, _ := strings.Cut(rest, "/")
	id, err := url.PathUnescape(idEsc)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	user, ok := f.userFor(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var cl *FakeCluster
	for i := range f.Clusters {
		if f.Clusters[i].ID == id {
			cl = &f.Clusters[i]
		}
	}
	if cl == nil {
		http.NotFound(w, r)
		return
	}
	segs := strings.Split(tail, "/")
	switch {
	case r.Method == http.MethodPost && tail == "apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastSSAR = body
		f.ssarCalls++
		f.mu.Unlock()
		if cl.SSARStatus != 0 {
			http.Error(w, "forced", cl.SSARStatus)
			return
		}
		allowed := !cl.Deny[user]
		status := map[string]any{"allowed": allowed}
		if !allowed {
			status["reason"] = "RBAC: access denied for " + user
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "status": status,
		})
	case r.Method == http.MethodGet && len(segs) == 13 &&
		segs[0] == "api" && segs[1] == "v1" && segs[2] == "namespaces" && segs[4] == "services" &&
		segs[6] == "proxy" && segs[7] == "api" && segs[8] == "a2a" &&
		segs[11] == ".well-known" && segs[12] == "agent-card.json":
		f.mu.Lock()
		f.lastCard = r.URL.EscapedPath()
		f.cardCalls++
		f.mu.Unlock()
		if cl.CardDelay > 0 {
			select {
			case <-time.After(cl.CardDelay):
			case <-r.Context().Done():
				return
			}
		}
		if cl.CardStatus != 0 {
			w.WriteHeader(cl.CardStatus)
			return
		}
		if cl.CardBody != "" {
			_, _ = w.Write([]byte(cl.CardBody))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name": "rps-agent", "description": "fake agent", "version": "1.0.0",
			"capabilities": map[string]any{"streaming": true},
		})
	default:
		http.NotFound(w, r)
	}
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
