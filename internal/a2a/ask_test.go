package a2a

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// askPart is a kagent (Python ADK) ask_user confirmation data part.
const askPart = `{"kind":"data","metadata":{"kagent_type":"function_call","kagent_is_long_running":true},
"data":{"name":"adk_request_confirmation","id":"adk-conf-1","args":{
 "originalFunctionCall":{"name":"ask_user","id":"call_abc","args":{"questions":[
   {"question":"Which env?","choices":["dev","prod"],"multiple":false},
   {"question":"Anything else?"}]}},
 "toolConfirmation":{"hint":"Which env?; Anything else?","confirmed":false,"payload":null}}}}`

func meta(t *testing.T, part string) (map[string]json.RawMessage, json.RawMessage) {
	t.Helper()
	var p rawPart
	if err := json.Unmarshal([]byte(part), &p); err != nil {
		t.Fatal(err)
	}
	return p.Metadata, p.Data
}

func TestParseAsk_AskUser(t *testing.T) {
	a := ParseAsk(meta(t, askPart))
	if a == nil {
		t.Fatal("want ask")
	}
	want := &Ask{ID: "adk-conf-1", Questions: []Question{
		{Question: "Which env?", Choices: []string{"dev", "prod"}},
		{Question: "Anything else?"},
	}}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("got %+v", a)
	}
}

func TestParseAsk_ADKPrefix(t *testing.T) {
	p := strings.NewReplacer(`"kagent_type"`, `"adk_type"`, `"kagent_is_long_running"`, `"adk_is_long_running"`).Replace(askPart)
	if a := ParseAsk(meta(t, p)); a == nil || len(a.Questions) != 2 {
		t.Fatalf("got %+v", a)
	}
}

func TestParseAsk_Subagent(t *testing.T) {
	p := `{"kind":"data","metadata":{"kagent_type":"function_call","kagent_is_long_running":true},
"data":{"name":"adk_request_confirmation","id":"adk-conf-2","args":{
 "originalFunctionCall":{"name":"helper_agent","id":"call_sub","args":{"request":"x"}},
 "toolConfirmation":{"confirmed":false,"payload":{"subagent_name":"helper","hitl_parts":[
   {"name":"adk_request_confirmation","id":"inner","originalFunctionCall":{"name":"ask_user","id":"c2",
     "args":{"questions":[{"question":"Pick","choices":["a","b"],"multiple":true}]}}}]}}}}}`
	a := ParseAsk(meta(t, p))
	want := &Ask{ID: "adk-conf-2", Subagent: "helper", Questions: []Question{{Question: "Pick", Choices: []string{"a", "b"}, Multiple: true}}}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("got %+v", a)
	}
}

func TestParseAsk_NotAsk(t *testing.T) {
	cases := map[string]string{
		"approval":         strings.Replace(askPart, `"name":"ask_user"`, `"name":"k8s_delete_resource"`, 1),
		"not long running": strings.Replace(askPart, `"kagent_is_long_running":true`, `"kagent_is_long_running":false`, 1),
		"plain call":       `{"kind":"data","metadata":{"kagent_type":"function_call"},"data":{"id":"x","name":"ask_user","args":{}}}`,
		"response":         strings.Replace(askPart, `"function_call"`, `"function_response"`, 1),
	}
	for name, p := range cases {
		if a := ParseAsk(meta(t, p)); a != nil {
			t.Errorf("%s: got %+v", name, a)
		}
	}
}

func TestParseDataPart_HidesHITLInternals(t *testing.T) {
	hidden := map[string]string{
		"confirmation call": askPart,
		"credential call":   `{"kind":"data","metadata":{"kagent_type":"function_call"},"data":{"id":"x","name":"adk_request_credential","args":{}}}`,
		"confirmation response": `{"kind":"data","metadata":{"kagent_type":"function_response"},
			"data":{"id":"adk-conf-1","name":"adk_request_confirmation","response":{"response":"{}"}}}`,
		"ask_user pending stub": `{"kind":"data","metadata":{"kagent_type":"function_response"},
			"data":{"id":"call_abc","name":"ask_user","response":{"status":"pending","questions":[]}}}`,
		"approval stub": `{"kind":"data","metadata":{"kagent_type":"function_response"},
			"data":{"id":"c","name":"k8s_delete_resource","response":{"status":"confirmation_requested","tool":"k8s_delete_resource"}}}`,
	}
	for name, p := range hidden {
		if c, r := ParseDataPart(meta(t, p)); c != nil || r != nil {
			t.Errorf("%s: got call=%+v result=%+v", name, c, r)
		}
	}
	// The ask_user call itself and its final answer stay visible as a tool.
	c, _ := ParseDataPart(meta(t, `{"kind":"data","metadata":{"kagent_type":"function_call"},"data":{"id":"call_abc","name":"ask_user","args":{}}}`))
	_, r := ParseDataPart(meta(t, `{"kind":"data","metadata":{"kagent_type":"function_response"},"data":{"id":"call_abc","name":"ask_user","response":{"result":"[]"}}}`))
	if c == nil || r == nil {
		t.Fatalf("ask_user call/result hidden: %v %v", c, r)
	}
}

func TestParseEvent_InputRequiredAsk(t *testing.T) {
	ev, err := parseEvent([]byte(`{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"T1","contextId":"S1","final":true,
"status":{"state":"input-required","message":{"kind":"message","role":"agent","messageId":"m","parts":[` + askPart + `]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.State != "input-required" || !ev.Final || len(ev.Asks) != 1 || len(ev.Calls) != 0 || ev.Asks[0].ID != "adk-conf-1" {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParseDecision(t *testing.T) {
	d := ParseDecision(json.RawMessage(`{"decision_type":"approve","ask_user_answers":[{"answer":["prod"]},{"answer":[]},{}]}`))
	if d == nil || d.Type != "approve" || !reflect.DeepEqual(d.Answers, [][]string{{"prod"}, {}, {}}) {
		t.Fatalf("got %+v", d)
	}
	if d := ParseDecision(json.RawMessage(`{"decision_type":"reject"}`)); d == nil || d.Type != "reject" || d.Answers != nil {
		t.Fatalf("reject: %+v", d)
	}
	if d := ParseDecision(json.RawMessage(`{"foo":1}`)); d != nil {
		t.Fatalf("not a decision: %+v", d)
	}
}

func TestValidTaskID(t *testing.T) {
	for _, s := range []string{"T1", "4cec67da-1234-4abc-8def-0123456789ab", "task_1.a:b"} {
		if !ValidTaskID(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "a b", "../x", "x/y", strings.Repeat("a", 129), "é"} {
		if ValidTaskID(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}
