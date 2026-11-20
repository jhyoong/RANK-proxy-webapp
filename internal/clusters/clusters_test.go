package clusters_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

const principal = "activedirectory_user://CN=Test User,DC=example,DC=com"

func cl(id, name string) ranchertest.FakeCluster {
	return ranchertest.FakeCluster{ID: id, DisplayName: name, State: "active"}
}

func newFakes(cs ...[]ranchertest.FakeCluster) []*ranchertest.Fake {
	var fs []*ranchertest.Fake
	for _, c := range cs {
		fs = append(fs, &ranchertest.Fake{
			Users:    map[string]ranchertest.FakeUser{"jdoe": {Password: "pw", ID: "u-1", PrincipalIDs: []string{"local://u-1", principal}}},
			Clusters: c,
		})
	}
	return fs
}

type env struct {
	svc   *clusters.Service
	sites []auth.SiteResult
	logs  *syncBuf
}

func setup(t *testing.T, fs []*ranchertest.Fake) env {
	t.Helper()
	var clients []*rancher.Client
	for i, f := range fs {
		site := config.Site{Name: []string{"site-1", "site-2", "site-3"}[i]}
		site.URL, site.CAFile = ranchertest.NewTLS(t, f)
		c, err := rancher.NewClient(site, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	res := auth.New(clients, time.Hour, log).Login(context.Background(), "jdoe", "pw")
	return env{clusters.New(clients, config.KagentConfig{}, log), res.Sites, logs}
}

func ids(l *clusters.Listing) []string {
	var out []string
	for _, r := range l.Rows {
		out = append(out, r.Site+"/"+r.ID)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestList_MergesAndSorts(t *testing.T) {
	e := setup(t, newFakes(
		[]ranchertest.FakeCluster{cl("c-b", "b"), cl("c-a", "A")},
		[]ranchertest.FakeCluster{cl("c-c", "c")},
		nil))
	l := e.svc.List(context.Background(), e.sites)
	if got, want := ids(l), []string{"site-1/c-a", "site-1/c-b", "site-2/c-c"}; !eq(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if l.Sites[0].Count != 2 || l.Sites[1].Count != 1 || l.Sites[0].Status != "ok" {
		t.Fatalf("sites = %+v", l.Sites)
	}
	if l.Fetched.IsZero() {
		t.Fatal("Fetched not set")
	}
}

func TestList_DropsLocal(t *testing.T) {
	mk := func() []ranchertest.FakeCluster {
		return []ranchertest.FakeCluster{cl("local", "local"), cl("c-1", "one")}
	}
	e := setup(t, newFakes(mk(), mk(), mk()))
	l := e.svc.List(context.Background(), e.sites)
	for _, r := range l.Rows {
		if r.ID == "local" {
			t.Fatal("local not dropped")
		}
	}
	for _, s := range l.Sites {
		if s.Count != 1 {
			t.Fatalf("count = %d", s.Count)
		}
	}
}

func TestList_SiteUnauthorizedOthersStillListed(t *testing.T) {
	fs := newFakes(
		[]ranchertest.FakeCluster{cl("c-1", "one")},
		[]ranchertest.FakeCluster{cl("c-2", "two")},
		[]ranchertest.FakeCluster{cl("c-3", "three")})
	fs[1].ListStatus = 401
	e := setup(t, fs)
	l := e.svc.List(context.Background(), e.sites)
	s := l.Sites[1]
	if s.Status != "error" || s.HTTPCode != 401 || s.Error != "token expired or revoked: log in again" {
		t.Fatalf("site-2 = %+v", s)
	}
	if got, want := ids(l), []string{"site-1/c-1", "site-3/c-3"}; !eq(got, want) {
		t.Fatalf("rows = %v", got)
	}
}

func TestList_OtherHTTPError(t *testing.T) {
	fs := newFakes([]ranchertest.FakeCluster{cl("c-1", "one")})
	fs[0].ListStatus = 500
	e := setup(t, fs)
	s := e.svc.List(context.Background(), e.sites).Sites[0]
	if s.Status != "error" || s.HTTPCode != 500 || s.Error != "HTTP 500 from Rancher" {
		t.Fatalf("%+v", s)
	}
}

func TestList_SkipsSitesNotLoggedIn(t *testing.T) {
	fs := newFakes(
		[]ranchertest.FakeCluster{cl("c-1", "one")},
		[]ranchertest.FakeCluster{cl("c-2", "two")},
		[]ranchertest.FakeCluster{cl("c-3", "three")})
	e := setup(t, fs)
	e.sites[2].Status = auth.StatusUnavailable
	l := e.svc.List(context.Background(), e.sites)
	if l.Sites[2].Status != "skipped" {
		t.Fatalf("site-3 = %+v", l.Sites[2])
	}
	if fs[2].ListCalls() != 0 {
		t.Fatal("list call made to skipped site")
	}
	if len(l.Rows) != 2 {
		t.Fatalf("rows = %v", ids(l))
	}
}

func TestList_Parallel(t *testing.T) {
	fs := newFakes(
		[]ranchertest.FakeCluster{cl("c-1", "one")},
		[]ranchertest.FakeCluster{cl("c-2", "two")},
		[]ranchertest.FakeCluster{cl("c-3", "three")})
	for _, f := range fs {
		f.Delay = 300 * time.Millisecond
	}
	e := setup(t, fs)
	start := time.Now()
	l := e.svc.List(context.Background(), e.sites)
	if d := time.Since(start); d >= 600*time.Millisecond {
		t.Fatalf("took %v", d)
	}
	if len(l.Rows) != 3 {
		t.Fatalf("rows = %v", ids(l))
	}
}

func TestListing_FindAndFilter(t *testing.T) {
	e := setup(t, newFakes(
		[]ranchertest.FakeCluster{cl("c-x", "x"), cl("c-p1", "prod-a"), cl("c-prod9", "nine"), cl("c-q", "qa")}))
	l := e.svc.List(context.Background(), e.sites)
	if r, ok := l.Find("site-1", "c-x"); !ok || r.DisplayName != "x" {
		t.Fatalf("find = %+v %v", r, ok)
	}
	if _, ok := l.Find("site-2", "c-x"); ok {
		t.Fatal("wrong site hit")
	}
	if got := len(l.Filter("PROD")); got != 2 {
		t.Fatalf("filter PROD = %d", got)
	}
	if got := len(l.Filter("")); got != 4 {
		t.Fatalf("filter empty = %d", got)
	}
}

func TestList_LogHasNoToken(t *testing.T) {
	e := setup(t, newFakes([]ranchertest.FakeCluster{cl("c-1", "one")}))
	e.svc.List(context.Background(), e.sites)
	out := e.logs.String()
	if !bytes.Contains([]byte(out), []byte("clusters_list")) {
		t.Fatalf("no event logged: %s", out)
	}
	if b := e.sites[0].Token.Bearer; b == "" || bytes.Contains([]byte(out), []byte(b)) {
		t.Fatalf("bearer %q empty or leaked", b)
	}
}
