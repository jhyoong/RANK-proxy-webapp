package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/chat"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/history"
)

// Chat is what the web layer needs from chat.Service. Send emits done last
// and returns once the reply is over.
type Chat interface {
	Send(ctx context.Context, sites []auth.SiteResult, row clusters.Row, principal, text, contextID string, emit func(chat.Out))
}

type chatRequest struct {
	Text      string `json:"text"`
	ContextID string `json:"context_id"`
}

// chat streams one kagent reply as server-sent events. Every check answers
// with a plain status code before Send is called.
func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Error(w, "not logged in", http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.CSRF)) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	limit := s.cfg.Chat.MaxMessageBytes
	s.setBodyDeadline(w)
	r.Body = http.MaxBytesReader(w, r.Body, int64(limit)+1024)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "message too long", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		return
	}
	// Defensive: net/http already clears the deadline when the body hits EOF;
	// this guards future changes so the (long) SSE reply is never cut.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if len(req.Text) > limit {
		http.Error(w, "message too long", http.StatusRequestEntityTooLarge)
		return
	}
	if strings.TrimSpace(req.Text) == "" || !a2a.ValidUUID(req.ContextID) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	row, found := s.listing(r, sess).Find(r.PathValue("site"), r.PathValue("id"))
	if !found {
		http.Error(w, "cluster not found", http.StatusNotFound)
		return
	}
	if !row.Connected {
		http.Error(w, "cluster disconnected", http.StatusConflict)
		return
	}
	if !s.claim(sess.ID) {
		http.Error(w, "a reply is already running", http.StatusTooManyRequests)
		return
	}
	defer s.unclaim(sess.ID)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	expired := false
	s.ch.Send(r.Context(), sess.Sites, row, sess.Principal, req.Text, req.ContextID, func(o chat.Out) {
		if e, ok := o.Data.(chat.Error); ok && o.Type == "error" && e.Code == "expired" {
			expired = true
		}
		// A write error means the browser went away; Send sees the
		// cancelled context and finishes.
		if writeEvent(w, o) == nil {
			_ = rc.Flush()
		}
	})
	// The headers are sent, so the cookie stays; the browser's next request
	// finds no session.
	if expired {
		s.expire(sess)
	}
}

// transcript returns one past conversation as JSON. The cluster page has
// already run the kagent check, so this doesn't.
func (s *server) transcript(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not_logged_in"})
		return
	}
	row, found := s.listing(r, sess).Find(r.PathValue("site"), r.PathValue("id"))
	cid := r.PathValue("cid")
	if !found || !a2a.ValidUUID(cid) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	tr, err := s.hist.Transcript(r.Context(), sess.Sites, row, cid)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, tr)
	case errors.Is(err, history.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	case errors.Is(err, history.ErrExpired):
		s.expire(sess)
		s.setCookie(w, "", -1)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "expired"})
	case errors.Is(err, history.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	default:
		s.log.Warn("transcript failed", "event", "history_failed", "site", row.Site, "cluster", row.ID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed"})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// claim marks a session as having a running reply. It reports false if one
// is already running.
func (s *server) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[id] {
		return false
	}
	s.active[id] = true
	return true
}

func (s *server) unclaim(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, id)
}

// writeEvent writes o as one SSE event. json.Marshal escapes newlines and
// HTML characters, so agent text can't break the framing. A ping is a comment.
func writeEvent(w io.Writer, o chat.Out) error {
	if o.Type == "ping" {
		_, err := io.WriteString(w, ": ping\n\n")
		return err
	}
	b, err := json.Marshal(o.Data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", o.Type, b)
	return err
}
