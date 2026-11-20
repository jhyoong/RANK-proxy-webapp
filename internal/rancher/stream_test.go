package rancher_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
)

func streamClient(t *testing.T, h http.HandlerFunc, timeout time.Duration) *rancher.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := rancher.NewClient(config.Site{Name: "s", URL: srv.URL}, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPostStream_SendsHeadersAndBody(t *testing.T) {
	proceed := make(chan struct{})
	type seen struct{ method, auth, accept, ctype, body, path string }
	got := make(chan seen, 1)
	c := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("Content-Type"), string(b), r.URL.EscapedPath()}
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("one\n"))
		fl.Flush()
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("two\n"))
		fl.Flush()
		// Do not write the last chunk until the client has read the first.
		select {
		case <-proceed:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte("three\n"))
		fl.Flush()
	}, 5*time.Second)

	rc, err := c.PostStream(context.Background(), "tok", "/a:b/c/", map[string]string{"k": "v"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rc, buf); err != nil || string(buf) != "one\n" {
		t.Fatalf("first chunk %q err %v", buf, err)
	}
	close(proceed)
	rest, err := io.ReadAll(rc)
	if err != nil || string(rest) != "two\nthree\n" {
		t.Fatalf("rest %q err %v", rest, err)
	}
	s := <-got
	if s.method != "POST" || s.auth != "Bearer tok" || s.accept != "text/event-stream" ||
		s.ctype != "application/json" || s.body != `{"k":"v"}` || s.path != "/a:b/c/" {
		t.Fatalf("server saw %+v", s)
	}
}

func TestPostStream_NoTotalTimeout(t *testing.T) {
	c := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			_, _ = w.Write([]byte("x"))
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}, 100*time.Millisecond)
	rc, err := c.PostStream(context.Background(), "tok", "/s", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil || string(b) != "xxxx" {
		t.Fatalf("read %q err %v", b, err)
	}
}

func TestPostStream_StatusError(t *testing.T) {
	c := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden here", 403)
	}, time.Second)
	_, err := c.PostStream(context.Background(), "tok", "/s", nil, 0)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 403 || se.Body == "" {
		t.Fatalf("err = %v", err)
	}
}

func TestPostStream_HeaderTimeout(t *testing.T) {
	c := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}, time.Second)
	start := time.Now()
	_, err := c.PostStream(context.Background(), "tok", "/s", nil, 100*time.Millisecond)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestPostStream_CtxCancelAbortsRead(t *testing.T) {
	c := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A header timeout must not tie the body to the derived context.
	rc, err := c.PostStream(ctx, "tok", "/s", nil, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	done := make(chan error, 1)
	go func() { _, err := rc.Read(make([]byte, 1)); done <- err }()
	time.Sleep(150 * time.Millisecond) // past headerTimeout: body must still be open
	select {
	case err := <-done:
		t.Fatalf("read ended early: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(time.Second):
		t.Fatal("read not aborted")
	}
}

func TestPostStream_NoRedirect(t *testing.T) {
	c := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}, time.Second)
	_, err := c.PostStream(context.Background(), "tok", "/s", nil, 0)
	var se *rancher.StatusError
	if !errors.As(err, &se) || se.Code != 307 {
		t.Fatalf("err = %v", err)
	}
}
