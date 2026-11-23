// Package history reads past kagent conversations for one agent through
// Rancher, as the user, and turns them into transcripts. It sends kagent no
// user_id, so every user with access to the agent sees the same history.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
)

var (
	ErrNotFound  = errors.New("history: not found")
	ErrExpired   = errors.New("history: expired")
	ErrForbidden = errors.New("history: forbidden")
)

const (
	maxConversations = 50
	maxListBody      = 4 << 20
	maxTasksBody     = 16 << 20
)

// Conversation is one kagent session. Updated is zero if kagent's timestamp
// could not be parsed.
type Conversation struct {
	ID, Title string
	Updated   time.Time
}

// Service reads conversations from kagent on each cluster.
type Service struct {
	clients     map[string]*rancher.Client
	kagent      config.KagentConfig
	maxDuration time.Duration
	now         func() time.Time
}

// New builds a Service. maxDuration is the chat reply cap, used to tell a
// running turn from an abandoned one.
func New(clients []*rancher.Client, k config.KagentConfig, maxDuration time.Duration) *Service {
	m := make(map[string]*rancher.Client, len(clients))
	for _, c := range clients {
		m[c.Name] = c
	}
	return &Service{clients: m, kagent: k, maxDuration: maxDuration, now: time.Now}
}

// wrapped is kagent's REST envelope.
type wrapped struct {
	Error   bool            `json:"error"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func (s *Service) client(sites []auth.SiteResult, row clusters.Row) (*rancher.Client, string, error) {
	c := s.clients[row.Site]
	for _, sr := range sites {
		if sr.Site == row.Site && sr.Status == auth.StatusOK && c != nil {
			return c, sr.Token.Bearer, nil
		}
	}
	return nil, "", ErrExpired
}

func (s *Service) base(row clusters.Row) string {
	e := url.PathEscape
	return "/k8s/clusters/" + e(row.ID) + "/api/v1/namespaces/" + e(s.kagent.Namespace) +
		"/services/" + e(s.kagent.Service) + "/proxy/api/sessions/"
}

// mapErr turns Rancher status codes into the package errors.
func mapErr(err error, notFound bool) error {
	var se *rancher.StatusError
	if errors.As(err, &se) {
		switch {
		case se.Code == 401:
			return ErrExpired
		case se.Code == 403:
			return ErrForbidden
		case se.Code == 404 && notFound:
			return ErrNotFound
		}
	}
	return fmt.Errorf("history: %w", err)
}

func (s *Service) get(ctx context.Context, c *rancher.Client, bearer, path string, limit int64) (json.RawMessage, error) {
	var w wrapped
	if err := c.GetJSON(ctx, bearer, path, limit, &w); err != nil {
		return nil, err
	}
	if w.Error {
		return nil, fmt.Errorf("history: kagent error: %s", w.Message)
	}
	return w.Data, nil
}

// List returns up to 50 conversations for the configured agent, newest first.
// Sessions whose id isn't a UUID are skipped: the portal can't open them.
// An agent kagent doesn't know (404) has no conversations.
func (s *Service) List(ctx context.Context, sites []auth.SiteResult, row clusters.Row) ([]Conversation, error) {
	c, bearer, err := s.client(sites, row)
	if err != nil {
		return nil, err
	}
	e := url.PathEscape
	path := s.base(row) + "agent/" + e(s.kagent.AgentNamespace) + "/" + e(s.kagent.AgentName)
	data, err := s.get(ctx, c, bearer, path, maxListBody)
	if err != nil {
		var se *rancher.StatusError
		if errors.As(err, &se) && se.Code == 404 {
			return []Conversation{}, nil
		}
		return nil, mapErr(err, false)
	}
	var raw []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		UpdatedAt string `json:"updated_at"`
	}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("history: decode sessions: %w", err)
		}
	}
	out := make([]Conversation, 0, min(len(raw), maxConversations))
	for _, r := range raw {
		if !a2a.ValidUUID(r.ID) {
			continue // the portal can only open UUID context ids
		}
		if len(out) == maxConversations {
			break
		}
		cv := Conversation{ID: r.ID, Title: r.Name}
		if t, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err == nil {
			cv.Updated = t
		}
		if cv.Title == "" {
			cv.Title = "Conversation"
			if !cv.Updated.IsZero() {
				cv.Title = "Conversation · " + cv.Updated.Format("2 Jan 2006")
			}
		}
		out = append(out, cv)
	}
	return out, nil
}

// Transcript returns conversation id, which must be one List returns.
func (s *Service) Transcript(ctx context.Context, sites []auth.SiteResult, row clusters.Row, id string) (Transcript, error) {
	list, err := s.List(ctx, sites, row)
	if err != nil {
		return Transcript{}, err
	}
	found := false
	for _, cv := range list {
		found = found || cv.ID == id
	}
	if !found || id == "" {
		return Transcript{}, ErrNotFound
	}
	c, bearer, err := s.client(sites, row)
	if err != nil {
		return Transcript{}, err
	}
	data, err := s.get(ctx, c, bearer, s.base(row)+url.PathEscape(id)+"/tasks", maxTasksBody)
	if err != nil {
		return Transcript{}, mapErr(err, true)
	}
	return Normalise(data, s.now(), s.maxDuration)
}
