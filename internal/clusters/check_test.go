package clusters_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

var kagentCfg = config.KagentConfig{Namespace: "kagent", Service: "http:kagent-controller:8083", AgentNamespace: "kagent", AgentName: "test-agent"}

func checkSetup(t *testing.T, timeout time.Duration, c ranchertest.FakeCluster) (env, *ranchertest.Fake, clusters.Row) {
	t.Helper()
	f := newFakes([]ranchertest.FakeCluster{c})[0]
	site := config.Site{Name: "site-1"}
	site.URL, site.CAFile = ranchertest.NewTLS(t, f)
	cli, err := rancher.NewClient(site, timeout)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	res := auth.New([]*rancher.Client{cli}, time.Hour, log).Login(context.Background(), "jdoe", "pw")
	row := clusters.Row{Site: "site-1", Cluster: rancher.Cluster{ID: c.ID, DisplayName: c.DisplayName, Connected: true}}
	return env{clusters.New([]*rancher.Client{cli}, kagentCfg, log), res.Sites, logs}, f, row
}

func TestCheck_Ready(t *testing.T) {
	e, f, row := checkSetup(t, 5*time.Second, cl("c-1", "one"))
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "ready" || r.Message != "kagent is ready" || r.Card == nil || r.Card.Name == "" ||
		r.SSARAllowed == nil || !*r.SSARAllowed || r.CardCode != 200 {
		t.Fatalf("%+v", r)
	}
	if f.SSARCalls() != 1 || f.CardCalls() != 1 {
		t.Fatal("call counts")
	}
}

func TestCheck_Disconnected(t *testing.T) {
	e, f, row := checkSetup(t, 5*time.Second, cl("c-1", "one"))
	row.Connected = false
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "disconnected" || r.Message != "Cluster is disconnected in Rancher" {
		t.Fatalf("%+v", r)
	}
	if f.SSARCalls() != 0 || f.CardCalls() != 0 {
		t.Fatal("calls made")
	}
}

func TestCheck_NoAccess(t *testing.T) {
	c := cl("c-1", "one")
	c.Deny = map[string]bool{"jdoe": true}
	e, f, row := checkSetup(t, 5*time.Second, c)
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "no-access" || r.Message != "You don't have kagent access on this cluster" ||
		r.SSARAllowed == nil || *r.SSARAllowed || r.SSARReason == "" {
		t.Fatalf("%+v", r)
	}
	if f.CardCalls() != 0 {
		t.Fatal("card called")
	}
}

func TestCheck_CardCodes(t *testing.T) {
	for _, tc := range []struct {
		code int
		want clusters.Outcome
		msg  string
	}{
		{404, "not-installed", "kagent or the agent isn't installed on this cluster"},
		{503, "not-running", "kagent is installed but not running"},
		{502, "unreachable", "Cluster unreachable through Rancher"},
		{403, "denied", "Access denied by the cluster (the permission check said yes)"},
		{401, "expired", "Your session on site-1 has expired: log in again"},
		{418, "unexpected", "Unexpected response (HTTP 418)"},
	} {
		c := cl("c-1", "one")
		c.CardStatus = tc.code
		e, _, row := checkSetup(t, 5*time.Second, c)
		r := e.svc.Check(context.Background(), e.sites, row, principal)
		if r.Outcome != tc.want || r.Message != tc.msg || r.CardCode != tc.code {
			t.Errorf("%d: %+v", tc.code, r)
		}
	}
}

func TestCheck_BadCard(t *testing.T) {
	c := cl("c-1", "one")
	c.CardBody = "<html>"
	e, _, row := checkSetup(t, 5*time.Second, c)
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "bad-card" || r.Message != "Unexpected agent card format" || r.CardCode != 200 {
		t.Fatalf("%+v", r)
	}
}

func TestCheck_SSARError(t *testing.T) {
	c := cl("c-1", "one")
	c.SSARStatus = 500
	e, f, row := checkSetup(t, 5*time.Second, c)
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "check-failed" || r.SSARCode != 500 || r.SSARAllowed != nil ||
		!strings.HasPrefix(r.Message, "Permission check failed: ") {
		t.Fatalf("%+v", r)
	}
	if f.CardCalls() != 0 {
		t.Fatal("card called")
	}
}

func TestCheck_SSAR401Expired(t *testing.T) {
	c := cl("c-1", "one")
	c.SSARStatus = 401
	e, f, row := checkSetup(t, 5*time.Second, c)
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "expired" || r.SSARCode != 401 || !strings.Contains(r.Message, "site-1") || f.CardCalls() != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestCheck_CardHangsTimesOut(t *testing.T) {
	c := cl("c-1", "one")
	c.CardDelay = 2 * time.Second
	e, _, row := checkSetup(t, 200*time.Millisecond, c)
	start := time.Now()
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "unreachable" || r.CardCode != 0 || time.Since(start) > time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
}

func TestCheck_SiteNotLoggedIn(t *testing.T) {
	e, f, row := checkSetup(t, 5*time.Second, cl("c-1", "one"))
	e.sites[0].Status = auth.StatusRejected
	r := e.svc.Check(context.Background(), e.sites, row, principal)
	if r.Outcome != "expired" || !strings.Contains(r.Message, "site-1") || f.SSARCalls() != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestCheck_LogFields(t *testing.T) {
	e, _, row := checkSetup(t, 5*time.Second, cl("c-1", "one"))
	e.svc.Check(context.Background(), e.sites, row, principal)
	out := e.logs.String()
	i := strings.Index(out, `"event":"cluster_check"`)
	if i < 0 {
		t.Fatalf("no line: %s", out)
	}
	line := out[i:]
	for _, k := range []string{"site", "cluster", "principal", "ssar_allowed", "ssar_ms", "card_http_code", "card_ms", "result"} {
		if !strings.Contains(line, `"`+k+`":`) {
			t.Errorf("missing %s in %s", k, line)
		}
	}
	if strings.Contains(out, e.sites[0].Token.Bearer) {
		t.Fatal("bearer logged")
	}
}
