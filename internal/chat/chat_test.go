package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/chat"
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

const (
	principal = "activedirectory_user://CN=Test User,DC=example,DC=com"
	userText  = "how many pods are running in kube-system"
)

var kagentCfg = config.KagentConfig{Namespace: "kagent", Service: "http:kagent-controller:8083", AgentNamespace: "kagent", AgentName: "test-agent"}

func baseCfg() config.ChatConfig {
	return config.ChatConfig{
		MaxDuration:     30 * time.Second,
		IdleTimeout:     5 * time.Second,
		MaxMessageBytes: 8192,
		Heartbeat:       time.Second,
		LogEvents:       false,
	}
}

type env struct {
	svc   *chat.Service
	f     *ranchertest.Fake
	sites []auth.SiteResult
	row   clusters.Row
	logs  *syncBuf
}

func setup(t *testing.T, c ranchertest.FakeCluster, cfg config.ChatConfig) env {
	t.Helper()
	c.ID, c.State = "c-1", "active"
	f := &ranchertest.Fake{
		Users:    map[string]ranchertest.FakeUser{"jdoe": {Password: "pw", ID: "u-1", PrincipalIDs: []string{"local://u-1", principal}}},
		Clusters: []ranchertest.FakeCluster{c},
	}
	site := config.Site{Name: "site-1"}
	site.URL, site.CAFile = ranchertest.NewTLS(t, f)
	cli, err := rancher.NewClient(site, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	res := auth.New([]*rancher.Client{cli}, time.Hour, log).Login(context.Background(), "jdoe", "pw")
	if !res.OK {
		t.Fatalf("login: %+v", res.Sites)
	}
	row := clusters.Row{Site: "site-1", Cluster: rancher.Cluster{ID: "c-1", DisplayName: "one", Connected: true}}
	return env{chat.New([]*rancher.Client{cli}, kagentCfg, cfg, log), f, res.Sites, row, logs}
}

type recorder struct {
	mu   sync.Mutex
	outs []chat.Out
}

func (r *recorder) emit(o chat.Out) { r.mu.Lock(); r.outs = append(r.outs, o); r.mu.Unlock() }
func (r *recorder) all() []chat.Out {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]chat.Out(nil), r.outs...)
}

func (e env) send(t *testing.T, ctx context.Context, text, contextID string) []chat.Out {
	t.Helper()
	var r recorder
	e.svc.Send(ctx, e.sites, e.row, principal, text, contextID, nil, r.emit)
	return r.all()
}

func types(outs []chat.Out) []string {
	var s []string
	for _, o := range outs {
		s = append(s, o.Type)
	}
	return s
}

// checkDone asserts exactly one done, last, and returns its data.
func checkDone(t *testing.T, outs []chat.Out) chat.Done {
	t.Helper()
	n := 0
	for _, o := range outs {
		if o.Type == "done" {
			n++
		}
	}
	if n != 1 || len(outs) == 0 || outs[len(outs)-1].Type != "done" {
		t.Fatalf("want exactly one done, last; got %v", types(outs))
	}
	d, ok := outs[len(outs)-1].Data.(chat.Done)
	if !ok {
		t.Fatalf("done data = %#v", outs[len(outs)-1].Data)
	}
	return d
}

func errorsOf(outs []chat.Out) []string {
	var s []string
	for _, o := range outs {
		if o.Type == "error" {
			s = append(s, o.Data.(chat.Error).Message)
		}
	}
	return s
}

func wantSingleError(t *testing.T, outs []chat.Out, msg string) {
	t.Helper()
	errs := errorsOf(outs)
	if len(errs) != 1 || errs[0] != msg {
		t.Fatalf("errors = %q, want [%q]; outs = %v", errs, msg, types(outs))
	}
	checkDone(t, outs)
	if outs[len(outs)-2].Type != "error" {
		t.Fatalf("error not right before done: %v", types(outs))
	}
}

// chatLine returns the single event=chat log line, decoded.
func chatLine(t *testing.T, logs string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event"] == "chat" {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one event=chat line, got %d in:\n%s", len(found), logs)
	}
	return found[0]
}

// wantRunning checks the error's Running flag: whether kagent may still be
// running the turn, so the browser should poll the transcript.
func wantRunning(t *testing.T, outs []chat.Out, want bool) {
	t.Helper()
	for _, o := range outs {
		if d, ok := o.Data.(chat.Error); ok {
			if d.Running != want {
				t.Fatalf("error %+v: running = %v, want %v", d, d.Running, want)
			}
			return
		}
	}
	t.Fatalf("no error in %v", types(outs))
}

func wantResult(t *testing.T, logs, result string) {
	t.Helper()
	if got := chatLine(t, logs)["result"]; got != result {
		t.Fatalf("result = %v, want %s", got, result)
	}
}

func step(data string) ranchertest.Step {
	return ranchertest.Step{Delay: 10 * time.Millisecond, Data: data}
}

const (
	submitted = `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":false,"status":{"state":"submitted","message":{"kind":"message","messageId":"u","role":"user","parts":[{"kind":"text","text":"{{text}}"}]}}}}`
	working   = `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":false,"status":{"state":"working"}}}`
)

func finalState(state string) string {
	return `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":true,"status":{"state":"` + state + `"}}}`
}

func TestSend_DefaultScript(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{}, baseCfg())
	outs := e.send(t, context.Background(), userText, a2a.NewUUID())
	want := []string{"state", "state", "tool", "progress", "progress", "progress", "answer", "state", "done"}
	if got := types(outs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("types = %v, want %v", got, want)
	}
	if s := outs[0].Data.(chat.State).State; s != "submitted" {
		t.Fatalf("state 0 = %q", s)
	}
	if s := outs[1].Data.(chat.State).State; s != "working" {
		t.Fatalf("state 1 = %q", s)
	}
	if n := outs[2].Data.(chat.Tool).Name; n != "k8s_get_resources" {
		t.Fatalf("tool = %q", n)
	}
	for i, wantPartial := range map[int]bool{3: true, 4: true, 5: false} {
		if p := outs[i].Data.(chat.Progress); p.Partial != wantPartial {
			t.Fatalf("progress %d = %+v", i, p)
		}
	}
	full := outs[5].Data.(chat.Progress).Text
	if !strings.Contains(full, userText) || !strings.Contains(full, "turn 1") {
		t.Fatalf("complete step = %q", full)
	}
	ans := outs[6].Data.(chat.Answer).Text
	if !strings.Contains(ans, userText) {
		t.Fatalf("answer = %q", ans)
	}
	if s := outs[7].Data.(chat.State).State; s != "completed" {
		t.Fatalf("state 7 = %q", s)
	}
	for _, o := range outs {
		switch d := o.Data.(type) {
		case chat.Progress:
			if d.Text == userText {
				t.Fatal("user echo shown as progress")
			}
		case chat.Answer:
			if d.Text == userText {
				t.Fatal("user echo shown as answer")
			}
		}
	}
	d := checkDone(t, outs)
	if d.State != "completed" || d.TaskID != "t1" || d.Events != 8 {
		t.Fatalf("done = %+v", d)
	}
	if h := e.f.LastChatHeaders(); h.Get("X-User-Id") != "" || h.Get("Authorization") == "" {
		t.Fatalf("headers = %v", h)
	}
	if u := e.f.LastChatURL(); u == nil || u.Query().Has("user_id") {
		t.Fatalf("url = %v", u)
	}
	wantResult(t, e.logs.String(), "completed")
}

func TestSend_MultiTurnSameContext(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{}, baseCfg())
	ctxID := a2a.NewUUID()
	for i, want := range []string{"turn 1", "turn 2"} {
		outs := e.send(t, context.Background(), "hello", ctxID)
		var ans string
		for _, o := range outs {
			if o.Type == "answer" {
				ans = o.Data.(chat.Answer).Text
			}
		}
		if !strings.Contains(ans, want) {
			t.Fatalf("send %d answer = %q, want %q", i, ans, want)
		}
	}
}

func TestSend_PreStreamCodes(t *testing.T) {
	for _, tc := range []struct {
		code   int
		msg    string
		result string
	}{
		{401, "Your session on site-1 has expired: log in again", "expired"},
		{403, "You don't have kagent access on this cluster", "no-access"},
		{404, "kagent or the agent isn't installed on this cluster", "not-installed"},
		{503, "kagent is installed but not running", "not-running"},
		{502, "Cluster unreachable through Rancher", "unreachable"},
		{418, "Unexpected response from the cluster. Try again, or contact your platform team.", "unexpected"},
	} {
		e := setup(t, ranchertest.FakeCluster{ChatStatus: tc.code}, baseCfg())
		outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
		wantSingleError(t, outs, tc.msg)
		wantRunning(t, outs, false)
		if len(outs) != 2 {
			t.Fatalf("%d: outs = %v", tc.code, types(outs))
		}
		line := chatLine(t, e.logs.String())
		if line["result"] != tc.result || line["http_code"] != float64(tc.code) {
			t.Fatalf("%d: log = %v", tc.code, line)
		}
		_, hasBody := line["rancher_body"]
		if wantBody := tc.code != 401 && tc.code != 403 && tc.code != 404; hasBody != wantBody {
			t.Fatalf("%d: rancher_body present=%v", tc.code, hasBody)
		}
	}
}

func TestSend_SiteNotLoggedIn(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{}, baseCfg())
	e.sites[0].Status = auth.StatusUnavailable
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	wantSingleError(t, outs, "Your session on site-1 has expired: log in again")
	if e.f.ChatCalls() != 0 {
		t.Fatal("chat call made")
	}
	wantResult(t, e.logs.String(), "expired")
}

func TestSend_JSONRPCError(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{
		step(`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"boom"}}`),
		step(working),
	}}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	wantSingleError(t, outs, "Agent error: boom")
	wantResult(t, e.logs.String(), "error")
	wantRunning(t, outs, false)

	long := strings.Repeat("é", 400)
	e = setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{
		step(`{"jsonrpc":"2.0","id":"1","error":{"code":-32603,"message":"` + long + `"}}`),
	}}, baseCfg())
	outs = e.send(t, context.Background(), "hi", a2a.NewUUID())
	wantSingleError(t, outs, "Agent error: "+strings.Repeat("é", 300))
}

func TestSend_FailedState(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{
		step(submitted), step(working), step(finalState("failed")),
	}}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	wantSingleError(t, outs, "The agent reported the task failed")
	if d := checkDone(t, outs); d.State != "failed" {
		t.Fatalf("done = %+v", d)
	}
	wantResult(t, e.logs.String(), "failed")
	wantRunning(t, outs, false)
}

func TestSend_InputRequired(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{
		step(submitted), step(working), step(finalState("input-required")),
	}}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	wantSingleError(t, outs, "The agent is asking for approval, which the portal doesn't support yet")
	wantResult(t, e.logs.String(), "input-required")
	wantRunning(t, outs, false)
}

func TestSend_InputRequiredAsk(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{}, baseCfg())
	outs := e.send(t, context.Background(), "please ask me something", a2a.NewUUID())
	if errs := errorsOf(outs); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
	var asks []chat.Ask
	for _, o := range outs {
		if o.Type == "ask" {
			asks = append(asks, o.Data.(chat.Ask))
		}
		if tl, ok := o.Data.(chat.Tool); ok && tl.Name != "ask_user" {
			t.Fatalf("internal tool shown: %+v", tl)
		}
		if tr, ok := o.Data.(chat.ToolResult); ok {
			t.Fatalf("pending stub shown: %+v", tr)
		}
	}
	if len(asks) != 1 || !strings.HasPrefix(asks[0].TaskID, "task-") || asks[0].ID != "adk-conf-1" ||
		len(asks[0].Questions) != 2 || !asks[0].Questions[1].Multiple {
		t.Fatalf("asks = %+v; outs = %v", asks, types(outs))
	}
	if d := checkDone(t, outs); d.State != "input-required" || d.TaskID != asks[0].TaskID {
		t.Fatalf("done = %+v", d)
	}
	wantResult(t, e.logs.String(), "input-required")
}

func TestSend_AskDeduplicated(t *testing.T) {
	ask := `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":%s,` +
		`"status":{"state":"%s","message":{"kind":"message","role":"agent","parts":[` + ranchertest.AskConfirmation + `]}}}}`
	e := setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{
		step(submitted), step(fmt.Sprintf(ask, "false", "working")), step(fmt.Sprintf(ask, "true", "input-required")),
	}}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	n := 0
	for _, o := range outs {
		if o.Type == "ask" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("asks = %d; outs = %v", n, types(outs))
	}
}

func TestSend_Resume(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{}, baseCfg())
	ctxID := a2a.NewUUID()
	first := e.send(t, context.Background(), "ask me", ctxID)
	task := checkDone(t, first).TaskID
	var r recorder
	e.svc.Send(context.Background(), e.sites, e.row, principal, "", ctxID,
		&chat.Resume{TaskID: task, Answers: [][]string{{"default"}, {"pods", "services"}}}, r.emit)
	outs := r.all()
	if errs := errorsOf(outs); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
	if d := checkDone(t, outs); d.State != "completed" || d.TaskID != task {
		t.Fatalf("done = %+v", d)
	}
	msg := e.f.LastChatBody()["params"].(map[string]any)["message"].(map[string]any)
	if msg["taskId"] != task || msg["contextId"] != ctxID {
		t.Fatalf("message = %v", msg)
	}
	var answer string
	for _, o := range outs {
		if o.Type == "answer" || o.Type == "progress" {
			if a, ok := o.Data.(chat.Answer); ok {
				answer = a.Text
			}
			if p, ok := o.Data.(chat.Progress); ok {
				answer = p.Text
			}
		}
		if o.Type == "tool_result" && o.Data.(chat.ToolResult).Name != "ask_user" {
			t.Fatalf("unexpected result %+v", o.Data)
		}
	}
	if answer != "Thanks, you chose [[default] [pods services]]." {
		t.Fatalf("answer = %q; outs = %v", answer, types(outs))
	}
}

func TestSend_Skip(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{}, baseCfg())
	ctxID := a2a.NewUUID()
	task := checkDone(t, e.send(t, context.Background(), "ask me", ctxID)).TaskID
	var r recorder
	e.svc.Send(context.Background(), e.sites, e.row, principal, "", ctxID, &chat.Resume{TaskID: task, Skip: true}, r.emit)
	outs := r.all()
	if d := checkDone(t, outs); d.State != "completed" || len(errorsOf(outs)) != 0 {
		t.Fatalf("outs = %v", types(outs))
	}
	msg := e.f.LastChatBody()["params"].(map[string]any)["message"].(map[string]any)
	data := msg["parts"].([]any)[0].(map[string]any)["data"].(map[string]any)
	if msg["taskId"] != task || data["decision_type"] != "reject" {
		t.Fatalf("message = %v", msg)
	}
}

func TestSend_EOFWithoutFinal(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{ChatSteps: ranchertest.DefaultChat()[:2]}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	wantSingleError(t, outs, "The connection to the agent was cut before the reply finished")
	if d := checkDone(t, outs); d.State != "working" {
		t.Fatalf("done = %+v", d)
	}
	wantResult(t, e.logs.String(), "cut-off")
	wantRunning(t, outs, true)
}

func TestSend_MalformedEvent(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{
		step(submitted), {Delay: 10 * time.Millisecond, Raw: "data: {not json\n\n"}, step(finalState("completed")),
	}}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	wantSingleError(t, outs, "Unexpected event from the agent")
	wantResult(t, e.logs.String(), "unexpected")
	wantRunning(t, outs, true)
}

func TestSend_UnknownKindIgnored(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{
		step(`{"jsonrpc":"2.0","id":"1","result":{"kind":"something-new"}}`), step(finalState("completed")),
	}}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	if errs := errorsOf(outs); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
	if d := checkDone(t, outs); d.Events != 2 || d.State != "completed" {
		t.Fatalf("done = %+v", d)
	}
}

func TestSend_IdleTimeout(t *testing.T) {
	cfg := baseCfg()
	cfg.IdleTimeout = 200 * time.Millisecond
	e := setup(t, ranchertest.FakeCluster{ChatHang: true}, cfg)
	start := time.Now()
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	wantSingleError(t, outs, "No response from the agent for 1 second, so the reply was stopped")
	wantResult(t, e.logs.String(), "idle-timeout")
	wantRunning(t, outs, true)
	waitCancelled(t, e.f)
}

func TestSend_TotalTimeout(t *testing.T) {
	cfg := baseCfg()
	cfg.MaxDuration = 300 * time.Millisecond
	var steps []ranchertest.Step
	for i := 0; i < 20; i++ {
		steps = append(steps, ranchertest.Step{Delay: 100 * time.Millisecond, Data: working})
	}
	e := setup(t, ranchertest.FakeCluster{ChatSteps: steps}, cfg)
	start := time.Now()
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("took %v", d)
	}
	wantSingleError(t, outs, "The reply took longer than 1 second and was stopped")
	wantResult(t, e.logs.String(), "timeout")
	wantRunning(t, outs, true)
	waitCancelled(t, e.f)
}

func TestSend_HeartbeatDuringSilence(t *testing.T) {
	cfg := baseCfg()
	cfg.Heartbeat = 50 * time.Millisecond
	e := setup(t, ranchertest.FakeCluster{ChatSteps: []ranchertest.Step{{
		Delay: 300 * time.Millisecond,
		Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":true,` +
			`"status":{"state":"completed","message":{"kind":"message","messageId":"m","role":"agent","parts":[{"kind":"text","text":"all good"}]}}}}`,
	}}}, cfg)
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	if errs := errorsOf(outs); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
	pings, answered := 0, false
	for _, o := range outs {
		switch o.Type {
		case "ping":
			if answered {
				continue
			}
			if o.Data != nil {
				t.Fatalf("ping data = %#v", o.Data)
			}
			pings++
		case "answer":
			answered = true
			if o.Data.(chat.Answer).Text != "all good" {
				t.Fatalf("answer = %+v", o.Data)
			}
		}
	}
	if !answered || pings < 3 {
		t.Fatalf("pings before answer = %d, answered = %v: %v", pings, answered, types(outs))
	}
	if d := checkDone(t, outs); d.State != "completed" {
		t.Fatalf("done = %+v", d)
	}
}

func TestSend_CancelStopsUpstream(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{}, baseCfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var r recorder
	emit := func(o chat.Out) {
		r.emit(o)
		cancel() // after the first event
	}
	start := time.Now()
	e.svc.Send(ctx, e.sites, e.row, principal, "hi", a2a.NewUUID(), nil, emit)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	outs := r.all()
	checkDone(t, outs)
	if errs := errorsOf(outs); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
	wantResult(t, e.logs.String(), "cancelled")
	waitCancelled(t, e.f)
}

func waitCancelled(t *testing.T, f *ranchertest.Fake) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.ChatCancelled() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.ChatCancelled(); got != 1 {
		t.Fatalf("ChatCancelled = %d", got)
	}
}

func TestSend_LogFields(t *testing.T) {
	cfg := baseCfg()
	cfg.LogEvents = true
	e := setup(t, ranchertest.FakeCluster{}, cfg)
	ctxID := a2a.NewUUID()
	outs := e.send(t, context.Background(), userText, ctxID)
	var answer string
	for _, o := range outs {
		if o.Type == "answer" {
			answer = o.Data.(chat.Answer).Text
		}
	}
	if answer == "" {
		t.Fatal("no answer")
	}
	logs := e.logs.String()
	line := chatLine(t, logs)
	for _, k := range []string{"site", "cluster", "principal", "context_id", "task_id", "http_code", "first_event_ms", "total_ms", "events", "result"} {
		if _, ok := line[k]; !ok {
			t.Errorf("missing key %q in %v", k, line)
		}
	}
	if line["site"] != "site-1" || line["cluster"] != "c-1" || line["principal"] != principal ||
		line["context_id"] != ctxID || line["task_id"] != "t1" || line["http_code"] != float64(200) ||
		line["events"] != float64(8) || line["result"] != "completed" {
		t.Fatalf("line = %v", line)
	}
	if _, ok := line["rancher_body"]; ok {
		t.Fatal("rancher_body on success")
	}
	bearer := e.sites[0].Token.Bearer
	if bearer == "" {
		t.Fatal("no bearer")
	}
	for _, bad := range []string{bearer, userText, answer, "Answer to"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("logs contain %q:\n%s", bad, logs)
		}
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["event"] != "chat_event" {
			continue
		}
		n++
		for _, k := range []string{"context_id", "n", "ms", "shape"} {
			if _, ok := m[k]; !ok {
				t.Errorf("chat_event missing %q: %v", k, m)
			}
		}
	}
	if n != 8 {
		t.Fatalf("chat_event lines = %d", n)
	}
}

func TestSend_CompleteStepTextBeforeTools(t *testing.T) {
	msg := func(partial, parts string) string {
		return `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":false,` +
			`"status":{"state":"working","message":{"kind":"message","messageId":"m","role":"agent",` +
			`"metadata":{"kagent_adk_partial":` + partial + `},"parts":[` + parts + `]}}}}`
	}
	text := func(s string) string { return `{"kind":"text","text":"` + s + `"}` }
	call := `{"kind":"data","data":{"id":"call_1","name":"k8s_get_resources","args":{}},"metadata":{"kagent_type":"function_call"}}`
	steps := []ranchertest.Step{
		step(msg("true", text("Let me "))),
		step(msg("true", text("check."))),
		step(msg("false", text("Let me check.")+","+call)),
		step(finalState("completed")),
	}
	e := setup(t, ranchertest.FakeCluster{ChatSteps: steps}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	want := []string{"progress", "state", "progress", "progress", "tool", "state", "done"}
	if got := types(outs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("types = %v, want %v", got, want)
	}
	if p := outs[3].Data.(chat.Progress); p.Partial || p.Text != "Let me check." {
		t.Fatalf("complete step = %+v", p)
	}
	if n := outs[4].Data.(chat.Tool).Name; n != "k8s_get_resources" {
		t.Fatalf("tool = %q", n)
	}
}

func TestSend_HeaderTimeout(t *testing.T) {
	cfg := baseCfg()
	cfg.IdleTimeout = 150 * time.Millisecond
	// Delay applies to every request (login included); it is far above IdleTimeout.
	e := setup(t, ranchertest.FakeCluster{}, cfg)
	e.f.Delay = 2 * time.Second
	start := time.Now()
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	wantSingleError(t, outs, "No response from the agent for 1 second, so the reply was stopped")
	wantResult(t, e.logs.String(), "idle-timeout")
	wantRunning(t, outs, true)
}

func TestSend_ToolEventsCarryArgsAndResults(t *testing.T) {
	msg := func(parts string) string {
		return `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":false,` +
			`"status":{"state":"working","message":{"kind":"message","messageId":"m","role":"agent","parts":[` + parts + `]}}}}`
	}
	call := `{"kind":"data","data":{"id":"call_1","name":"k8s_get_resources","args":{"namespace":"kagent","resource_type":"pod"}},"metadata":{"kagent_type":"function_call"}}`
	resp := `{"kind":"data","data":{"id":"call_1","name":"k8s_get_resources","response":{"content":[{"type":"text","text":"NAME READY"}],"isError":false}},"metadata":{"kagent_type":"function_response"}}`
	steps := []ranchertest.Step{step(msg(call)), step(msg(resp)), step(finalState("completed"))}
	e := setup(t, ranchertest.FakeCluster{ChatSteps: steps}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	var tool *chat.Tool
	var res *chat.ToolResult
	for _, o := range outs {
		switch d := o.Data.(type) {
		case chat.Tool:
			tool = &d
		case chat.ToolResult:
			if o.Type != "tool_result" {
				t.Fatalf("result type = %q", o.Type)
			}
			res = &d
		}
	}
	if tool == nil || tool.ID != "call_1" || tool.Name != "k8s_get_resources" || tool.Args == "" || tool.Summary == "" {
		t.Fatalf("tool = %+v", tool)
	}
	if res == nil || res.ID != "call_1" || res.Name != "k8s_get_resources" || !strings.Contains(res.Result, "NAME READY") || res.IsError {
		t.Fatalf("result = %+v", res)
	}
	ti, ri := -1, -1
	for i, o := range outs {
		if o.Type == "tool" {
			ti = i
		}
		if o.Type == "tool_result" {
			ri = i
		}
	}
	if ti > ri {
		t.Fatalf("tool after tool_result: %v", types(outs))
	}
}

func TestSend_ExpiredHasCode(t *testing.T) {
	e := setup(t, ranchertest.FakeCluster{ChatStatus: 401}, baseCfg())
	outs := e.send(t, context.Background(), "hi", a2a.NewUUID())
	var got *chat.Error
	for _, o := range outs {
		if d, ok := o.Data.(chat.Error); ok {
			got = &d
		}
	}
	if got == nil || got.Code != "expired" {
		t.Fatalf("error = %+v", got)
	}
	e = setup(t, ranchertest.FakeCluster{ChatStatus: 403}, baseCfg())
	for _, o := range e.send(t, context.Background(), "hi", a2a.NewUUID()) {
		if d, ok := o.Data.(chat.Error); ok && d.Code != "" {
			t.Fatalf("403 code = %q", d.Code)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		200 * time.Millisecond:       "1 second",
		time.Second:                  "1 second",
		45 * time.Second:             "45 seconds",
		1500 * time.Millisecond:      "2 seconds",
		time.Minute:                  "1 minute",
		10 * time.Minute:             "10 minutes",
		90 * time.Second:             "1 minute 30 seconds",
		time.Hour:                    "1 hour",
		2*time.Hour + 30*time.Minute: "2 hours 30 minutes",
		3*time.Hour + 5*time.Second:  "3 hours 5 seconds",
	} {
		if got := chat.HumanDuration(d); got != want {
			t.Errorf("%v: got %q want %q", d, got, want)
		}
	}
}
