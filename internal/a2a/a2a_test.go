package a2a_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

var kcfg = config.KagentConfig{Namespace: "kagent", Service: "http:kagent-controller:8083", AgentNamespace: "kagent", AgentName: "my-agent"}

func TestPath(t *testing.T) {
	want := "/k8s/clusters/c-abc12/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/kagent/my-agent/"
	if got := a2a.Path("c-abc12", kcfg); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestPath_MatchesCardPath(t *testing.T) {
	f := &ranchertest.Fake{
		Users:    map[string]ranchertest.FakeUser{"jdoe": {ID: "u-1", Password: "pw"}},
		Clusters: []ranchertest.FakeCluster{{ID: "c-abc12", State: "active"}},
	}
	u, ca := ranchertest.NewTLS(t, f)
	c, err := rancher.NewClient(config.Site{Name: "s", URL: u, CAFile: ca}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AgentCard(context.Background(), tok.Bearer, "c-abc12", kcfg.Namespace, kcfg.Service, kcfg.AgentNamespace, kcfg.AgentName); err != nil {
		t.Fatal(err)
	}
	if got, want := a2a.Path("c-abc12", kcfg)+".well-known/agent-card.json", f.LastCardPath(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestStreamRequest(t *testing.T) {
	ctxID := a2a.NewUUID()
	b, err := json.Marshal(a2a.StreamRequest("hello", ctxID))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Message struct {
				Kind      string `json:"kind"`
				MessageID string `json:"messageId"`
				ContextID string `json:"contextId"`
				Role      string `json:"role"`
				Parts     []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	m := r.Params.Message
	if r.JSONRPC != "2.0" || r.Method != "message/stream" || m.Role != "user" || m.Kind != "message" ||
		m.ContextID != ctxID || len(m.Parts) != 1 || m.Parts[0].Kind != "text" || m.Parts[0].Text != "hello" {
		t.Fatalf("body = %s", b)
	}
	if !a2a.ValidUUID(r.ID) || !a2a.ValidUUID(m.MessageID) {
		t.Fatalf("ids: %q %q", r.ID, m.MessageID)
	}
	b2, _ := json.Marshal(a2a.StreamRequest("hello", ctxID))
	var r2 struct {
		Params struct {
			Message struct {
				MessageID string `json:"messageId"`
			} `json:"message"`
		} `json:"params"`
	}
	_ = json.Unmarshal(b2, &r2)
	if r2.Params.Message.MessageID == m.MessageID {
		t.Fatal("messageId must differ between calls")
	}
}

func TestValidUUID(t *testing.T) {
	if id := a2a.NewUUID(); !a2a.ValidUUID(id) {
		t.Fatalf("NewUUID %q invalid", id)
	}
	for _, s := range []string{"", "x", "../", "123E4567-E89B-12D3-A456-426614174000", "123e4567-e89b-12d3-a456-42661417400g", "123e4567-e89b-12d3-a456-4266141740000"} {
		if a2a.ValidUUID(s) {
			t.Fatalf("%q should be invalid", s)
		}
	}
}
