package history_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/history"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

const maxDur = 30 * time.Minute

// docTask is the task from docs/kagent-chat-via-rancher.md section 4, with
// the elided data bodies filled in.
func docTask(state, ts string) string {
	return `{"id":"4cec67da","contextId":"0891727d","kind":"task",
 "status":{"state":"` + state + `","timestamp":"` + ts + `"},
 "metadata":{"kagent_invocation_id":"e-16c16ec7","kagent_user_id":"u"},
 "history":[
   {"role":"user","messageId":"0f95ea83","parts":[{"kind":"text","text":"list pods"}]},
   {"role":"user","messageId":"0f95ea83","parts":[{"kind":"text","text":"list pods"}]},
   {"role":"agent","messageId":"a1","metadata":{"kagent_adk_partial":false},
    "parts":[{"kind":"text","text":"Let me check..."},{"kind":"data","metadata":{"kagent_type":"function_call"},
      "data":{"id":"call_1","name":"k8s_get_resources","args":{"resource_type":"pods","namespace":"default"}}}]},
   {"role":"agent","parts":[{"kind":"data","metadata":{"kagent_type":"function_response"},
      "data":{"id":"call_1","name":"k8s_get_resources","response":{"content":[{"type":"text","text":"pod/a Running"}],"isError":false}}}]},
   {"role":"agent","parts":[{"kind":"text","text":"Final answer ..."}]}],
 "artifacts":[{"artifactId":"x","parts":[{"kind":"text","text":"Final answer ..."}]}]}`
}

const doneTS = "2026-11-20T11:34:22.391479+00:00"

func mustNormalise(t *testing.T, tasks ...string) history.Transcript {
	t.Helper()
	tr, err := history.Normalise([]byte("["+strings.Join(tasks, ",")+"]"), now, maxDur)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func lastItem(tu history.Turn) history.Item { return tu.Items[len(tu.Items)-1] }

func TestNormalise_DocSample(t *testing.T) {
	tr := mustNormalise(t, docTask("completed", doneTS))
	if len(tr.Turns) != 1 || tr.Running {
		t.Fatalf("%+v", tr)
	}
	tu := tr.Turns[0]
	if tu.User != "list pods" || tu.State != "completed" {
		t.Fatalf("turn %+v", tu)
	}
	if len(tu.Items) != 3 {
		t.Fatalf("items %+v", tu.Items)
	}
	if it := tu.Items[0]; it.Kind != "text" || it.Text != "Let me check..." {
		t.Fatalf("item0 %+v", it)
	}
	it := tu.Items[1]
	if it.Kind != "tool" || it.ID != "call_1" || it.Name != "k8s_get_resources" || it.Result != "pod/a Running" ||
		it.IsError || it.Summary != "pods, default" || !strings.Contains(it.Args, `"namespace": "default"`) {
		t.Fatalf("item1 %+v", it)
	}
	if it := tu.Items[2]; it.Kind != "text" || it.Text != "Final answer ..." {
		t.Fatalf("item2 %+v", it)
	}
	b, _ := json.Marshal(tr)
	if strings.Count(string(b), "list pods") != 1 {
		t.Fatalf("user message repeated: %s", b)
	}
	if !strings.Contains(string(b), `"turns":[`) || !strings.Contains(string(b), `"running":false`) ||
		!strings.Contains(string(b), `"kind":"tool"`) || strings.Contains(string(b), `"is_error"`) {
		t.Fatalf("json: %s", b)
	}
}

func TestNormalise_ArtifactFallback(t *testing.T) {
	task := `{"status":{"state":"completed","timestamp":"` + doneTS + `"},
	 "history":[{"role":"user","messageId":"u","parts":[{"kind":"text","text":"hi"}]}],
	 "artifacts":[{"parts":[{"kind":"text","text":"Hello there"}]}]}`
	tr := mustNormalise(t, task)
	if len(tr.Turns) != 1 || len(tr.Turns[0].Items) != 1 {
		t.Fatalf("%+v", tr)
	}
	if it := tr.Turns[0].Items[0]; it.Kind != "text" || it.Text != "Hello there" {
		t.Fatalf("%+v", it)
	}
}

func TestNormalise_RunningLastTask(t *testing.T) {
	recent := now.Add(-time.Minute).Format(time.RFC3339Nano)
	tr := mustNormalise(t, docTask("completed", doneTS), docTask("working", recent))
	if !tr.Running {
		t.Fatal("want running")
	}
	for _, tu := range tr.Turns {
		for _, it := range tu.Items {
			if it.Kind == "notice" {
				t.Fatalf("unexpected notice %+v", it)
			}
		}
	}

	stale := now.Add(-31 * time.Minute).Format(time.RFC3339Nano)
	tr = mustNormalise(t, docTask("working", stale))
	if tr.Running {
		t.Fatal("want not running")
	}
	if it := lastItem(tr.Turns[0]); it.Kind != "notice" || it.Text != "This reply didn't finish." {
		t.Fatalf("%+v", it)
	}
}

func TestNormalise_StaleEarlierTask(t *testing.T) {
	recent := now.Add(-time.Minute).Format(time.RFC3339Nano)
	tr := mustNormalise(t, docTask("submitted", recent), docTask("completed", doneTS))
	if tr.Running {
		t.Fatal("want not running")
	}
	if it := lastItem(tr.Turns[0]); it.Kind != "notice" || it.Text != "This reply didn't finish." {
		t.Fatalf("%+v", it)
	}
	if it := lastItem(tr.Turns[1]); it.Kind == "notice" {
		t.Fatalf("completed turn got notice %+v", it)
	}
}

func TestNormalise_InputRequired(t *testing.T) {
	tr := mustNormalise(t, docTask("input-required", doneTS))
	if it := lastItem(tr.Turns[0]); it.Kind != "notice" ||
		it.Text != "The agent asked for approval, which the portal doesn't support yet." {
		t.Fatalf("%+v", it)
	}
}

func TestNormalise_OrphanResult(t *testing.T) {
	task := `{"status":{"state":"completed","timestamp":"` + doneTS + `"},
	 "history":[{"role":"user","messageId":"u","parts":[{"kind":"text","text":"hi"}]},
	  {"role":"agent","parts":[{"kind":"data","metadata":{"kagent_type":"function_response"},
	    "data":{"id":"call_9","name":"helm_list","response":{"content":[{"type":"text","text":"boom"}],"isError":true}}}]},
	  {"role":"agent","parts":[{"kind":"text","text":"done"}]}]}`
	tr := mustNormalise(t, task)
	items := tr.Turns[0].Items
	if len(items) != 2 {
		t.Fatalf("%+v", items)
	}
	if it := items[0]; it.Kind != "tool" || it.ID != "call_9" || it.Name != "helm_list" || it.Result != "boom" || !it.IsError {
		t.Fatalf("%+v", it)
	}
}

func TestNormalise_ThoughtSkipped(t *testing.T) {
	task := `{"status":{"state":"completed","timestamp":"` + doneTS + `"},
	 "history":[{"role":"user","messageId":"u","parts":[{"kind":"text","text":"hi"}]},
	  {"role":"agent","parts":[{"kind":"text","text":"secret thinking","metadata":{"kagent_thought":true}},
	   {"kind":"text","text":"more","metadata":{"adk_thought":true}},
	   {"kind":"text","text":"visible","metadata":{"kagent_thought":false}}]}]}`
	tr := mustNormalise(t, task)
	items := tr.Turns[0].Items
	if len(items) != 1 || items[0].Text != "visible" {
		t.Fatalf("%+v", items)
	}
}

func TestNormalise_HugeResult(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	resp, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": big}}})
	task := `{"status":{"state":"completed","timestamp":"` + doneTS + `"},
	 "history":[{"role":"user","messageId":"u","parts":[{"kind":"text","text":"hi"}]},
	  {"role":"agent","parts":[{"kind":"data","metadata":{"kagent_type":"function_call"},"data":{"id":"c1","name":"t","args":{}}}]},
	  {"role":"agent","parts":[{"kind":"data","metadata":{"kagent_type":"function_response"},"data":{"id":"c1","name":"t","response":` + string(resp) + `}}]}]}`
	tr := mustNormalise(t, task)
	it := tr.Turns[0].Items[0]
	if len(it.Result) > 64<<10+64 || !strings.HasSuffix(it.Result, "\n… (truncated)") {
		t.Fatalf("result len %d", len(it.Result))
	}
}

func TestNormalise_BadJSON(t *testing.T) {
	if _, err := history.Normalise([]byte(`{"not":"array"}`), now, maxDur); err == nil {
		t.Fatal("want error")
	}
}
