package a2a

import (
	"bytes"
	"encoding/json"
)

// kagent's human-in-the-loop (HITL) flow. The ask_user tool pauses the task:
// the final status update is input-required and carries a long-running
// adk_request_confirmation function call whose originalFunctionCall is the
// ask_user call. The client resumes the same task with a message whose data
// part is {"decision_type":"approve","ask_user_answers":[{"answer":[...]}]},
// one entry per question, in question order. kagent's own UI does the same
// (ui/src/components/chat/ChatInterface.tsx, handleAskUserSubmit).

const (
	confirmationCall = "adk_request_confirmation"
	credentialCall   = "adk_request_credential"
	askUserTool      = "ask_user"
	resumeText       = "Answered questions"
	skipText         = "Skipped questions"
	skipReason       = "The user skipped the questions without answering."
	maxTaskIDLen     = 128
)

// Question is one ask_user question. Choices may be empty (free text only).
type Question struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
}

// Ask is a pending ask_user request. ID is the confirmation call's ID;
// Subagent names the subagent that asked, if any.
type Ask struct {
	ID        string     `json:"id"`
	Questions []Question `json:"questions"`
	Subagent  string     `json:"subagent,omitempty"`
}

// metaValue reads adk_<key>, then kagent_<key>, as kagent's UI does.
func metaValue(m map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if v, ok := m["adk_"+key]; ok {
		return v, true
	}
	v, ok := m["kagent_"+key]
	return v, ok
}

type origCall struct {
	Name string `json:"name"`
	Args struct {
		Questions []Question `json:"questions"`
	} `json:"args"`
}

// ParseAsk returns the ask_user request in a data part, or nil if the part is
// anything else (including a plain tool approval, which the portal doesn't
// support).
func ParseAsk(meta map[string]json.RawMessage, data json.RawMessage) *Ask {
	if metaType(meta) != "function_call" {
		return nil
	}
	if v, ok := metaValue(meta, "is_long_running"); !ok || !bytes.Equal(bytes.TrimSpace(v), []byte("true")) {
		return nil
	}
	var d struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Args struct {
			Orig origCall `json:"originalFunctionCall"`
			Conf struct {
				Payload struct {
					Subagent string `json:"subagent_name"`
					Parts    []struct {
						Orig origCall `json:"originalFunctionCall"`
					} `json:"hitl_parts"`
				} `json:"payload"`
			} `json:"toolConfirmation"`
		} `json:"args"`
	}
	if json.Unmarshal(data, &d) != nil || d.Name != confirmationCall {
		return nil
	}
	var qs []Question
	var sub string
	switch p := d.Args.Conf.Payload; {
	case d.Args.Orig.Name == askUserTool:
		qs = d.Args.Orig.Args.Questions
	case len(p.Parts) == 1 && p.Parts[0].Orig.Name == askUserTool:
		qs, sub = p.Parts[0].Orig.Args.Questions, p.Subagent
	default:
		return nil
	}
	if len(qs) == 0 {
		return nil
	}
	return &Ask{ID: d.ID, Questions: qs, Subagent: sub}
}

// hidden reports whether a tool call or result is HITL plumbing that kagent's
// UI doesn't show: the confirmation and credential calls (and their
// responses), and the stub results a paused tool returns.
func hidden(name string, response json.RawMessage) bool {
	if name == confirmationCall || name == credentialCall {
		return true
	}
	var r struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(response, &r) == nil {
		return r.Status == "pending" || r.Status == "confirmation_requested"
	}
	return false
}

// Decision is a user's HITL decision: Type is "approve", "reject" or
// "batch"; Answers holds the ask_user answers, if any.
type Decision struct {
	Type    string
	Answers [][]string
}

// ParseDecision reads a decision data part (one with a decision_type), or
// returns nil for any other part.
func ParseDecision(data json.RawMessage) *Decision {
	var d struct {
		Type    *string `json:"decision_type"`
		Answers []struct {
			Answer []string `json:"answer"`
		} `json:"ask_user_answers"`
	}
	if json.Unmarshal(data, &d) != nil || d.Type == nil {
		return nil
	}
	out := &Decision{Type: *d.Type}
	if d.Answers != nil {
		out.Answers = make([][]string, len(d.Answers))
		for i, a := range d.Answers {
			out.Answers[i] = append([]string{}, a.Answer...)
		}
	}
	return out
}

// ResumeRequest is the JSON-RPC message/stream body that answers the ask_user
// request paused in taskID: one answer list per question, in question order.
func ResumeRequest(contextID, taskID string, answers [][]string) any {
	list := make([]map[string]any, len(answers))
	for i, a := range answers {
		if a == nil {
			a = []string{}
		}
		list[i] = map[string]any{"answer": a}
	}
	return decisionRequest(contextID, taskID, resumeText, map[string]any{"decision_type": "approve", "ask_user_answers": list})
}

// SkipRequest is the message/stream body that declines the ask_user request
// paused in taskID. kagent resumes the task and the tool tells the model the
// user cancelled (Python ADK: {"status":"cancelled"}).
func SkipRequest(contextID, taskID string) any {
	return decisionRequest(contextID, taskID, skipText, map[string]any{"decision_type": "reject", "rejection_reason": skipReason})
}

// decisionRequest is a message/stream body resuming taskID with a decision
// data part and a display-only text part, as kagent's UI sends.
func decisionRequest(contextID, taskID, text string, decision map[string]any) any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      NewUUID(),
		"method":  "message/stream",
		"params": map[string]any{
			"message": map[string]any{
				"kind":      "message",
				"messageId": NewUUID(),
				"contextId": contextID,
				"taskId":    taskID,
				"role":      "user",
				"parts": []map[string]any{
					// No kagent_type metadata: kagent reads this as a decision, not a tool part.
					{"kind": "data", "metadata": map[string]any{}, "data": decision},
					{"kind": "text", "text": text},
				},
			},
		},
	}
}

// ValidTaskID reports whether s looks like a kagent task ID: 1-128 ASCII
// letters, digits, '-', '_', '.' or ':'.
func ValidTaskID(s string) bool {
	if s == "" || len(s) > maxTaskIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}
