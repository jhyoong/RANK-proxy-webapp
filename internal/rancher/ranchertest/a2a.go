package ranchertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Step is one piece of a scripted A2A stream.
type Step struct {
	Delay time.Duration
	Data  string // one SSE data payload (JSON); "{{text}}" and "{{turn}}" are substituted
	Raw   string // if set, written verbatim instead of "data: <Data>\n\n"
}

// DefaultChat returns a fresh copy of the kagent-shaped script: submitted echo,
// working, tool call, two partial texts, complete step, artifact, completed.
func DefaultChat() []Step {
	const d = 20 * time.Millisecond
	msg := func(partial, parts string) string {
		return `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":false,` +
			`"status":{"state":"working","message":{"kind":"message","messageId":"m","role":"agent",` +
			`"metadata":{"kagent_adk_partial":` + partial + `},"parts":[` + parts + `]}}}}`
	}
	text := func(s string) string { return `{"kind":"text","text":"` + s + `"}` }
	whole := `Answer to \"{{text}}\" (turn {{turn}})`
	return []Step{
		{Delay: d, Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":false,` +
			`"status":{"state":"submitted","message":{"kind":"message","messageId":"u","role":"user","parts":[{"kind":"text","text":"{{text}}"}]}}}}`},
		{Delay: d, Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":false,` +
			`"status":{"state":"working"},"metadata":{"kagent_session_id":"s1","kagent_user_id":"u1"}}}`},
		{Delay: d, Data: msg("false", `{"kind":"data","data":{"id":"call_1","name":"k8s_get_resources","args":{}},"metadata":{"kagent_type":"function_call"}}`)},
		{Delay: d, Data: msg("true", text(`Answer to \"{{text}}\"`))},
		{Delay: d, Data: msg("true", text(` (turn {{turn}})`))},
		{Delay: d, Data: msg("false", text(whole))},
		{Delay: d, Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"artifact-update","taskId":"t1","contextId":"c",` +
			`"artifact":{"artifactId":"a1","parts":[` + text(whole) + `]}}}`},
		{Delay: d, Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c","final":true,` +
			`"status":{"state":"completed"}}}`},
	}
}

// AskTrigger in a user message makes the fake agent pause on an ask_user
// question (AskChat) instead of running its script.
const AskTrigger = "ask me"

// AskConfirmation is the long-running adk_request_confirmation data part that
// kagent's Python ADK sends when the ask_user tool pauses a task.
const AskConfirmation = `{"kind":"data","metadata":{"kagent_type":"function_call","kagent_is_long_running":true},` +
	`"data":{"name":"adk_request_confirmation","id":"adk-conf-1","args":{` +
	`"originalFunctionCall":{"name":"ask_user","id":"call_ask","args":{"questions":[` +
	`{"question":"Which namespace should I check?","choices":["default","kube-system"],"multiple":false},` +
	`{"question":"Which resources?","choices":["pods","deployments","services"],"multiple":true}]}},` +
	`"toolConfirmation":{"hint":"Which namespace should I check?; Which resources?","confirmed":false,"payload":null}}}}`

// AskChat returns the script of a turn that pauses on an ask_user question:
// submitted, working, the ask_user call and its pending stub, then a final
// input-required status carrying AskConfirmation. "{{task}}" is the task ID.
func AskChat() []Step {
	const d = 20 * time.Millisecond
	upd := func(final, state, parts string) string {
		return `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"{{task}}","contextId":"c","final":` + final +
			`,"status":{"state":"` + state + `","message":{"kind":"message","messageId":"m","role":"agent","parts":[` + parts + `]}}}}`
	}
	return []Step{
		{Delay: d, Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"{{task}}","contextId":"c","final":false,` +
			`"status":{"state":"submitted","message":{"kind":"message","messageId":"u","role":"user","parts":[{"kind":"text","text":"{{text}}"}]}}}}`},
		{Delay: d, Data: upd("false", "working", `{"kind":"text","text":"I need a little more detail first."}`)},
		{Delay: d, Data: upd("false", "working", `{"kind":"data","metadata":{"kagent_type":"function_call"},`+
			`"data":{"id":"call_ask","name":"ask_user","args":{"questions":[{"question":"Which namespace should I check?"}]}}}`)},
		{Delay: d, Data: upd("false", "working", `{"kind":"data","metadata":{"kagent_type":"function_response"},`+
			`"data":{"id":"call_ask","name":"ask_user","response":{"status":"pending","questions":[]}}}`)},
		{Delay: d, Data: upd("true", "input-required", AskConfirmation)},
	}
}

// ResumeChat returns the script of a resumed ask_user task: the decision
// echo, the ask_user result, an answer quoting the answers ("{{text}}"), and
// completed. "{{task}}" is the task ID.
func ResumeChat() []Step {
	const d = 20 * time.Millisecond
	upd := func(parts string) string {
		return `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"{{task}}","contextId":"c","final":false,` +
			`"status":{"state":"working","message":{"kind":"message","messageId":"m","role":"agent","parts":[` + parts + `]}}}}`
	}
	return []Step{
		{Delay: d, Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"{{task}}","contextId":"c","final":false,` +
			`"status":{"state":"working","message":{"kind":"message","messageId":"u","role":"user","parts":[` +
			`{"kind":"data","data":{"decision_type":"approve"}},{"kind":"text","text":"Answered questions"}]}}}}`},
		{Delay: d, Data: upd(`{"kind":"data","metadata":{"kagent_type":"function_response"},` +
			`"data":{"id":"call_ask","name":"ask_user","response":{"result":"answered"}}}`)},
		{Delay: d, Data: upd(`{"kind":"text","text":"Thanks, you chose {{text}}."}`)},
		{Delay: d, Data: `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"{{task}}","contextId":"c","final":true,` +
			`"status":{"state":"completed"}}}`},
	}
}

// ChatCalls returns how many A2A POSTs reached a known cluster.
func (f *Fake) ChatCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatCalls
}

// LastChatBody returns the decoded body of the most recent A2A POST.
func (f *Fake) LastChatBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatBody
}

// LastChatHeaders returns a copy of the headers of the most recent A2A POST.
func (f *Fake) LastChatHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatHeaders.Clone()
}

// LastChatURL returns a copy of the URL of the most recent A2A POST.
func (f *Fake) LastChatURL() *url.URL {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chatURL == nil {
		return nil
	}
	u := *f.chatURL
	return &u
}

// ChatCancelled counts chat requests whose context ended before all steps were sent.
func (f *Fake) ChatCancelled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatCancelled
}

func (f *Fake) chat(w http.ResponseWriter, r *http.Request, cl *FakeCluster, user, agentNS, agentName string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.chatCalls++
	f.chatBody = body
	f.chatHeaders = r.Header.Clone()
	u := *r.URL
	f.chatURL = &u
	f.mu.Unlock()
	if cl.Deny[user] {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if cl.ChatStatus != 0 {
		http.Error(w, "forced", cl.ChatStatus)
		return
	}
	ctxID, taskID, text, answers, ok := parseChat(body)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	steps := cl.ChatSteps
	f.mu.Lock()
	if f.chatTurns == nil {
		f.chatTurns = map[string]int{}
	}
	f.chatTurns[ctxID]++
	turn := f.chatTurns[ctxID]
	task := fmt.Sprintf("task-%s-%d", ctxID, turn)
	switch {
	case taskID != "":
		task = taskID
		text = fmt.Sprint(answers)
		steps = ResumeChat()
		resumeSession(cl, ctxID, taskID, answers)
	case strings.Contains(strings.ToLower(text), AskTrigger):
		steps = AskChat()
		recordAsk(cl, agentID(agentNS, agentName), ctxID, task, text)
	default:
		recordSession(cl, agentID(agentNS, agentName), ctxID, text, turn)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	flush()
	cancelled := func() {
		f.mu.Lock()
		f.chatCancelled++
		f.mu.Unlock()
	}
	if cl.ChatHang {
		<-r.Context().Done()
		cancelled()
		return
	}
	if steps == nil {
		steps = DefaultChat()
	}
	esc, _ := json.Marshal(text)
	rep := strings.NewReplacer("{{text}}", string(esc[1:len(esc)-1]), "{{turn}}", strconv.Itoa(turn), "{{task}}", task)
	for _, s := range steps {
		if s.Delay > 0 {
			select {
			case <-time.After(s.Delay):
			case <-r.Context().Done():
				cancelled()
				return
			}
		}
		if r.Context().Err() != nil {
			cancelled()
			return
		}
		if s.Raw != "" {
			_, _ = w.Write([]byte(rep.Replace(s.Raw)))
		} else {
			_, _ = w.Write([]byte("data: " + rep.Replace(s.Data) + "\n\n"))
		}
		flush()
	}
}

// parseChat validates the A2A body. A plain message has a single text part.
// A HITL resume (kagent's ask_user answer) has a taskId, a decision data part
// and a text part; it returns the answers (nil for a reject, i.e. skip).
func parseChat(body map[string]any) (ctxID, taskID, text string, answers [][]string, ok bool) {
	if body["method"] != "message/stream" {
		return
	}
	params, _ := body["params"].(map[string]any)
	msg, _ := params["message"].(map[string]any)
	ctxID, _ = msg["contextId"].(string)
	taskID, _ = msg["taskId"].(string)
	parts, _ := msg["parts"].([]any)
	if ctxID == "" {
		return "", "", "", nil, false
	}
	if taskID != "" {
		if len(parts) != 2 {
			return "", "", "", nil, false
		}
		d, _ := parts[0].(map[string]any)
		data, _ := d["data"].(map[string]any)
		list, isList := data["ask_user_answers"].([]any)
		skip := data["decision_type"] == "reject"
		if d["kind"] != "data" || !(skip || data["decision_type"] == "approve" && isList) {
			return "", "", "", nil, false
		}
		for _, a := range list {
			m, _ := a.(map[string]any)
			vals, _ := m["answer"].([]any)
			var ss []string
			for _, v := range vals {
				s, _ := v.(string)
				ss = append(ss, s)
			}
			answers = append(answers, ss)
		}
		parts = parts[1:]
	}
	if len(parts) != 1 {
		return "", "", "", nil, false
	}
	p, _ := parts[0].(map[string]any)
	if p["kind"] != "text" {
		return "", "", "", nil, false
	}
	text, ok = p["text"].(string)
	return
}

// agentID is kagent's session agent_id for an agent: "<ns>__NS__<name>" with
// '-' in the name replaced by '_'.
func agentID(ns, name string) string {
	return ns + "__NS__" + strings.ReplaceAll(name, "-", "_")
}

// recordSession stores the DefaultChat turn as a completed kagent task on the
// session for ctxID, creating the session on its first turn. Caller holds f.mu.
func recordSession(cl *FakeCluster, agent, ctxID, text string, turn int) {
	now := time.Now().UTC()
	final := `Answer to "` + text + `" (turn ` + strconv.Itoa(turn) + `)`
	textPart := func(s string) map[string]any { return map[string]any{"kind": "text", "text": s} }
	userMsg := map[string]any{"kind": "message", "role": "user", "messageId": fmt.Sprintf("u-%s-%d", ctxID, turn),
		"parts": []any{textPart(text)}}
	task := map[string]any{
		"id": fmt.Sprintf("task-%s-%d", ctxID, turn), "contextId": ctxID, "kind": "task",
		"status": map[string]any{"state": "completed", "timestamp": now.Format(time.RFC3339Nano)},
		// kagent stores the user message twice per task.
		"history": []any{userMsg, userMsg,
			map[string]any{"kind": "message", "role": "agent", "messageId": fmt.Sprintf("a1-%s-%d", ctxID, turn), "parts": []any{
				map[string]any{"kind": "data", "metadata": map[string]any{"kagent_type": "function_call"},
					"data": map[string]any{"id": "call_1", "name": "k8s_get_resources", "args": map[string]any{}}}}},
			map[string]any{"kind": "message", "role": "agent", "messageId": fmt.Sprintf("a2-%s-%d", ctxID, turn), "parts": []any{
				map[string]any{"kind": "data", "metadata": map[string]any{"kagent_type": "function_response"},
					"data": map[string]any{"id": "call_1", "name": "k8s_get_resources", "response": map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "pod/web-1   Running"}}, "isError": false}}}}},
			map[string]any{"kind": "message", "role": "agent", "messageId": fmt.Sprintf("a3-%s-%d", ctxID, turn),
				"parts": []any{textPart(final)}},
		},
		"artifacts": []any{map[string]any{"artifactId": "a1", "parts": []any{textPart(final)}}},
	}
	appendTask(cl, agent, ctxID, text, task, now)
}

// appendTask adds task to the session for ctxID, creating the session (named
// after text) if it is new. Caller holds f.mu.
func appendTask(cl *FakeCluster, agent, ctxID, text string, task map[string]any, now time.Time) {
	for i := range cl.Sessions {
		s := &cl.Sessions[i]
		if s.ID != ctxID {
			continue
		}
		var tasks []any
		_ = json.Unmarshal([]byte(s.TasksJSON), &tasks)
		b, _ := json.Marshal(append(tasks, task))
		s.TasksJSON, s.Updated = string(b), now
		return
	}
	name := text
	if r := []rune(name); len(r) > 20 {
		name = string(r[:20]) + "..."
	}
	b, _ := json.Marshal([]any{task})
	cl.Sessions = append(cl.Sessions, FakeSession{ID: ctxID, Name: name, AgentID: agent, Updated: now, TasksJSON: string(b)})
}

// recordAsk stores an AskChat turn as an input-required task, as kagent does:
// the confirmation is in the status message, not yet in the history. Caller
// holds f.mu.
func recordAsk(cl *FakeCluster, agent, ctxID, taskID, text string) {
	now := time.Now().UTC()
	var conf any
	_ = json.Unmarshal([]byte(AskConfirmation), &conf)
	userMsg := map[string]any{"kind": "message", "role": "user", "messageId": "u-" + taskID,
		"parts": []any{map[string]any{"kind": "text", "text": text}}}
	task := map[string]any{
		"id": taskID, "contextId": ctxID, "kind": "task",
		"status": map[string]any{"state": "input-required", "timestamp": now.Format(time.RFC3339Nano),
			"message": map[string]any{"kind": "message", "role": "agent", "messageId": "conf-" + taskID, "parts": []any{conf}}},
		"history": []any{userMsg, userMsg},
	}
	appendTask(cl, agent, ctxID, text, task, now)
}

// resumeSession completes the input-required task taskID with the user's
// answers: the confirmation moves to the history, followed by the decision
// and the reply. Caller holds f.mu.
func resumeSession(cl *FakeCluster, ctxID, taskID string, answers [][]string) {
	now := time.Now().UTC()
	for i := range cl.Sessions {
		s := &cl.Sessions[i]
		if s.ID != ctxID {
			continue
		}
		var tasks []map[string]any
		_ = json.Unmarshal([]byte(s.TasksJSON), &tasks)
		for _, t := range tasks {
			if t["id"] != taskID {
				continue
			}
			st, _ := t["status"].(map[string]any)
			hist, _ := t["history"].([]any)
			if m := st["message"]; m != nil {
				hist = append(hist, m)
			}
			decision := map[string]any{"decision_type": "reject"}
			if answers != nil {
				list := make([]any, len(answers))
				for j, a := range answers {
					list[j] = map[string]any{"answer": a}
				}
				decision = map[string]any{"decision_type": "approve", "ask_user_answers": list}
			}
			hist = append(hist,
				map[string]any{"kind": "message", "role": "user", "messageId": "d-" + taskID, "parts": []any{
					map[string]any{"kind": "data", "data": decision},
					map[string]any{"kind": "text", "text": "Answered questions"}}},
				map[string]any{"kind": "message", "role": "agent", "messageId": "r-" + taskID, "parts": []any{
					map[string]any{"kind": "text", "text": fmt.Sprintf("Thanks, you chose %v.", answers)}}})
			t["history"] = hist
			t["status"] = map[string]any{"state": "completed", "timestamp": now.Format(time.RFC3339Nano)}
		}
		b, _ := json.Marshal(tasks)
		s.TasksJSON, s.Updated = string(b), now
		return
	}
}

// LastSessionsURL returns a copy of the URL of the most recent /api/sessions* request.
func (f *Fake) LastSessionsURL() *url.URL {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessionsURL == nil {
		return nil
	}
	u := *f.sessionsURL
	return &u
}

// LastSessionsHeaders returns a copy of the headers of the most recent /api/sessions* request.
func (f *Fake) LastSessionsHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionsHeaders.Clone()
}

// TasksCalls returns how many api/sessions/{id}/tasks requests reached a known cluster.
func (f *Fake) TasksCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasksCalls
}

// sessionsPrelude records the request and applies the Deny, SessionStatus and TasksStatus
// knobs. It reports whether the handler should go on.
func (f *Fake) sessionsPrelude(w http.ResponseWriter, r *http.Request, cl *FakeCluster, user string, tasks bool) bool {
	f.mu.Lock()
	u := *r.URL
	f.sessionsURL, f.sessionsHeaders = &u, r.Header.Clone()
	if tasks {
		f.tasksCalls++
	}
	f.mu.Unlock()
	if cl.Deny[user] {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if cl.SessionStatus != 0 {
		http.Error(w, "forced", cl.SessionStatus)
		return false
	}
	if tasks && cl.TasksStatus != 0 {
		http.Error(w, "forced", cl.TasksStatus)
		return false
	}
	return true
}

// sessionList serves GET api/sessions/agent/{ns}/{name}: the sessions for that
// agent (or with no AgentID), newest first, in kagent's wrapped shape.
func (f *Fake) sessionList(w http.ResponseWriter, r *http.Request, cl *FakeCluster, user, ns, name string) {
	if !f.sessionsPrelude(w, r, cl, user, false) {
		return
	}
	want := agentID(ns, name)
	f.mu.Lock()
	var ss []FakeSession
	for _, s := range cl.Sessions {
		if s.AgentID == "" || s.AgentID == want {
			ss = append(ss, s)
		}
	}
	f.mu.Unlock()
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].Updated.After(ss[j].Updated) })
	data := make([]map[string]any, 0, len(ss))
	for _, s := range ss {
		d := map[string]any{"id": s.ID, "name": s.Name, "agent_id": s.AgentID}
		if !s.Updated.IsZero() {
			ts := s.Updated.UTC().Format(time.RFC3339Nano)
			d["created_at"], d["updated_at"] = ts, ts
		}
		data = append(data, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"error": false, "data": data})
}

// sessionTasks serves GET api/sessions/{id}/tasks.
func (f *Fake) sessionTasks(w http.ResponseWriter, r *http.Request, cl *FakeCluster, user, id string) {
	if !f.sessionsPrelude(w, r, cl, user, true) {
		return
	}
	f.mu.Lock()
	tasks, found := "", false
	for _, s := range cl.Sessions {
		if s.ID == id {
			tasks, found = s.TasksJSON, true
		}
	}
	f.mu.Unlock()
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": true, "message": "not found"})
		return
	}
	if strings.TrimSpace(tasks) == "" {
		tasks = "[]"
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"error":false,"data":` + tasks + `}`))
}
