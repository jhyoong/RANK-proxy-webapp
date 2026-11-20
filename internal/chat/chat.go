// Package chat relays one A2A reply from kagent, through Rancher, as the
// logged-in user, with total and silence caps and an audit log line.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
)

// Out is one event for the browser. Type is "progress", "tool", "answer",
// "state", "error", "ping" or "done"; Data is nil for ping and otherwise the
// matching struct below.
type Out struct {
	Type string
	Data any
}

// Progress is agent text from a non-final step. A partial chunk is replaced
// by the complete text that follows it.
type Progress struct {
	Text    string `json:"text"`
	Partial bool   `json:"partial"`
}

// Tool names a tool the agent called.
type Tool struct {
	Name string `json:"name"`
}

// Answer is agent text from a final event or an artifact.
type Answer struct {
	Text string `json:"text"`
}

// State is a task state change.
type State struct {
	State string `json:"state"`
}

// Error is user-facing copy for a failed reply.
type Error struct {
	Message string `json:"message"`
}

// Done is always the last Out of a reply.
type Done struct {
	State        string `json:"state"`
	TaskID       string `json:"task_id"`
	FirstEventMs int64  `json:"first_event_ms"`
	TotalMs      int64  `json:"total_ms"`
	Events       int    `json:"events"`
}

const maxAgentErr = 300

// Service relays chat messages to kagent with each user's own token.
type Service struct {
	clients map[string]*rancher.Client
	kagent  config.KagentConfig
	cfg     config.ChatConfig
	log     *slog.Logger
	now     func() time.Time
}

// New builds a Service. cfg is used as given (config.Load applies defaults).
func New(clients []*rancher.Client, k config.KagentConfig, c config.ChatConfig, log *slog.Logger) *Service {
	m := make(map[string]*rancher.Client, len(clients))
	for _, cl := range clients {
		m[cl.Name] = cl
	}
	return &Service{clients: m, kagent: k, cfg: c, log: log, now: time.Now}
}

func expiredMsg(site string) string {
	return fmt.Sprintf("Your session on %s has expired: log in again", site)
}

// reply is the state of one Send.
type reply struct {
	start      time.Time
	firstEvent time.Duration
	events     int
	state      string
	taskID     string
	httpCode   int
	result     string
	body       string // rancher_body for the log
	errMsg     string
}

func (r *reply) fail(result, msg string) { r.result, r.errMsg = result, msg }

type next struct {
	ev  a2a.Event
	err error
}

// Send relays text to the agent on row's cluster and emits the reply. It
// always emits exactly one done, last, and returns only after the upstream
// body is closed and its reader goroutine has exited.
func (s *Service) Send(ctx context.Context, sites []auth.SiteResult, row clusters.Row, principal, text, contextID string, emit func(Out)) {
	r := &reply{start: s.now()}
	defer func() {
		if r.errMsg != "" {
			emit(Out{Type: "error", Data: Error{Message: r.errMsg}})
		}
		total := s.now().Sub(r.start)
		emit(Out{Type: "done", Data: Done{
			State: r.state, TaskID: r.taskID, FirstEventMs: r.firstEvent.Milliseconds(),
			TotalMs: total.Milliseconds(), Events: r.events,
		}})
		args := []any{"event", "chat", "site", row.Site, "cluster", row.ID, "principal", principal,
			"context_id", contextID, "task_id", r.taskID, "http_code", r.httpCode,
			"first_event_ms", r.firstEvent.Milliseconds(), "total_ms", total.Milliseconds(),
			"events", r.events, "result", r.result}
		if r.body != "" {
			args = append(args, "rancher_body", r.body)
		}
		s.log.Info("chat", args...)
	}()

	var bearer string
	for i := range sites {
		if sites[i].Site == row.Site && sites[i].Status == auth.StatusOK {
			bearer = sites[i].Token.Bearer
		}
	}
	c := s.clients[row.Site]
	if bearer == "" || c == nil {
		r.fail("expired", expiredMsg(row.Site))
		return
	}

	rctx, cancel := context.WithTimeout(ctx, s.cfg.MaxDuration)
	defer cancel()
	body, err := c.PostStream(rctx, bearer, a2a.Path(row.ID, s.kagent), a2a.StreamRequest(text, contextID), s.cfg.IdleTimeout)
	if err != nil {
		s.preStreamError(ctx, rctx, row.Site, err, r)
		return
	}
	r.httpCode = 200

	ch := make(chan next)
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		rd := a2a.NewReader(body)
		for {
			ev, err := rd.Next()
			select {
			case ch <- next{ev, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		// Cancelling the request ends a blocked read; then wait for the
		// reader before closing the body it reads from.
		cancel()
		close(stop)
		<-readerDone
		body.Close()
	}()

	s.relay(ctx, rctx, contextID, ch, r, emit)
}

// preStreamError classifies a PostStream failure.
func (s *Service) preStreamError(parent, rctx context.Context, site string, err error, r *reply) {
	var se *rancher.StatusError
	switch {
	case errors.As(err, &se):
		r.httpCode = se.Code
		if se.Code != 401 && se.Code != 403 && se.Code != 404 {
			r.body = se.Body
		}
		switch se.Code {
		case 401:
			r.fail("expired", expiredMsg(site))
		case 403:
			r.fail("no-access", "You don't have kagent access on this cluster")
		case 404:
			r.fail("not-installed", "kagent or the agent isn't installed on this cluster")
		case 503:
			r.fail("not-running", "kagent is installed but not running")
		case 502:
			r.fail("unreachable", "Cluster unreachable through Rancher")
		default:
			r.fail("unexpected", fmt.Sprintf("Unexpected response (HTTP %d)", se.Code))
		}
	case parent.Err() != nil:
		r.result = "cancelled"
	case rctx.Err() != nil:
		r.fail("timeout", s.totalMsg())
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// Neither context ended, so PostStream's header timer (IdleTimeout) fired.
		r.fail("idle-timeout", s.idleMsg())
	default:
		r.fail("unreachable", "Cluster unreachable through Rancher")
	}
}

func (s *Service) totalMsg() string {
	return fmt.Sprintf("Reply took longer than %s and was stopped", s.cfg.MaxDuration)
}

func (s *Service) idleMsg() string {
	return fmt.Sprintf("No response from the agent for %s; stopped", s.cfg.IdleTimeout)
}

// relay runs the main loop until the reply ends; it sets r's result.
func (s *Service) relay(parent, rctx context.Context, contextID string, ch <-chan next, r *reply, emit func(Out)) {
	idle := time.NewTimer(s.cfg.IdleTimeout)
	defer idle.Stop()
	beat := time.NewTicker(s.cfg.Heartbeat)
	defer beat.Stop()
	sawEvent := false
	stopped := func() {
		if parent.Err() != nil {
			r.result = "cancelled"
		} else {
			r.fail("timeout", s.totalMsg())
		}
	}

	for {
		select {
		case <-rctx.Done():
			stopped()
			return
		case <-idle.C:
			r.fail("idle-timeout", s.idleMsg())
			return
		case <-beat.C:
			if !sawEvent {
				emit(Out{Type: "ping"})
			}
			sawEvent = false
		case n := <-ch:
			if n.err != nil {
				switch {
				case rctx.Err() != nil:
					stopped()
				case errors.Is(n.err, a2a.ErrBadEvent) || errors.Is(n.err, a2a.ErrEventTooLarge):
					r.fail("unexpected", "Unexpected event from the agent")
				default: // io.EOF or a broken connection before any final event
					r.fail("cut-off", "The connection to the agent was cut before the reply finished")
				}
				return
			}
			sawEvent = true
			idle.Reset(s.cfg.IdleTimeout)
			if s.event(contextID, n.ev, r, emit) {
				return
			}
		}
	}
}

// event maps one upstream event to Outs. It reports whether the reply ended.
func (s *Service) event(contextID string, ev a2a.Event, r *reply, emit func(Out)) bool {
	since := s.now().Sub(r.start)
	r.events++
	if r.events == 1 {
		r.firstEvent = since
	}
	if s.cfg.LogEvents {
		s.log.Info("chat event", "event", "chat_event", "context_id", contextID, "n", r.events,
			"ms", since.Milliseconds(), "shape", ev.Shape)
	}
	if ev.TaskID != "" {
		r.taskID = ev.TaskID
	}

	switch ev.Kind {
	case "":
		return false // unknown kind: ignored
	case "error":
		msg := []rune(ev.ErrMessage)
		if len(msg) > maxAgentErr {
			msg = msg[:maxAgentErr]
		}
		r.fail("error", "Agent error: "+string(msg))
		return true
	}

	if ev.Role != "user" { // the submitted echo of the user's own message
		answer := ev.Final || ev.Kind == "message" || ev.Kind == "artifact-update"
		for _, t := range ev.Texts {
			if answer {
				emit(Out{Type: "answer", Data: Answer{Text: t.Text}})
			} else {
				emit(Out{Type: "progress", Data: Progress{Text: t.Text, Partial: t.Partial}})
			}
		}
	}
	// Tools follow the texts: the browser closes the open partial line on a
	// tool, so a complete step's text must land first to replace it.
	for _, name := range ev.Tools {
		emit(Out{Type: "tool", Data: Tool{Name: name}})
	}
	if ev.State != "" && ev.State != r.state {
		r.state = ev.State
		emit(Out{Type: "state", Data: State{State: ev.State}})
	}

	switch {
	case ev.State == "failed" || ev.State == "rejected":
		r.fail("failed", "The agent reported the task failed")
		return true
	case ev.Final && ev.State == "input-required":
		r.fail("input-required", "The agent is asking for approval, which the portal doesn't support yet")
		return true
	case ev.Final:
		r.result = "completed"
		return true
	}
	return false
}
