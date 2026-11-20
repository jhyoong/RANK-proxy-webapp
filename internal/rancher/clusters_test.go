package rancher_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

func listSetup(t *testing.T, f *ranchertest.Fake) (*rancher.Client, string) {
	t.Helper()
	c := newClient(t, f, 5*time.Second)
	tok, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return c, tok.Bearer
}

func TestListClusters_Fields(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{
		{ID: "c-aaa11", DisplayName: "prod", State: "active", Conditions: []ranchertest.Condition{
			{Type: "Connected", Status: "True"}, {Type: "Ready", Status: "True"}, {Type: "Other", Status: "True"}}},
		{ID: "c-bbb22", DisplayName: "dev", State: "unavailable", Conditions: []ranchertest.Condition{
			{Type: "Connected", Status: "False", Reason: "Disconnected"}}},
	}
	c, bearer := listSetup(t, f)
	got, err := c.ListClusters(context.Background(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d clusters", len(got))
	}
	a, b := got[0], got[1]
	if a.ID != "c-aaa11" || a.DisplayName != "prod" || a.State != "active" || !a.Connected {
		t.Fatalf("bad cluster a: %+v", a)
	}
	if len(a.Conditions) != 2 || a.Conditions[0] != (rancher.Condition{Type: "Connected", Status: "True"}) ||
		a.Conditions[1] != (rancher.Condition{Type: "Ready", Status: "True"}) {
		t.Fatalf("bad conditions a: %+v", a.Conditions)
	}
	if b.ID != "c-bbb22" || b.DisplayName != "dev" || b.State != "unavailable" || b.Connected {
		t.Fatalf("bad cluster b: %+v", b)
	}
	if len(b.Conditions) != 1 || b.Conditions[0] != (rancher.Condition{Type: "Connected", Status: "False", Reason: "Disconnected"}) {
		t.Fatalf("bad conditions b: %+v", b.Conditions)
	}
	// The fake only answers a bearer it issued, so success proves the header.
	if _, err := c.ListClusters(context.Background(), "bogus"); err == nil {
		t.Fatal("unknown bearer must fail")
	}
}

func TestListClusters_ConnectedFallback(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{
		{ID: "c-1", State: "active"},
		{ID: "c-2", State: "unavailable"},
	}
	c, bearer := listSetup(t, f)
	got, err := c.ListClusters(context.Background(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Connected || got[1].Connected {
		t.Fatalf("fallback wrong: %+v", got)
	}
}

func TestListClusters_DisplayNameFallback(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{{ID: "c-1", State: "active"}}
	c, bearer := listSetup(t, f)
	got, err := c.ListClusters(context.Background(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].DisplayName != "c-1" {
		t.Fatalf("DisplayName = %q", got[0].DisplayName)
	}
}

func TestListClusters_Pagination(t *testing.T) {
	f := newFake()
	for _, id := range []string{"c-1", "c-2", "c-3", "c-4", "c-5"} {
		f.Clusters = append(f.Clusters, ranchertest.FakeCluster{ID: id, State: "active"})
	}
	f.PageSize = 2
	c, bearer := listSetup(t, f)
	got, err := c.ListClusters(context.Background(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d clusters", len(got))
	}
	for i, id := range []string{"c-1", "c-2", "c-3", "c-4", "c-5"} {
		if got[i].ID != id {
			t.Fatalf("order wrong at %d: %+v", i, got)
		}
	}
	if f.ListCalls() != 3 {
		t.Fatalf("list calls = %d", f.ListCalls())
	}
}

func TestListClusters_PaginationTopLevelContinue(t *testing.T) {
	f := newFake()
	for _, id := range []string{"c-1", "c-2", "c-3", "c-4", "c-5"} {
		f.Clusters = append(f.Clusters, ranchertest.FakeCluster{ID: id, State: "active"})
	}
	f.PageSize = 2
	f.TopLevelContinue = true
	c, bearer := listSetup(t, f)
	got, err := c.ListClusters(context.Background(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d clusters", len(got))
	}
	for i, id := range []string{"c-1", "c-2", "c-3", "c-4", "c-5"} {
		if got[i].ID != id {
			t.Fatalf("order wrong at %d: %+v", i, got)
		}
	}
}

func TestListClusters_PaginationCap(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{{ID: "c-1", State: "active"}}
	f.LoopPages = true
	c, bearer := listSetup(t, f)
	_, err := c.ListClusters(context.Background(), bearer)
	if err == nil || !strings.Contains(err.Error(), "50 pages") {
		t.Fatalf("want 50 pages error, got %v", err)
	}
	if f.ListCalls() != 50 {
		t.Fatalf("list calls = %d, want 50", f.ListCalls())
	}
}

func TestListClusters_Unauthorized(t *testing.T) {
	f := newFake()
	f.ListStatus = 401
	c, bearer := listSetup(t, f)
	_, err := c.ListClusters(context.Background(), bearer)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("want StatusError 401, got %v", err)
	}
}

func TestListClusters_BadJSON(t *testing.T) {
	f := newFake()
	f.ListStatus = 200
	f.ListBody = "<html>not json</html>"
	c, bearer := listSetup(t, f)
	_, err := c.ListClusters(context.Background(), bearer)
	if !errors.Is(err, rancher.ErrBadResponse) {
		t.Fatalf("want ErrBadResponse, got %v", err)
	}
}

var kagentAttrs = rancher.AccessAttrs{
	Namespace: "kagent", Verb: "create", Resource: "services",
	Subresource: "proxy", Name: "http:kagent-controller:8083",
}

func accessSetup(t *testing.T, f *ranchertest.Fake) (*rancher.Client, string) {
	t.Helper()
	if f.Clusters == nil {
		f.Clusters = []ranchertest.FakeCluster{{ID: "c-abc12", State: "active"}}
	}
	return listSetup(t, f)
}

func TestCanI_Allowed(t *testing.T) {
	f := newFake()
	c, bearer := accessSetup(t, f)
	allowed, _, err := c.CanI(context.Background(), bearer, "c-abc12", kagentAttrs)
	if err != nil || !allowed {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
	ra := f.LastSSAR()["spec"].(map[string]any)["resourceAttributes"].(map[string]any)
	want := map[string]any{
		"namespace": "kagent", "verb": "create", "resource": "services",
		"subresource": "proxy", "name": "http:kagent-controller:8083",
	}
	if len(ra) != len(want) {
		t.Fatalf("resourceAttributes = %v", ra)
	}
	for k, v := range want {
		if ra[k] != v {
			t.Fatalf("resourceAttributes[%s] = %v, want %v", k, ra[k], v)
		}
	}
	if b := f.LastSSAR(); b["apiVersion"] != "authorization.k8s.io/v1" || b["kind"] != "SelfSubjectAccessReview" {
		t.Fatalf("bad body: %v", b)
	}
	// The fake only answers a bearer it issued, so success proves the header.
	_, _, err = c.CanI(context.Background(), "bogus", "c-abc12", kagentAttrs)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("bogus bearer: err = %v", err)
	}
}

func TestCanI_Denied(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{{ID: "c-abc12", State: "active", Deny: map[string]bool{"jdoe": true}}}
	c, bearer := accessSetup(t, f)
	allowed, reason, err := c.CanI(context.Background(), bearer, "c-abc12", kagentAttrs)
	if err != nil || allowed {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
	if reason == "" {
		t.Fatal("expected a reason from the fake")
	}
}

func TestCanI_UnknownCluster(t *testing.T) {
	f := newFake()
	c, bearer := accessSetup(t, f)
	_, _, err := c.CanI(context.Background(), bearer, "c-nope", kagentAttrs)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentCard_OK(t *testing.T) {
	f := newFake()
	c, bearer := accessSetup(t, f)
	card, err := c.AgentCard(context.Background(), bearer, "c-abc12", "kagent", "http:kagent-controller:8083", "kagent", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "test-agent" || card.Description == "" || card.Version == "" || !card.Streaming {
		t.Fatalf("card = %+v", card)
	}
	want := "/k8s/clusters/c-abc12/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/kagent/test-agent/.well-known/agent-card.json"
	if got := f.LastCardPath(); got != want {
		t.Fatalf("path = %q\nwant   %q", got, want)
	}
	if _, err := c.AgentCard(context.Background(), "bogus", "c-abc12", "kagent", "http:kagent-controller:8083", "kagent", "test-agent"); err == nil {
		t.Fatal("unknown bearer must fail")
	}
}

func TestAgentCard_Status(t *testing.T) {
	for _, code := range []int{401, 403, 404, 502, 503} {
		f := newFake()
		f.Clusters = []ranchertest.FakeCluster{{ID: "c-abc12", State: "active", CardStatus: code}}
		c, bearer := accessSetup(t, f)
		_, err := c.AgentCard(context.Background(), bearer, "c-abc12", "kagent", "svc", "kagent", "a")
		var se *rancher.StatusError
		if !errors.As(err, &se) || se.Code != code {
			t.Fatalf("code %d: err = %v", code, err)
		}
	}
}

func TestAgentCard_BadJSON(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{{ID: "c-abc12", State: "active", CardBody: "<html>"}}
	c, bearer := accessSetup(t, f)
	_, err := c.AgentCard(context.Background(), bearer, "c-abc12", "kagent", "svc", "kagent", "a")
	if !errors.Is(err, rancher.ErrBadResponse) {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentCard_Hang(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{{ID: "c-abc12", State: "active", CardDelay: 2 * time.Second}}
	c := newClient(t, f, 100*time.Millisecond)
	tok, err := c.Login(context.Background(), "jdoe", "pw", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.AgentCard(context.Background(), tok.Bearer, "c-abc12", "kagent", "svc", "kagent", "a"); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout not honoured")
	}
}

func TestAgentCard_ClusterIDEscaped(t *testing.T) {
	f := newFake()
	f.Clusters = []ranchertest.FakeCluster{{ID: "c-a/b", State: "active"}}
	c, bearer := accessSetup(t, f)
	if _, err := c.AgentCard(context.Background(), bearer, "c-a/b", "kagent", "svc", "kagent", "a"); err != nil {
		t.Fatal(err)
	}
	if got := f.LastCardPath(); !strings.HasPrefix(got, "/k8s/clusters/c-a%2Fb/") {
		t.Fatalf("path = %q", got)
	}
}
