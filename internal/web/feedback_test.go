package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/a2a"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
)

const fbConv = "9b1e2c3d-4e5f-4a6b-8c7d-0e1f2a3b4c5d"

// fbHarness is a logged-in harness whose log is captured as JSON.
type fbHarness struct {
	harness
	cookie *http.Cookie
	csrf   string
	buf    *syncBuf
}

func newFB(t *testing.T) fbHarness {
	t.Helper()
	hs := newHarness(t, true)
	buf := &syncBuf{}
	hs.srv.log = slog.New(slog.NewJSONHandler(buf, nil))
	c := hs.login(t)
	return fbHarness{harness: hs, cookie: c, csrf: csrfFrom(t, hs, c), buf: buf}
}

// post submits the page form (csrf in the body) or, with api, the chat
// strip's request (csrf in X-CSRF-Token).
func (f fbHarness) post(form url.Values, api bool) *httptest.ResponseRecorder {
	if api {
		req := httptest.NewRequest("POST", "/feedback", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.AddCookie(f.cookie)
		rr := httptest.NewRecorder()
		f.h.ServeHTTP(rr, req)
		return rr
	}
	form.Set("csrf", f.csrf)
	return f.do("POST", "/feedback", form, f.cookie)
}

// events returns the logged feedback events.
func (f fbHarness) events(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if m["event"] == "feedback" {
			out = append(out, m)
		}
	}
	return out
}

func (f fbHarness) one(t *testing.T) map[string]any {
	t.Helper()
	ev := f.events(t)
	if len(ev) != 1 {
		t.Fatalf("want 1 feedback event, got %d: %s", len(ev), f.buf.String())
	}
	return ev[0]
}

func TestFeedback_GetNoSessionRedirects(t *testing.T) {
	hs := newHarness(t, true)
	rr := hs.do("GET", "/feedback", nil, nil)
	if rr.Code != 303 || rr.Header().Get("Location") != "/" {
		t.Fatalf("got %d %q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestFeedback_GetShowsForm(t *testing.T) {
	f := newFB(t)
	rr := f.do("GET", "/feedback?source=menu&from=/clusters/site-1/c-1", nil, f.cookie)
	body := rr.Body.String()
	if rr.Code != 200 {
		t.Fatalf("code %d", rr.Code)
	}
	for _, want := range []string{"Rate your experience", `name="rating" value="5"`, `value="menu"`, `value="/clusters/site-1/c-1"`, "Anything else you'd like to tell us?"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q", want)
		}
	}
}

func TestFeedback_PostRequiresCSRF(t *testing.T) {
	f := newFB(t)
	if rr := f.do("POST", "/feedback", url.Values{"rating": {"4"}}, f.cookie); rr.Code != 403 {
		t.Fatalf("no csrf: %d", rr.Code)
	}
	req := httptest.NewRequest("POST", "/feedback", strings.NewReader("rating=4"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", "wrong")
	req.AddCookie(f.cookie)
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("wrong header: %d", rr.Code)
	}
	if n := len(f.events(t)); n != 0 {
		t.Fatalf("logged %d events", n)
	}
}

func TestFeedback_PostNoSession(t *testing.T) {
	hs := newHarness(t, true)
	if rr := hs.do("POST", "/feedback", url.Values{"rating": {"4"}}, nil); rr.Code != 303 {
		t.Fatalf("form: %d", rr.Code)
	}
	req := httptest.NewRequest("POST", "/feedback", strings.NewReader("rating=4"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", "x")
	rr := httptest.NewRecorder()
	hs.h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("api: %d", rr.Code)
	}
}

func TestFeedback_PostFormLogs(t *testing.T) {
	f := newFB(t)
	from := "/clusters/site-1/c-1?c=" + fbConv
	rr := f.post(url.Values{"rating": {"4"}, "comment": {"  slow tools  "}, "source": {"footer"}, "from": {from}}, false)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Thanks for your feedback") {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	ev := f.one(t)
	want := map[string]any{"username": "jdoe", "principal": principal, "rating": float64(4), "comment": "slow tools",
		"source": "footer", "page": from, "site": "site-1", "cluster": "c-1", "conversation": fbConv, "version": "v-test"}
	for k, v := range want {
		if ev[k] != v {
			t.Errorf("%s = %v, want %v", k, ev[k], v)
		}
	}
	if id, _ := ev["feedback_id"].(string); !a2a.ValidUUID(id) {
		t.Errorf("feedback_id %q", id)
	}
}

func TestFeedback_PostAPIMode(t *testing.T) {
	f := newFB(t)
	rr := f.post(url.Values{"rating": {"5"}, "source": {"chat"}, "conversation": {fbConv}}, true)
	if rr.Code != 204 || rr.Body.Len() != 0 {
		t.Fatalf("code %d body %q", rr.Code, rr.Body.String())
	}
	ev := f.one(t)
	if ev["source"] != "chat" || ev["conversation"] != fbConv || ev["rating"] != float64(5) {
		t.Fatalf("event %v", ev)
	}
}

func TestFeedback_CommentOnlyOK(t *testing.T) {
	f := newFB(t)
	if rr := f.post(url.Values{"comment": {"nice"}}, true); rr.Code != 204 {
		t.Fatalf("code %d", rr.Code)
	}
	if ev := f.one(t); ev["rating"] != float64(0) || ev["comment"] != "nice" {
		t.Fatalf("event %v", ev)
	}
}

func TestFeedback_EmptyRejected(t *testing.T) {
	f := newFB(t)
	rr := f.post(url.Values{"comment": {"   "}}, false)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "Pick a rating or write a comment.") {
		t.Fatalf("form: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.post(url.Values{}, true); rr.Code != 400 {
		t.Fatalf("api: %d", rr.Code)
	}
	if n := len(f.events(t)); n != 0 {
		t.Fatalf("logged %d", n)
	}
}

func TestFeedback_RatingBounds(t *testing.T) {
	for _, tc := range []struct {
		rating string
		code   int
	}{{"0", 400}, {"6", 400}, {"x", 400}, {"-1", 400}, {"1", 204}, {"5", 204}} {
		f := newFB(t)
		if rr := f.post(url.Values{"rating": {tc.rating}}, true); rr.Code != tc.code {
			t.Errorf("rating %q: %d, want %d", tc.rating, rr.Code, tc.code)
		}
	}
	f := newFB(t)
	if rr := f.post(url.Values{"rating": {"9"}}, false); !strings.Contains(rr.Body.String(), "Pick a rating from 1 to 5.") {
		t.Errorf("form message missing: %s", rr.Body.String())
	}
}

func TestFeedback_CommentCap(t *testing.T) {
	f := newFB(t)
	if rr := f.post(url.Values{"comment": {strings.Repeat("a", 2000)}}, true); rr.Code != 204 {
		t.Fatalf("2000: %d", rr.Code)
	}
	rr := f.post(url.Values{"comment": {strings.Repeat("a", 2001)}}, false)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "Your comment is too long (2000 characters max).") {
		t.Fatalf("2001: %d", rr.Code)
	}
}

func TestFeedback_CommentMultibyteAtCap(t *testing.T) {
	f := newFB(t)
	if rr := f.post(url.Values{"comment": {strings.Repeat("界", 2000)}}, true); rr.Code != 204 {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestFeedback_FromValidation(t *testing.T) {
	for _, tc := range []struct{ from, want string }{
		{"//evil.example", ""}, {"https://evil.example", ""}, {`/\evil`, ""},
		{"/" + strings.Repeat("a", 512), ""}, {"/home", "/home"},
	} {
		f := newFB(t)
		if rr := f.post(url.Values{"rating": {"3"}, "from": {tc.from}}, true); rr.Code != 204 {
			t.Fatalf("%q: %d", tc.from, rr.Code)
		}
		if got := f.one(t)["page"]; got != tc.want {
			t.Errorf("from %q logged page %v, want %q", tc.from, got, tc.want)
		}
	}
}

func TestFeedback_BadSourceAndConversation(t *testing.T) {
	f := newFB(t)
	f.post(url.Values{"rating": {"3"}, "source": {"x"}, "conversation": {"not-a-uuid"}}, true)
	ev := f.one(t)
	if ev["source"] != "footer" || ev["conversation"] != "" {
		t.Fatalf("event %v", ev)
	}
}

func TestFeedback_RateLimited(t *testing.T) {
	f := newFB(t)
	for i := 0; i < 10; i++ {
		if rr := f.post(url.Values{"rating": {"3"}}, true); rr.Code != 204 {
			t.Fatalf("post %d: %d", i, rr.Code)
		}
	}
	if rr := f.post(url.Values{"rating": {"3"}}, true); rr.Code != 429 {
		t.Fatalf("11th api: %d", rr.Code)
	}
	rr := f.post(url.Values{"rating": {"3"}}, false)
	if rr.Code != 429 || !strings.Contains(rr.Body.String(), "You&#39;ve sent a lot of feedback recently. Please try again later.") {
		t.Fatalf("11th form: %d %s", rr.Code, rr.Body.String())
	}
	*f.clock = f.clock.Add(61 * time.Minute)
	if rr := f.post(url.Values{"rating": {"3"}}, true); rr.Code != 204 {
		t.Fatalf("after window: %d", rr.Code)
	}
}

func TestFooter_OnInnerPages(t *testing.T) {
	f := newFB(t)
	body := strings.ToLower(f.do("GET", "/home", nil, f.cookie).Body.String())
	for _, want := range []string{`class="site-footer"`, `href="/feedback?source=footer&amp;from=%2fhome"`, `target="_blank"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
}

func TestFooter_NotOnLoginOrLoggedOut(t *testing.T) {
	f := newFB(t)
	pages := map[string]string{
		"login":     f.do("GET", "/", nil, nil).Body.String(),
		"loggedout": f.do("POST", "/logout", url.Values{"csrf": {f.csrf}}, f.cookie).Body.String(),
	}
	for name, body := range pages {
		if strings.Contains(body, "site-footer") || strings.Contains(body, "/feedback") {
			t.Errorf("%s page has a feedback link", name)
		}
	}
}

func TestUserMenu_FeedbackLink(t *testing.T) {
	f := newFB(t)
	body := f.do("GET", "/home", nil, f.cookie).Body.String()
	menu, logout := strings.Index(body, "/feedback?source=menu"), strings.Index(body, `action="/logout"`)
	if menu < 0 || logout < 0 || menu > logout {
		t.Fatalf("menu link at %d, logout form at %d", menu, logout)
	}
}

func TestFooter_ClusterPagePath(t *testing.T) {
	f := newFB(t)
	f.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady}
	body := f.do("GET", "/clusters/site-1/c-abc12?c="+fbConv, nil, f.cookie).Body.String()
	want := "from=" + strings.ToLower(url.QueryEscape("/clusters/site-1/c-abc12?c="+fbConv))
	if !strings.Contains(strings.ToLower(body), want) {
		t.Fatalf("footer link lacks %q", want)
	}
}

func TestClusterPage_FeedbackStripMarkup(t *testing.T) {
	f := newFB(t)
	f.cl.check = clusters.CheckResult{Outcome: clusters.OutcomeReady}
	body := f.do("GET", "/clusters/site-1/c-abc12", nil, f.cookie).Body.String()
	logEnd := strings.Index(body, `<div id="log" aria-live="polite"></div>`)
	strip := strings.Index(body, `id="feedback-strip"`)
	composer := strings.Index(body, `class="composer"`)
	if logEnd < 0 || strip < logEnd || composer < strip {
		t.Fatalf("log %d, strip %d, composer %d", logEnd, strip, composer)
	}
	for _, want := range []string{`data-feedback-url="/feedback"`, `data-page="/clusters/site-1/c-abc12"`} {
		if !strings.Contains(body, want) {
			t.Errorf("chat panel lacks %s", want)
		}
	}
}

// Browsers count a textarea newline as one character for maxlength but
// submit it as CRLF, so a comment at the cap must still be accepted.
func TestFeedback_CRLFCommentAtCap(t *testing.T) {
	f := newFB(t)
	c := strings.Repeat(strings.Repeat("a", 99)+"\r\n", 20) // 2000 chars with LF, 2020 with CRLF
	rr := f.post(url.Values{"comment": {c}}, false)
	if rr.Code != 200 {
		t.Fatalf("code %d", rr.Code)
	}
	if got := f.one(t)["comment"].(string); strings.Contains(got, "\r") {
		t.Fatalf("comment kept CR: %q", got[:110])
	}
}

// Links on the feedback page itself point back at the page the user came
// from, not at the feedback page (no nested from=/feedback?...).
func TestFooter_FeedbackPageKeepsFrom(t *testing.T) {
	f := newFB(t)
	body := strings.ToLower(f.do("GET", "/feedback?source=footer&from=/home", nil, f.cookie).Body.String())
	for _, want := range []string{
		`href="/feedback?source=footer&amp;from=%2fhome"`,
		`href="/feedback?source=menu&amp;from=%2fhome"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("feedback page lacks %q", want)
		}
	}
	if strings.Contains(body, "%2ffeedback") {
		t.Errorf("feedback page links nest the feedback page's own path")
	}
}

// An oversized page-form post (no X-CSRF-Token header) is refused as too
// large, not as a CSRF failure, and logs nothing.
func TestFeedback_OversizedFormIs413(t *testing.T) {
	f := newFB(t)
	rr := f.post(url.Values{"rating": {"3"}, "comment": {strings.Repeat("a", maxFeedbackForm+1)}}, false)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d body %q", rr.Code, rr.Body.String())
	}
	if n := len(f.events(t)); n != 0 {
		t.Fatalf("logged %d events", n)
	}
}
