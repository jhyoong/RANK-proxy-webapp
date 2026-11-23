package ranchertest_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
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

func (e *chatEnv) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.base+"/k8s/clusters/c-1/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func TestFake_ChatRecordsSession(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{})
	resp, err := e.post(context.Background(), t, "C", "please list every pod now", "message/stream")
	if err != nil {
		t.Fatal(err)
	}
	readEvents(t, resp)

	code, body := e.get(t, "api/sessions/agent/kagent/k8s-agent")
	var list struct {
		Error bool
		Data  []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			AgentID string `json:"agent_id"`
		}
	}
	if err := json.Unmarshal([]byte(body), &list); code != 200 || err != nil || list.Error {
		t.Fatalf("%d %v %s", code, err, body)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "C" || list.Data[0].Name != "please list every po..." ||
		list.Data[0].AgentID != "kagent__NS__k8s_agent" {
		t.Fatalf("list %s", body)
	}

	code, body = e.get(t, "api/sessions/C/tasks")
	var tasks struct {
		Data []struct {
			Status  struct{ State string }
			History []struct {
				Role  string
				Parts []struct {
					Kind     string
					Metadata map[string]string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &tasks); code != 200 || err != nil {
		t.Fatalf("%d %v %s", code, err, body)
	}
	if len(tasks.Data) != 1 || tasks.Data[0].Status.State != "completed" {
		t.Fatalf("tasks %s", body)
	}
	var call bool
	for _, m := range tasks.Data[0].History {
		for _, p := range m.Parts {
			call = call || (p.Kind == "data" && p.Metadata["kagent_type"] == "function_call")
		}
	}
	if !call {
		t.Fatalf("no function_call: %s", body)
	}
	if !strings.Contains(body, `Answer to \"please list every pod now\" (turn 1)`) {
		t.Fatalf("final text: %s", body)
	}

	if code, body := e.get(t, "api/sessions/nope/tasks"); code != 404 || !strings.Contains(body, `"not found"`) {
		t.Fatalf("unknown: %d %s", code, body)
	}
}

func TestFake_SessionStatus(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{Clusters: []ranchertest.FakeCluster{{ID: "c-1", SessionStatus: 403}}})
	if code, _ := e.get(t, "api/sessions/agent/kagent/k8s-agent"); code != 403 {
		t.Fatalf("code %d", code)
	}
}

func TestFake_TasksStatus(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{Clusters: []ranchertest.FakeCluster{{ID: "c-1", TasksStatus: 401,
		Sessions: []ranchertest.FakeSession{{ID: "s1"}}}}})
	if code, _ := e.get(t, "api/sessions/agent/kagent/k8s-agent"); code != 200 {
		t.Fatalf("list code %d", code)
	}
	if code, _ := e.get(t, "api/sessions/s1/tasks"); code != 401 {
		t.Fatalf("tasks code %d", code)
	}
}

// Listing clusters while chats record sessions must not race on the cluster.
func TestFake_ChatAndListConcurrent(t *testing.T) {
	e := newChatEnv(t, &ranchertest.Fake{Clusters: []ranchertest.FakeCluster{{ID: "c-1", ChatSteps: []ranchertest.Step{}}}})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				resp, err := e.post(context.Background(), t, fmt.Sprintf("ctx-%d-%d", g, i), "x", "message/stream")
				if err != nil {
					t.Error(err)
					return
				}
				readEvents(t, resp)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				req, _ := http.NewRequest("GET", e.base+"/v1/management.cattle.io.clusters", nil)
				req.Header.Set("Authorization", "Bearer "+e.token)
				resp, err := e.client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}
