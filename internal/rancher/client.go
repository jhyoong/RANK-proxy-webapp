// Package rancher is a minimal client for one Rancher site: AD login, the
// current user's principals, and logout.
package rancher

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
)

const maxBody = 1 << 20

// Client talks to one Rancher site.
type Client struct {
	Name string
	base string
	http *http.Client
}

// Token is a Rancher login token. Bearer is opaque and must never be logged
// or shown; ID is safe to display.
type Token struct {
	Bearer    string
	ID        string
	UserID    string
	ExpiresAt string
}

// User is the Rancher user behind a token.
type User struct {
	ID           string
	Username     string
	PrincipalIDs []string
}

// ErrBadResponse means Rancher answered 2xx with a body the portal could not
// use (not JSON, wrong shape, or no token).
var ErrBadResponse = errors.New("unexpected response from Rancher")

// StatusError is a non-2xx response from Rancher.
type StatusError struct {
	Code int
	Body string // first 200 bytes, for logs only
}

func (e *StatusError) Error() string { return fmt.Sprintf("rancher returned HTTP %d", e.Code) }

// BadResponseError is ErrBadResponse for a 2xx body that failed to decode; it
// carries the HTTP status so callers can report it.
type BadResponseError struct {
	Code int
	msg  string
}

func (e *BadResponseError) Error() string        { return ErrBadResponse.Error() + ": " + e.msg }
func (e *BadResponseError) Is(target error) bool { return target == ErrBadResponse }

// NewClient builds a client for site. timeout bounds each request.
func NewClient(site config.Site, timeout time.Duration) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if site.CAFile != "" {
		pem, err := os.ReadFile(site.CAFile)
		if err != nil {
			return nil, fmt.Errorf("site %s: %w", site.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("site %s: no certificates in %s", site.Name, site.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &Client{
		Name: site.Name,
		base: site.URL,
		http: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			// Never follow redirects: a 307/308 would re-send the password
			// elsewhere, and a 301/302 turns the POST into a confusing GET.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, bearer string, body any, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, maxBody)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(lr, 200))
		return &StatusError{Code: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, lr)
		return nil
	}
	if err := json.NewDecoder(lr).Decode(out); err != nil {
		return &BadResponseError{Code: resp.StatusCode, msg: fmt.Sprintf("decode %s: %v", path, err)}
	}
	return nil
}

// Login authenticates against the site's Active Directory provider and asks
// for a token valid for ttl.
func (c *Client) Login(ctx context.Context, username, password string, ttl time.Duration) (Token, error) {
	req := map[string]any{
		"type":         "activeDirectoryProvider",
		"username":     username,
		"password":     password,
		"responseType": "json",
		"ttl":          ttl.Milliseconds(),
	}
	var resp struct {
		Token     string `json:"token"`
		ID        string `json:"id"`
		UserID    string `json:"userId"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1-public/login", nil, "", req, &resp); err != nil {
		return Token{}, err
	}
	if resp.Token == "" {
		return Token{}, fmt.Errorf("%w: login response has no token field", ErrBadResponse)
	}
	return Token{Bearer: resp.Token, ID: resp.ID, UserID: resp.UserID, ExpiresAt: resp.ExpiresAt}, nil
}

// Me returns the user behind bearer via GET /v3/users?me=true.
func (c *Client) Me(ctx context.Context, bearer string) (User, error) {
	var resp struct {
		Data []struct {
			ID           string   `json:"id"`
			Username     string   `json:"username"`
			PrincipalIDs []string `json:"principalIds"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v3/users", url.Values{"me": {"true"}}, bearer, nil, &resp); err != nil {
		return User{}, err
	}
	if len(resp.Data) == 0 {
		return User{}, fmt.Errorf("%w: users?me=true returned no user", ErrBadResponse)
	}
	d := resp.Data[0]
	return User{ID: d.ID, Username: d.Username, PrincipalIDs: d.PrincipalIDs}, nil
}

// Logout deletes the token at the site via POST /v3/tokens?action=logout.
func (c *Client) Logout(ctx context.Context, bearer string) error {
	return c.do(ctx, http.MethodPost, "/v3/tokens", url.Values{"action": {"logout"}}, bearer, nil, nil)
}
