package a2a

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func parseOne(t *testing.T, js string) Event {
	t.Helper()
	r := NewReader(strings.NewReader("data: " + strings.ReplaceAll(js, "\n", "") + "\n\n"))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

func TestReader_Basic(t *testing.T) {
	in := "data: {\"result\":{\"kind\":\"task\",\"id\":\"a\"}}\n\n: ping\n\nevent: x\nid: 1\nretry: 5\ndata:{\"result\":{\"kind\":\"task\",\"id\":\"b\"}}\n\n"
	r := NewReader(strings.NewReader(in))
	var ids []string
	for {
		ev, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.TaskID)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestReader_MultiLineDataAndCRLF(t *testing.T) {
	in := "data: {\"result\":\r\ndata: {\"kind\":\"task\",\"id\":\"z\"}}\r\n\r\n"
	ev, err := NewReader(strings.NewReader(in)).Next()
	if err != nil || ev.TaskID != "z" {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
}

func TestReader_Oversized(t *testing.T) {
	in := "data: " + strings.Repeat("a", 1<<20+1) + "\n\n"
	_, err := NewReader(strings.NewReader(in)).Next()
	if !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestReader_EOF(t *testing.T) {
	if _, err := NewReader(strings.NewReader("")).Next(); err != io.EOF {
		t.Fatalf("err = %v", err)
	}
	r := NewReader(strings.NewReader("data: {\"result\":{\"kind\":\"task\",\"id\":\"q\"}}"))
	ev, err := r.Next()
	if err != nil || ev.TaskID != "q" {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("err = %v", err)
	}
}

func TestParse_StatusUpdateWithTextAndTool(t *testing.T) {
	ev := parseOne(t, `{"jsonrpc":"2.0","id":"1","result":{"kind":"status-update","taskId":"t1","contextId":"c1","final":false,
"status":{"state":"working","message":{"role":"agent","metadata":{"kagent_adk_partial":false},"parts":[
{"kind":"text","text":"SECRET-TEXT"},
{"kind":"data","metadata":{"kagent_type":"function_call"},"data":{"id":"x","name":"k8s_get_resources","args":{"a":"SECRET-ARG"}}},
{"kind":"data","metadata":{"kagent_type":"function_response"},"data":{"name":"k8s_get_resources"}}]}}}}`)
	if ev.Kind != "status-update" || ev.TaskID != "t1" || ev.ContextID != "c1" || ev.State != "working" || ev.Role != "agent" || ev.Final {
		t.Fatalf("ev = %+v", ev)
	}
	if len(ev.Texts) != 1 || ev.Texts[0].Text != "SECRET-TEXT" || ev.Texts[0].Partial {
		t.Fatalf("texts = %+v", ev.Texts)
	}
	if len(ev.Tools) != 1 || ev.Tools[0] != "k8s_get_resources" {
		t.Fatalf("tools = %v", ev.Tools)
	}
	if !strings.Contains(ev.Shape, "meta=") || !strings.Contains(ev.Shape, "kagent_type") ||
		!strings.Contains(ev.Shape, "status-update") || !strings.Contains(ev.Shape, "state=working") {
		t.Fatalf("shape = %q", ev.Shape)
	}
	for _, bad := range []string{"SECRET", "function_call", "role"} {
		if strings.Contains(ev.Shape, bad) {
			t.Fatalf("shape leaks %q: %q", bad, ev.Shape)
		}
	}
}

func TestParse_PartialText(t *testing.T) {
	cases := []struct {
		name, msgMeta, partMeta string
		want                    bool
	}{
		{"msg kagent true", `{"kagent_adk_partial":true}`, `null`, true},
		{"msg adk true", `{"adk_partial":true}`, `null`, true},
		{"part kagent true", `null`, `{"kagent_adk_partial":true}`, true},
		{"part adk true", `null`, `{"adk_partial":true}`, true},
		{"false", `{"kagent_adk_partial":false}`, `{"adk_partial":false}`, false},
		{"missing", `null`, `null`, false},
	}
	for _, c := range cases {
		ev := parseOne(t, `{"result":{"kind":"status-update","status":{"state":"working","message":{"role":"agent","metadata":`+c.msgMeta+`,"parts":[{"kind":"text","text":"hi","metadata":`+c.partMeta+`}]}}}}`)
		if len(ev.Texts) != 1 || ev.Texts[0].Partial != c.want {
			t.Errorf("%s: %+v", c.name, ev.Texts)
		}
	}
}

func TestParse_UserEcho(t *testing.T) {
	ev := parseOne(t, `{"result":{"kind":"status-update","taskId":"t","status":{"state":"submitted","message":{"role":"user","parts":[{"kind":"text","text":"hello"}]}}}}`)
	if ev.Role != "user" || ev.State != "submitted" || len(ev.Texts) != 1 || ev.Texts[0].Text != "hello" {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParse_ArtifactUpdate(t *testing.T) {
	ev := parseOne(t, `{"result":{"kind":"artifact-update","taskId":"t","contextId":"c","artifact":{"parts":[{"kind":"text","text":"answer"}]}}}`)
	if ev.Kind != "artifact-update" || ev.TaskID != "t" || len(ev.Texts) != 1 || ev.Texts[0].Text != "answer" || ev.Final {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParse_FinalCompleted(t *testing.T) {
	ev := parseOne(t, `{"result":{"kind":"status-update","taskId":"t","final":true,"status":{"state":"completed"}}}`)
	if !ev.Final || ev.State != "completed" {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParse_Task(t *testing.T) {
	ev := parseOne(t, `{"result":{"kind":"task","id":"tid","contextId":"c","status":{"state":"submitted"}}}`)
	if ev.Kind != "task" || ev.TaskID != "tid" || ev.State != "submitted" {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParse_Message(t *testing.T) {
	ev := parseOne(t, `{"result":{"kind":"message","contextId":"c","role":"agent","parts":[{"kind":"text","text":"yo"}]}}`)
	if ev.Kind != "message" || !ev.Final || ev.Role != "agent" || ev.ContextID != "c" || len(ev.Texts) != 1 {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParse_JSONRPCError(t *testing.T) {
	ev := parseOne(t, `{"jsonrpc":"2.0","id":"1","error":{"code":-32602,"message":"bad params"}}`)
	if ev.Kind != "error" || ev.ErrCode != -32602 || ev.ErrMessage != "bad params" {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParse_UnknownKindIgnored(t *testing.T) {
	ev := parseOne(t, `{"result":{"kind":"something"}}`)
	if ev.Kind != "" {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestParse_BadJSON(t *testing.T) {
	_, err := NewReader(strings.NewReader("data: {nope\n\n")).Next()
	if !errors.Is(err, ErrBadEvent) {
		t.Fatalf("err = %v", err)
	}
}
