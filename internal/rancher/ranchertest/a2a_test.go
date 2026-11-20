package ranchertest_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

const chatPath = "/k8s/clusters/c-1/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/kagent/k8s-agent/"

type chatEnv struct {
	f      *ranchertest.Fake
	base   string
	client *http.Client
	token  string
}

func newChatEnv(t *testing.T, f *ranchertest.Fake) *chatEnv {
	t.Helper()
	f.Users = map[string]ranchertest.FakeUser{"alice": {Password: "pw", ID: "u-1"}}
	if f.Clusters == nil {
		f.Clusters = []ranchertest.FakeCluster{{ID: "c-1", DisplayName: "one", State: "active"}}
	}
	base, ca := ranchertest.NewTLS(t, f)
	pem, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := c.Post(base+"/v1-public/login", "application/json",
		strings.NewReader(`{"type":"activeDirectoryProvider","responseType":"json","username":"alice","password":"pw","ttl":600000}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var lr struct{ Token string }
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil || lr.Token == "" {
		t.Fatalf("login: %v %q", err, lr.Token)
	}
	return &chatEnv{f: f, base: base, client: c, token: lr.Token}
}

func (e *chatEnv) post(ctx context.Context, t *testing.T, ctxID, text, method string) (*http.Response, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": method,
		"params": map[string]any{"message": map[string]any{
			"kind": "message", "messageId": "m", "contextId": ctxID, "role": "user",
			"parts": []any{map[string]any{"kind": "text", "text": text}},
		}},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", e.base+chatPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return e.client.Do(req)
}

func readEvents(t *testing.T, resp *http.Response) []string {
	t.Helper()
	defer resp.Body.Close()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestFakeChat_DefaultStreams(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{})
	resp, err := e.post(context.Background(), t, "ctx-1", `say "hi"`, "message/stream")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	evs := readEvents(t, resp)
	if len(evs) != 8 {
		t.Fatalf("got %d events: %v", len(evs), evs)
	}
	for _, ev := range evs {
		if !json.Valid([]byte(ev)) {
			t.Fatalf("invalid JSON: %s", ev)
		}
	}
	var art struct {
		Result struct {
			Kind     string
			Artifact struct{ Parts []struct{ Text string } }
		}
	}
	if err := json.Unmarshal([]byte(evs[6]), &art); err != nil {
		t.Fatal(err)
	}
	if art.Result.Kind != "artifact-update" || art.Result.Artifact.Parts[0].Text != `Answer to "say "hi"" (turn 1)` {
		t.Fatalf("artifact: %s", evs[6])
	}
	if !strings.Contains(evs[7], `"final":true`) || !strings.Contains(evs[7], `"completed"`) {
		t.Fatalf("last: %s", evs[7])
	}
	resp2, err := e.post(context.Background(), t, "ctx-1", "again", "message/stream")
	if err != nil {
		t.Fatal(err)
	}
	if evs := readEvents(t, resp2); !strings.Contains(evs[6], "turn 2") {
		t.Fatalf("second turn: %s", evs[6])
	}
	if !strings.Contains(evs[0], `say \"hi\"`) || !strings.Contains(evs[0], `"role":"user"`) {
		t.Fatalf("echo: %s", evs[0])
	}
	if !strings.Contains(evs[4], "(turn 1)") {
		t.Fatalf("partial: %s", evs[4])
	}
	if e.f.ChatCalls() != 2 {
		t.Fatalf("ChatCalls = %d", e.f.ChatCalls())
	}
	if e.f.LastChatBody()["method"] != "message/stream" {
		t.Fatalf("LastChatBody = %v", e.f.LastChatBody())
	}
	u := e.f.LastChatURL()
	if u == nil || u.Query().Has("user_id") || !strings.HasSuffix(u.Path, "/") {
		t.Fatalf("LastChatURL = %v", u)
	}
	h := e.f.LastChatHeaders()
	if h.Get("Authorization") == "" || h.Get("X-User-Id") != "" {
		t.Fatalf("headers = %v", h)
	}
}

func TestFakeChat_Deny403(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{Clusters: []ranchertest.FakeCluster{
		{ID: "c-1", Deny: map[string]bool{"alice": true}}}})
	resp, err := e.post(context.Background(), t, "c", "x", "message/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestFakeChat_Status(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{Clusters: []ranchertest.FakeCluster{{ID: "c-1", ChatStatus: 503}}})
	resp, err := e.post(context.Background(), t, "c", "x", "message/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestFakeChat_BadMethod400(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{})
	resp, err := e.post(context.Background(), t, "c", "x", "message/send")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestFakeChat_HangCancelled(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{Clusters: []ranchertest.FakeCluster{{ID: "c-1", ChatHang: true}}})
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := e.post(ctx, t, "c", "x", "message/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	deadline := time.Now().Add(time.Second)
	for e.f.ChatCancelled() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.f.ChatCancelled(); got != 1 {
		t.Fatalf("ChatCancelled = %d", got)
	}
}
