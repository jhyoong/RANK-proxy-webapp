package a2a

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	maxArgsBytes    = 8 << 10
	maxResultBytes  = 64 << 10
	truncatedMarker = "\n… (truncated)"
	maxSummaryRunes = 60
	maxSummaryVals  = 3
)

// ToolCall is a function_call data part.
type ToolCall struct {
	ID, Name string
	Args     json.RawMessage
}

// ToolResult is a function_response data part.
type ToolResult struct {
	ID, Name string
	Response json.RawMessage
}

func metaType(m map[string]json.RawMessage) string {
	for k, v := range m {
		if strings.HasSuffix(k, "_type") {
			var s string
			if json.Unmarshal(v, &s) == nil && (s == "function_call" || s == "function_response") {
				return s
			}
		}
	}
	return ""
}

// ParseDataPart classifies one A2A data part. Both returns are nil for a part
// that is not a tool call or result.
func ParseDataPart(meta map[string]json.RawMessage, data json.RawMessage) (call *ToolCall, result *ToolResult) {
	typ := metaType(meta)
	if typ == "" {
		return nil, nil
	}
	var d struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Args     json.RawMessage `json:"args"`
		Response json.RawMessage `json:"response"`
	}
	_ = json.Unmarshal(data, &d) // a malformed body still yields an empty call
	if typ == "function_call" {
		return &ToolCall{ID: d.ID, Name: d.Name, Args: d.Args}, nil
	}
	return nil, &ToolResult{ID: d.ID, Name: d.Name, Response: d.Response}
}

// truncate cuts s to at most max bytes on a rune boundary and appends the marker.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}

func isEmptyJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

func indent(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return string(raw)
	}
	return b.String()
}

// FormatArgs pretty-prints args JSON, capped at 8 KiB. Empty or null gives "".
func FormatArgs(raw json.RawMessage) string {
	if isEmptyJSON(raw) {
		return ""
	}
	return truncate(indent(raw), maxArgsBytes)
}

// FormatResult renders a tool response as text, capped at 64 KiB.
func FormatResult(raw json.RawMessage) (text string, isError bool) {
	if isEmptyJSON(raw) {
		return "", false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		if c, ok := obj["content"]; ok {
			var items []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(c, &items) == nil {
				var texts []string
				for _, it := range items {
					if it.Type == "text" {
						texts = append(texts, it.Text)
					}
				}
				if e, ok := obj["isError"]; ok {
					isError = bytes.Equal(bytes.TrimSpace(e), []byte("true"))
				}
				return truncate(strings.Join(texts, "\n"), maxResultBytes), isError
			}
		}
		if r, ok := obj["result"]; ok {
			var s string
			if json.Unmarshal(r, &s) == nil {
				return truncate(s, maxResultBytes), false
			}
			return truncate(indent(r), maxResultBytes), false
		}
	}
	return truncate(indent(raw), maxResultBytes), false
}

// ArgsSummary joins up to 3 top-level string/number arg values, in document
// order, for the collapsed tool line. At most 60 runes.
func ArgsSummary(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	var vals []string
	for dec.More() && len(vals) < maxSummaryVals {
		if _, err := dec.Token(); err != nil { // key
			break
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		switch x := v.(type) {
		case string:
			vals = append(vals, x)
		case float64:
			b, _ := json.Marshal(x)
			vals = append(vals, string(b))
		}
	}
	s := strings.Join(vals, ", ")
	if r := []rune(s); len(r) > maxSummaryRunes {
		s = string(r[:maxSummaryRunes-1]) + "…"
	}
	return s
}
