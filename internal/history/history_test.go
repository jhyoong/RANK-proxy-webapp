package history_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/history"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher/ranchertest"
)

var kagentCfg = config.KagentConfig{Namespace: "kagent", Service: "http:kagent-controller:8083", AgentNamespace: "kagent", AgentName: "k8s-agent"}

const agentID = "kagent__NS__k8s_agent"

type env struct {
	svc   *history.Service
	f     *ranchertest.Fake
	sites []auth.SiteResult
	row   clusters.Row
	logs  *strings.Builder
}

func setup(t *testing.T, c ranchertest.FakeCluster) env {
	t.Helper()
	c.ID = "c-1"
	f := &ranchertest.Fake{
		Users:    map[string]ranchertest.FakeUser{"jdoe": {Password: "pw", ID: "u-1"}},
		Clusters: []ranchertest.FakeCluster{c},
	}
	site := config.Site{Name: "site-1"}
	site.URL, site.CAFile = ranchertest.NewTLS(t, f)
	cli, err := rancher.NewClient(site, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	res := auth.New([]*rancher.Client{cli}, time.Hour, log).Login(context.Background(), "jdoe", "pw")
	row := clusters.Row{Site: "site-1", Cluster: rancher.Cluster{ID: "c-1", Connected: true}}
	logs := &strings.Builder{}
	hlog := slog.New(slog.NewTextHandler(logs, nil))
	return env{history.New([]*rancher.Client{cli}, kagentCfg, 30*time.Minute, hlog), f, res.Sites, row, logs}
}

// uid returns a valid lowercase UUID numbered n.
func uid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

var s1 = uid(1)

const oneTask = `[{"id":"t1","contextId":"s1","kind":"task","status":{"state":"completed","timestamp":"2026-11-20T11:34:22Z"},
 "history":[{"role":"user","messageId":"u","parts":[{"kind":"text","text":"hello"}]},
  {"role":"agent","messageId":"a","parts":[{"kind":"text","text":"hi back"}]}]}]`

func TestList_TitlesAndCap(t *testing.T) {
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	var ss []ranchertest.FakeSession
	for i := 0; i < 55; i++ {
		ss = append(ss, ranchertest.FakeSession{ID: uid(i), Name: fmt.Sprintf("chat %d", i), AgentID: agentID,
			Updated: base.Add(time.Duration(i) * time.Hour)})
	}
	ss[54].Name = "" // newest: date title
	e := setup(t, ranchertest.FakeCluster{Sessions: ss})
	got, err := e.svc.List(context.Background(), e.sites, e.row)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("len %d", len(got))
	}
	if got[0].ID != uid(54) || got[0].Title != "Conversation · 3 Nov 2026" || !got[0].Updated.Equal(ss[54].Updated) {
		t.Fatalf("first %+v", got[0])
	}
	if got[1].Title != "chat 53" || got[49].ID != uid(5) {
		t.Fatalf("order %+v ... %+v", got[1], got[49])
	}

	// No name and no parseable time.
	e = setup(t, ranchertest.FakeCluster{Sessions: []ranchertest.FakeSession{{ID: uid(99), AgentID: agentID}}})
	got, err = e.svc.List(context.Background(), e.sites, e.row)
	if err != nil || len(got) != 1 || got[0].Title != "Conversation" || !got[0].Updated.IsZero() {
		t.Fatalf("no-time %+v %v", got, err)
	}
}

func TestList_AgentMissing(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{SessionStatus: 404})
	got, err := e.svc.List(context.Background(), e.sites, e.row)
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	// The empty sidebar is explained in the log.
	l := e.logs.String()
	if !strings.Contains(l, "event=history_list") || !strings.Contains(l, "result=not-found") ||
		!strings.Contains(l, "agent=kagent/k8s-agent") || !strings.Contains(l, "kagent_body=") {
		t.Fatalf("log %q", l)
	}
}

func TestList_LogsCounts(t *testing.T) {
	ss := []ranchertest.FakeSession{{ID: uid(1), AgentID: agentID}, {ID: "not-a-uuid", AgentID: agentID}}
	e := setup(t, ranchertest.FakeCluster{Sessions: ss})
	if _, err := e.svc.List(context.Background(), e.sites, e.row); err != nil {
		t.Fatal(err)
	}
	l := e.logs.String()
	if !strings.Contains(l, "result=ok") || !strings.Contains(l, "sessions=2") || !strings.Contains(l, "shown=1") ||
		!strings.Contains(l, "skipped_ids=1") || !strings.Contains(l, "site=site-1") || !strings.Contains(l, "cluster=c-1") {
		t.Fatalf("log %q", l)
	}
	// Transcript re-checks the list on every poll; that check is not logged.
	before := strings.Count(e.logs.String(), "history_list")
	_, _ = e.svc.Transcript(context.Background(), e.sites, e.row, uid(1))
	if n := strings.Count(e.logs.String(), "history_list"); n != before {
		t.Fatalf("transcript logged the list: %q", e.logs.String())
	}
}

func TestList_Errors(t *testing.T) {
	for code, want := range map[int]error{401: history.ErrExpired, 403: history.ErrForbidden, 500: nil} {
		e := setup(t, ranchertest.FakeCluster{SessionStatus: code})
		_, err := e.svc.List(context.Background(), e.sites, e.row)
		if want != nil && !errors.Is(err, want) {
			t.Fatalf("%d: %v", code, err)
		}
		if want == nil && (err == nil || errors.Is(err, history.ErrExpired) || errors.Is(err, history.ErrForbidden)) {
			t.Fatalf("%d: %v", code, err)
		}
	}
}

func TestList_NotLoggedIn(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{})
	if _, err := e.svc.List(context.Background(), nil, e.row); !errors.Is(err, history.ErrExpired) {
		t.Fatal(err)
	}
}

func TestTranscript_NotInList(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{Sessions: []ranchertest.FakeSession{{ID: s1, AgentID: agentID, TasksJSON: oneTask}}})
	_, err := e.svc.Transcript(context.Background(), e.sites, e.row, "other")
	if !errors.Is(err, history.ErrNotFound) {
		t.Fatal(err)
	}
	if e.f.TasksCalls() != 0 {
		t.Fatalf("tasks calls %d", e.f.TasksCalls())
	}
}

func TestTranscript_OK(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{Sessions: []ranchertest.FakeSession{{ID: s1, Name: "hello", AgentID: agentID,
		Updated: time.Now(), TasksJSON: oneTask}}})
	tr, err := e.svc.Transcript(context.Background(), e.sites, e.row, s1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Turns) != 1 || tr.Turns[0].User != "hello" || len(tr.Turns[0].Items) != 1 || tr.Turns[0].Items[0].Text != "hi back" {
		t.Fatalf("%+v", tr)
	}
	if e.f.TasksCalls() != 1 {
		t.Fatalf("tasks calls %d", e.f.TasksCalls())
	}
}

func TestNoUserID(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{Sessions: []ranchertest.FakeSession{{ID: s1, AgentID: agentID, TasksJSON: oneTask}}})
	if _, err := e.svc.List(context.Background(), e.sites, e.row); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		u, h := e.f.LastSessionsURL(), e.f.LastSessionsHeaders()
		if u == nil || u.Query().Has("user_id") || strings.Contains(u.RawQuery, "user_id") {
			t.Fatalf("url %v", u)
		}
		if h.Get("Authorization") == "" || h.Get("X-User-Id") != "" {
			t.Fatalf("headers %v", h)
		}
	}
	check()
	if _, err := e.svc.Transcript(context.Background(), e.sites, e.row, s1); err != nil {
		t.Fatal(err)
	}
	if u := e.f.LastSessionsURL(); !strings.HasSuffix(u.Path, "/api/sessions/"+s1+"/tasks") {
		t.Fatalf("last %v", u)
	}
	check()
}

// Sessions whose id isn't a UUID can't be opened, so List leaves them out,
// before the 50-entry cap.
func TestList_SkipsNonUUID(t *testing.T) {
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	var ss []ranchertest.FakeSession
	for i := 0; i < 50; i++ {
		ss = append(ss, ranchertest.FakeSession{ID: uid(i), AgentID: agentID, Updated: base.Add(time.Duration(i) * time.Hour)})
	}
	// Newer than every valid one, so a cap applied first would drop valid ids.
	for i, id := range []string{"s1", "not-a-uuid", "00000000-0000-4000-8000-ABCDEF000000", ""} {
		ss = append(ss, ranchertest.FakeSession{ID: id, AgentID: agentID, Updated: base.Add(time.Duration(100+i) * time.Hour)})
	}
	e := setup(t, ranchertest.FakeCluster{Sessions: ss})
	got, err := e.svc.List(context.Background(), e.sites, e.row)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 || got[0].ID != uid(49) || got[49].ID != uid(0) {
		t.Fatalf("len %d first %+v", len(got), got)
	}
	if _, err := e.svc.Transcript(context.Background(), e.sites, e.row, "s1"); !errors.Is(err, history.ErrNotFound) {
		t.Fatalf("non-uuid transcript: %v", err)
	}
}

// The tasks call maps 404/401/403 to the package errors.
func TestTranscript_TasksErrors(t *testing.T) {
	for code, want := range map[int]error{404: history.ErrNotFound, 401: history.ErrExpired, 403: history.ErrForbidden} {
		e := setup(t, ranchertest.FakeCluster{TasksStatus: code,
			Sessions: []ranchertest.FakeSession{{ID: s1, AgentID: agentID, TasksJSON: oneTask}}})
		_, err := e.svc.Transcript(context.Background(), e.sites, e.row, s1)
		if !errors.Is(err, want) {
			t.Fatalf("%d: %v", code, err)
		}
		if e.f.TasksCalls() != 1 {
			t.Fatalf("%d: tasks calls %d", code, e.f.TasksCalls())
		}
	}
}
