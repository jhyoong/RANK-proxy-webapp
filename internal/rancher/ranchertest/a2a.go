package ranchertest

import (
	"encoding/json"
	"net/http"
	"net/url"
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

func (f *Fake) chat(w http.ResponseWriter, r *http.Request, cl *FakeCluster, user string) {
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
	ctxID, text, ok := parseChat(body)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	if f.chatTurns == nil {
		f.chatTurns = map[string]int{}
	}
	f.chatTurns[ctxID]++
	turn := f.chatTurns[ctxID]
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
	steps := cl.ChatSteps
	if steps == nil {
		steps = DefaultChat()
	}
	esc, _ := json.Marshal(text)
	rep := strings.NewReplacer("{{text}}", string(esc[1:len(esc)-1]), "{{turn}}", strconv.Itoa(turn))
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

// parseChat validates the A2A body and returns contextId and the single text part.
func parseChat(body map[string]any) (ctxID, text string, ok bool) {
	if body["method"] != "message/stream" {
		return
	}
	params, _ := body["params"].(map[string]any)
	msg, _ := params["message"].(map[string]any)
	ctxID, _ = msg["contextId"].(string)
	parts, _ := msg["parts"].([]any)
	if ctxID == "" || len(parts) != 1 {
		return "", "", false
	}
	p, _ := parts[0].(map[string]any)
	if p["kind"] != "text" {
		return "", "", false
	}
	text, ok = p["text"].(string)
	return
}
