package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
)

const (
	noticeUnfinished = "This reply didn't finish."
	noticeApproval   = "The agent asked for approval, which the portal doesn't support yet."
	noticeUnanswered = "This question wasn't answered."
)

// Item is one step of an agent reply: "text", "tool", "notice" or "ask" (an
// ask_user question set the user answered, with Answers in question order, or
// Skipped).
type Item struct {
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	Summary string `json:"summary,omitempty"`
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`

	Questions []a2a.Question `json:"questions,omitempty"`
	Answers   [][]string     `json:"answers,omitempty"`
	Skipped   bool           `json:"skipped,omitempty"`
	Subagent  string         `json:"subagent,omitempty"`
}

// Pending is the ask_user question the conversation's last task is paused
// on. The browser answers it by resuming TaskID.
type Pending struct {
	TaskID string `json:"task_id"`
	a2a.Ask
}

// Turn is one user message and the agent's reply.
type Turn struct {
	User  string `json:"user"`
	Items []Item `json:"items"`
	State string `json:"state"`
}

// Transcript is a whole conversation. Running is true while the last turn may
// still be in progress; Pending is set while it waits for an answer.
type Transcript struct {
	Turns   []Turn   `json:"turns"`
	Running bool     `json:"running"`
	Pending *Pending `json:"pending,omitempty"`
}

type part struct {
	Kind     string                     `json:"kind"`
	Text     string                     `json:"text"`
	Data     json.RawMessage            `json:"data"`
	Metadata map[string]json.RawMessage `json:"metadata"`
}

type message struct {
	Role      string `json:"role"`
	MessageID string `json:"messageId"`
	Parts     []part `json:"parts"`
}

type task struct {
	ID     string `json:"id"`
	Status struct {
		State     string   `json:"state"`
		Timestamp string   `json:"timestamp"`
		Message   *message `json:"message"`
	} `json:"status"`
	History   []message `json:"history"`
	Artifacts []struct {
		Parts []part `json:"parts"`
	} `json:"artifacts"`
}

func isThought(meta map[string]json.RawMessage) bool {
	for k, v := range meta {
		if strings.HasSuffix(k, "_thought") && bytes.Equal(bytes.TrimSpace(v), []byte("true")) {
			return true
		}
	}
	return false
}

func unfinished(state string) bool { return state == "submitted" || state == "working" }

// Normalise turns kagent's tasks array (the unwrapped data of
// api/sessions/{id}/tasks) into a transcript, one turn per task.
func Normalise(tasksJSON []byte, now time.Time, maxDuration time.Duration) (Transcript, error) {
	var tasks []task
	if err := json.Unmarshal(tasksJSON, &tasks); err != nil {
		return Transcript{}, fmt.Errorf("history: decode tasks: %w", err)
	}
	tr := Transcript{Turns: make([]Turn, 0, len(tasks))}
	for i, t := range tasks {
		tu := normaliseTask(t)
		last := i == len(tasks)-1
		switch {
		case unfinished(t.Status.State) && last && recent(t.Status.Timestamp, now, maxDuration):
			tr.Running = true
		case unfinished(t.Status.State):
			tu.Items = append(tu.Items, Item{Kind: "notice", Text: noticeUnfinished})
		case t.Status.State == "input-required":
			switch ask := pendingAsk(t); {
			case ask != nil && last:
				tr.Pending = &Pending{TaskID: t.ID, Ask: *ask}
			case ask != nil:
				tu.Items = append(tu.Items, Item{Kind: "notice", Text: noticeUnanswered})
			default:
				tu.Items = append(tu.Items, Item{Kind: "notice", Text: noticeApproval})
			}
		}
		tr.Turns = append(tr.Turns, tu)
	}
	return tr, nil
}

// pendingAsk returns the ask_user question in an input-required task's status
// message, or nil if it is paused on something else (a tool approval).
func pendingAsk(t task) *a2a.Ask {
	if t.Status.Message == nil {
		return nil
	}
	for _, p := range t.Status.Message.Parts {
		if p.Kind == "data" {
			if a := a2a.ParseAsk(p.Metadata, p.Data); a != nil {
				return a
			}
		}
	}
	return nil
}

// decisionAfter returns the first user decision after history[i], as
// kagent's UI pairs them, or nil if there is none yet.
func decisionAfter(history []message, i int) *a2a.Decision {
	for _, m := range history[i+1:] {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == "data" {
				if d := a2a.ParseDecision(p.Data); d != nil {
					return d
				}
			}
		}
	}
	return nil
}

func recent(ts string, now time.Time, maxDuration time.Duration) bool {
	t, err := time.Parse(time.RFC3339Nano, ts)
	return err == nil && now.Sub(t) < maxDuration
}

func normaliseTask(t task) Turn {
	tu := Turn{Items: []Item{}, State: t.Status.State}
	seen := map[string]bool{}
	asked := map[string]bool{}
	hasText := false
	for i, m := range t.History {
		if m.MessageID != "" {
			if seen[m.MessageID] {
				continue
			}
			seen[m.MessageID] = true
		}
		if m.Role == "user" {
			for _, p := range m.Parts {
				if tu.User == "" && p.Kind == "text" {
					tu.User = p.Text
				}
			}
			continue
		}
		for _, p := range m.Parts {
			if isThought(p.Metadata) {
				continue
			}
			switch p.Kind {
			case "text":
				if p.Text != "" {
					tu.Items = append(tu.Items, Item{Kind: "text", Text: p.Text})
					hasText = true
				}
			case "data":
				// An unanswered question is shown from the status message (see Normalise).
				if a := a2a.ParseAsk(p.Metadata, p.Data); a != nil && !asked[a.ID] {
					if d := decisionAfter(t.History, i); d != nil {
						asked[a.ID] = true
						tu.Items = append(tu.Items, Item{Kind: "ask", Questions: a.Questions, Answers: d.Answers,
							Skipped: d.Type == "reject", Subagent: a.Subagent})
					}
				}
				call, res := a2a.ParseDataPart(p.Metadata, p.Data)
				if call != nil {
					tu.Items = append(tu.Items, Item{Kind: "tool", ID: call.ID, Name: call.Name,
						Args: a2a.FormatArgs(call.Args), Summary: a2a.ArgsSummary(call.Args)})
				}
				if res != nil {
					addResult(&tu, res)
				}
			}
		}
	}
	if !hasText {
		var b strings.Builder
		for _, a := range t.Artifacts {
			for _, p := range a.Parts {
				if p.Kind == "text" {
					b.WriteString(p.Text)
				}
			}
		}
		if b.Len() > 0 {
			tu.Items = append(tu.Items, Item{Kind: "text", Text: b.String()})
		}
	}
	return tu
}

// addResult fills the tool item with the result's ID, or appends a tool item
// when there is no matching call.
func addResult(tu *Turn, res *a2a.ToolResult) {
	text, isErr := a2a.FormatResult(res.Response)
	for i := len(tu.Items) - 1; i >= 0; i-- {
		it := &tu.Items[i]
		if it.Kind == "tool" && it.ID == res.ID && res.ID != "" {
			it.Result, it.IsError = text, isErr
			return
		}
	}
	tu.Items = append(tu.Items, Item{Kind: "tool", ID: res.ID, Name: res.Name, Result: text, IsError: isErr})
}
