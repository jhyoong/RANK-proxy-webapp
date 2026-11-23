package a2a

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormatResult(t *testing.T) {
	cases := []struct {
		name, in, text string
		isErr          bool
	}{
		{"mcp", `{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"isError":true}`, "a\nb", true},
		{"result string", `{"result":"ok"}`, "ok", false},
		{"result object", `{"result":{"x":1}}`, "{\n  \"x\": 1\n}", false},
		{"weird", `{"weird":1}`, "{\n  \"weird\": 1\n}", false},
	}
	for _, c := range cases {
		got, e := FormatResult(json.RawMessage(c.in))
		if got != c.text || e != c.isErr {
			t.Errorf("%s: got %q,%v want %q,%v", c.name, got, e, c.text, c.isErr)
		}
	}
}

func TestFormatResult_Truncated(t *testing.T) {
	// "é" is 2 bytes; an odd offset puts the 64 KiB cut inside a rune.
	text := "x" + strings.Repeat("é", 35000)
	raw, _ := json.Marshal(map[string]any{"result": text})
	got, _ := FormatResult(raw)
	if len(got) > 65536+len(truncatedMarker) || !strings.HasSuffix(got, "… (truncated)") || !utf8.ValidString(got) {
		t.Fatalf("len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestFormatArgs(t *testing.T) {
	if got := FormatArgs(json.RawMessage(`null`)); got != "" {
		t.Fatalf("null = %q", got)
	}
	if got := FormatArgs(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	raw, _ := json.Marshal(map[string]string{"k": strings.Repeat("a", 10<<10)})
	got := FormatArgs(raw)
	if len(got) > 8192+len(truncatedMarker) || !strings.HasSuffix(got, "… (truncated)") {
		t.Fatalf("len=%d", len(got))
	}
	if got := FormatArgs(json.RawMessage(`{"a":1}`)); got != "{\n  \"a\": 1\n}" {
		t.Fatalf("small = %q", got)
	}
}

func TestArgsSummary(t *testing.T) {
	got := ArgsSummary(json.RawMessage(`{"resource_type":"pod","namespace":"kube-system","output":"wide","n":3}`))
	if got != "pod, kube-system, wide" {
		t.Fatalf("got %q", got)
	}
	if got := ArgsSummary(json.RawMessage(`{"o":{"a":1},"n":3,"s":"x"}`)); got != "3, x" {
		t.Fatalf("got %q", got)
	}
	if got := ArgsSummary(json.RawMessage(`[1]`)); got != "" {
		t.Fatalf("got %q", got)
	}
	long := ArgsSummary(json.RawMessage(`{"a":"` + strings.Repeat("é", 100) + `"}`))
	if utf8.RuneCountInString(long) > 60 {
		t.Fatalf("len=%d", utf8.RuneCountInString(long))
	}
}
