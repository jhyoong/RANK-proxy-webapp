package a2a

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const maxEventBytes = 1 << 20

// Text is a piece of agent text. Partial marks a streaming chunk that a later
// complete message replaces.
type Text struct {
	Text    string
	Partial bool
}

// Event is one parsed A2A JSON-RPC event. Kind is "" for an unknown kind.
type Event struct {
	Kind              string // "status-update","artifact-update","task","message","error"; "" if unknown
	TaskID, ContextID string
	Role              string
	State             string
	Final             bool
	Texts             []Text
	Calls             []ToolCall
	Results           []ToolResult
	Asks              []Ask // ask_user requests (with state input-required)
	ErrCode           int
	ErrMessage        string
	// Shape describes the event without content: kind, state, final, part
	// kinds and metadata keys only (never values). Safe to log.
	Shape string
}

var (
	ErrEventTooLarge = errors.New("a2a: event exceeds 1 MiB")
	ErrBadEvent      = errors.New("a2a: event is not valid JSON-RPC")
)

// Reader reads SSE events from a stream.
type Reader struct {
	br *bufio.Reader
}

func NewReader(r io.Reader) *Reader { return &Reader{br: bufio.NewReaderSize(r, 32*1024)} }

// readLine returns the next line without its terminator. total is the running
// byte count for the current event; it is checked as the line is read, so an
// endless line cannot exhaust memory.
func (r *Reader) readLine(total *int) (string, error) {
	var buf []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		*total += len(chunk)
		if *total > maxEventBytes {
			return "", ErrEventTooLarge
		}
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return strings.TrimSuffix(string(buf), "\r"), nil
			}
			return "", err
		}
		s := strings.TrimSuffix(string(buf), "\n")
		return strings.TrimSuffix(s, "\r"), nil
	}
}

// Next returns the next event, or io.EOF at a clean end of stream.
func (r *Reader) Next() (Event, error) {
	for {
		var data []string
		total := 0
		for {
			line, err := r.readLine(&total)
			if err != nil {
				if err == io.EOF && len(data) > 0 {
					break // final event without trailing blank line
				}
				return Event{}, err
			}
			if line == "" {
				if len(data) == 0 {
					continue
				}
				break
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			name, val, _ := strings.Cut(line, ":")
			if name != "data" {
				continue
			}
			data = append(data, strings.TrimPrefix(val, " "))
		}
		return parseEvent([]byte(strings.Join(data, "\n")))
	}
}

type rawPart struct {
	Kind     string                     `json:"kind"`
	Text     string                     `json:"text"`
	Metadata map[string]json.RawMessage `json:"metadata"`
	Data     json.RawMessage            `json:"data"`
}

type rawMessage struct {
	Role     string                     `json:"role"`
	Metadata map[string]json.RawMessage `json:"metadata"`
	Parts    []rawPart                  `json:"parts"`
}

type rawStatus struct {
	State   string      `json:"state"`
	Message *rawMessage `json:"message"`
}

type rawResult struct {
	Kind      string                     `json:"kind"`
	ID        string                     `json:"id"`
	TaskID    string                     `json:"taskId"`
	ContextID string                     `json:"contextId"`
	Final     bool                       `json:"final"`
	Status    *rawStatus                 `json:"status"`
	Role      string                     `json:"role"`
	Parts     []rawPart                  `json:"parts"`
	Metadata  map[string]json.RawMessage `json:"metadata"`
	Artifact  *struct {
		Parts    []rawPart                  `json:"parts"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	} `json:"artifact"`
}

type rawEnvelope struct {
	Result *rawResult `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func isTrue(m map[string]json.RawMessage, key string) bool {
	v, ok := m[key]
	return ok && bytes.Equal(bytes.TrimSpace(v), []byte("true"))
}

func partial(m map[string]json.RawMessage) bool {
	return isTrue(m, "kagent_adk_partial") || isTrue(m, "adk_partial")
}

func parseEvent(b []byte) (Event, error) {
	var env rawEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Event{}, ErrBadEvent
	}
	if env.Error != nil {
		return Event{
			Kind: "error", ErrCode: env.Error.Code, ErrMessage: env.Error.Message,
			Shape: "error code=" + fmt.Sprint(env.Error.Code),
		}, nil
	}
	res := env.Result
	if res == nil {
		return Event{Shape: "unknown"}, nil
	}
	ev := Event{ContextID: res.ContextID}
	metaKeys := map[string]bool{}
	partKinds := map[string]bool{}
	addMeta := func(m map[string]json.RawMessage) {
		for k := range m {
			metaKeys[k] = true
		}
	}
	addMeta(res.Metadata)

	var parts []rawPart
	var msgMeta map[string]json.RawMessage
	switch res.Kind {
	case "status-update", "task":
		ev.Kind = res.Kind
		ev.TaskID = res.TaskID
		if res.Kind == "task" {
			ev.TaskID = res.ID
		}
		ev.Final = res.Final && res.Kind == "status-update"
		if res.Status != nil {
			ev.State = res.Status.State
			if m := res.Status.Message; m != nil {
				ev.Role = m.Role
				parts = m.Parts
				msgMeta = m.Metadata
			}
		}
	case "artifact-update":
		ev.Kind = res.Kind
		ev.TaskID = res.TaskID
		if res.Artifact != nil {
			parts = res.Artifact.Parts
			msgMeta = res.Artifact.Metadata
		}
	case "message":
		ev.Kind = res.Kind
		ev.Final = true
		ev.Role = res.Role
		parts = res.Parts
		msgMeta = res.Metadata
	default:
		return Event{Shape: "unknown"}, nil
	}
	addMeta(msgMeta)
	msgPartial := partial(msgMeta)
	for _, p := range parts {
		addMeta(p.Metadata)
		partKinds[p.Kind] = true
		switch p.Kind {
		case "text":
			ev.Texts = append(ev.Texts, Text{Text: p.Text, Partial: msgPartial || partial(p.Metadata)})
		case "data":
			if a := ParseAsk(p.Metadata, p.Data); a != nil {
				ev.Asks = append(ev.Asks, *a)
			}
			call, res := ParseDataPart(p.Metadata, p.Data)
			if call != nil {
				ev.Calls = append(ev.Calls, *call)
			}
			if res != nil {
				ev.Results = append(ev.Results, *res)
			}
		}
	}

	sorted := func(m map[string]bool) string {
		s := make([]string, 0, len(m))
		for k := range m {
			s = append(s, k)
		}
		sort.Strings(s)
		return strings.Join(s, ",")
	}
	shape := ev.Kind
	if ev.State != "" {
		shape += " state=" + ev.State
	}
	shape += fmt.Sprintf(" final=%t", ev.Final)
	if len(partKinds) > 0 {
		shape += " parts=" + sorted(partKinds)
	}
	if len(metaKeys) > 0 {
		shape += " meta=" + sorted(metaKeys)
	}
	ev.Shape = shape
	return ev, nil
}
