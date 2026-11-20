package rancher

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// PostStream POSTs body as JSON with Accept: text/event-stream. A non-2xx
// answer is a *StatusError (body truncated to 200 bytes, response closed). On
// 2xx it returns the open body, which the caller must close. headerTimeout
// bounds the wait for response headers only (0 = none); the body stays tied
// to ctx alone. path is used as given, so ':' is not escaped.
func (c *Client) PostStream(ctx context.Context, bearer, path string, body any, headerTimeout time.Duration) (io.ReadCloser, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	// The request context is cancelled by the header timer; the body read
	// afterwards must not depend on it, so cancel only while waiting.
	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.base+path, rd)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	var timer *time.Timer
	if headerTimeout > 0 {
		timer = time.AfterFunc(headerTimeout, cancel)
	}
	resp, err := c.stream.Do(req)
	if timer != nil && !timer.Stop() {
		// The timer fired; if Do still succeeded the context is dead and
		// the body is unusable.
		if err == nil {
			resp.Body.Close()
			err = context.DeadlineExceeded
		}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		resp.Body.Close()
		cancel()
		return nil, &StatusError{Code: resp.StatusCode, Body: string(b)}
	}
	return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// cancelBody releases the request context when the body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
