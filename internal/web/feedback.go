package web

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
)

const (
	// maxFeedbackForm leaves room for a 2000-character comment of 3-byte
	// runes after URL encoding (about 18 KiB).
	maxFeedbackForm    = 32 << 10
	maxFeedbackComment = 2000 // characters (runes), after trimming
	feedbackPerHour    = 10   // submissions per user per hour
	maxFeedbackFrom    = 512  // bytes

	fbMsgEmpty   = "Pick a rating or write a comment."
	fbMsgRating  = "Pick a rating from 1 to 5."
	fbMsgTooLong = "Your comment is too long (2000 characters max)."
	fbMsgLimited = "You've sent a lot of feedback recently. Please try again later."
)

// feedbackForm is the feedback page's state: the submitted values to
// redisplay, a validation message, or Sent for the thanks card.
type feedbackForm struct {
	Rating                              int
	Comment, Source, From, Conversation string
	Error                               string
	Sent                                bool
}

// cleanSource maps anything but the known link sources to "footer".
func cleanSource(s string) string {
	switch s {
	case "footer", "menu", "chat":
		return s
	}
	return "footer"
}

// cleanFrom keeps from only if it is a local path (never another origin).
// It is only logged and echoed back in a hidden field, never redirected to.
func cleanFrom(raw string) string {
	if len(raw) > maxFeedbackFrom || !strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `/\`) {
		return ""
	}
	return raw
}

// clusterOf returns the site and cluster ID when from is a cluster page
// (/clusters/{site}/{id}), else empty strings.
func clusterOf(from string) (site, cluster string) {
	u, err := url.Parse(from)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "clusters" || parts[1] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[1], parts[2]
}

// conversationOf returns from's ?c= conversation ID when it is a UUID.
func conversationOf(from string) string {
	u, err := url.Parse(from)
	if err != nil {
		return ""
	}
	if c := u.Query().Get("c"); a2a.ValidUUID(c) {
		return c
	}
	return ""
}

func (s *server) feedbackPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	q := r.URL.Query()
	s.render(w, r, http.StatusOK, "feedback", pageData{Session: sess,
		Feedback: feedbackForm{Source: cleanSource(q.Get("source")), From: cleanFrom(q.Get("from"))}})
}

// feedbackSubmit takes a submission from the feedback page (CSRF in the
// form, answers with a page) or from the chat strip (CSRF in X-CSRF-Token,
// answers with a bare status code), and logs it as one feedback event.
func (s *server) feedbackSubmit(w http.ResponseWriter, r *http.Request) {
	api := r.Header.Get("X-CSRF-Token") != ""
	sess, ok := s.current(r)
	if !ok {
		if api {
			http.Error(w, "not logged in", http.StatusUnauthorized)
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return
	}
	s.setBodyDeadline(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackForm)
	perr := r.ParseForm()
	// A body over the cap leaves PostForm without the csrf field, so report
	// it as too large before the CSRF check; the request did nothing either way.
	var tooBig *http.MaxBytesError
	if errors.As(perr, &tooBig) {
		http.Error(w, "feedback too large", http.StatusRequestEntityTooLarge)
		return
	}
	token := r.Header.Get("X-CSRF-Token")
	if !api {
		token = r.PostForm.Get("csrf")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if perr != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	fb := feedbackForm{
		// Browsers submit textarea newlines as CRLF but count them as one
		// character for maxlength; normalise so the cap matches the browser's.
		Comment:      strings.TrimSpace(strings.ReplaceAll(f.Get("comment"), "\r\n", "\n")),
		Source:       cleanSource(f.Get("source")),
		From:         cleanFrom(f.Get("from")),
		Conversation: f.Get("conversation"),
	}
	if !a2a.ValidUUID(fb.Conversation) {
		fb.Conversation = conversationOf(fb.From)
	}
	fail := func(code int, msg string) {
		if api {
			http.Error(w, msg, code)
			return
		}
		fb.Error = msg
		s.render(w, r, code, "feedback", pageData{Session: sess, Feedback: fb})
	}
	if v := f.Get("rating"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5 {
			fail(http.StatusBadRequest, fbMsgRating)
			return
		}
		fb.Rating = n
	}
	switch {
	case fb.Rating == 0 && fb.Comment == "":
		fail(http.StatusBadRequest, fbMsgEmpty)
		return
	case utf8.RuneCountInString(fb.Comment) > maxFeedbackComment:
		fail(http.StatusBadRequest, fbMsgTooLong)
		return
	}
	if !s.fbLim.Reserve(sess.Key()) {
		fail(http.StatusTooManyRequests, fbMsgLimited)
		return
	}
	site, cluster := clusterOf(fb.From)
	s.log.Info("feedback received", "event", "feedback",
		"feedback_id", a2a.NewUUID(),
		"username", sess.Username,
		"principal", sess.Principal,
		"rating", fb.Rating,
		"comment", fb.Comment,
		"source", fb.Source,
		"page", fb.From,
		"site", site,
		"cluster", cluster,
		"conversation", fb.Conversation,
		"version", s.version)
	if api {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.render(w, r, http.StatusOK, "feedback", pageData{Session: sess, Feedback: feedbackForm{Sent: true, From: fb.From}})
}

// ratingLabels label the 1–5 scale on the feedback page.
var ratingLabels = []string{"1 – Poor", "2", "3", "4", "5 – Excellent"}
