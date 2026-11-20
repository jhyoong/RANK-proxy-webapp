// Package auth runs the portal's login and logout across all Rancher sites.
//
// Login is lockout-safe: sites are tried in config order until one gives a
// definite answer. Any 4xx or 500 from a site ends the login without trying
// another site, so a wrong password costs one AD attempt. Only transport
// errors and 502/503/504 fall through to the next site. After the first
// success the remaining sites are tried in parallel.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
)

// adPrefix marks the Active Directory principal used as the person's identity.
const adPrefix = "activedirectory_user://"

// Status is the outcome of logging in to one site.
type Status string

const (
	StatusOK          Status = "ok"
	StatusRejected    Status = "rejected"     // 4xx or 500 from login
	StatusUnavailable Status = "unavailable"  // transport error or 502/503/504
	StatusSkipped     Status = "skipped"      // not tried because login stopped earlier
	StatusBadResponse Status = "bad-response" // 2xx with a body the portal could not use
)

// SiteResult is the login outcome for one site.
type SiteResult struct {
	Site     string
	Status   Status
	HTTPCode int    // 0 if no HTTP response
	Error    string // short and safe to display; never contains token or body
	Token    rancher.Token
	User     *rancher.User // nil if Me failed or not ok
	MeError  string
	Latency  time.Duration
}

// Result is the outcome of a portal login.
type Result struct {
	OK          bool         // at least one site is StatusOK
	Sites       []SiteResult // config order
	PrincipalID string       // "" if none found
	Warnings    []string
}

// LogoutResult is the logout outcome for one site.
type LogoutResult struct {
	Site      string
	LoggedOut bool
	Revoked   bool // token no longer accepted after logout
	Attempts  int  // revocation checks made (Rancher caches may lag the delete)
	Error     string
}

// Authenticator logs users in and out of every configured site.
type Authenticator struct {
	clients []*rancher.Client
	ttl     time.Duration
	log     *slog.Logger

	revokeTries int
	revokeDelay time.Duration
}

// New returns an Authenticator. ttl is the token lifetime requested at login.
func New(clients []*rancher.Client, ttl time.Duration, log *slog.Logger) *Authenticator {
	return &Authenticator{clients: clients, ttl: ttl, log: log, revokeTries: 5, revokeDelay: 500 * time.Millisecond}
}

// SetRevokeRetry sets how many times, and how far apart, Logout checks that a
// token is no longer accepted. Defaults: 5 tries, 500ms apart.
func (a *Authenticator) SetRevokeRetry(tries int, delay time.Duration) {
	a.revokeTries, a.revokeDelay = max(tries, 1), delay
}

// classify maps a login error to a status, HTTP code and display message.
func classify(err error) (Status, int, string) {
	if errors.Is(err, rancher.ErrBadResponse) {
		return StatusBadResponse, 200, shortErr(err)
	}
	var se *rancher.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case 502, 503, 504:
			return StatusUnavailable, se.Code, fmt.Sprintf("HTTP %d from Rancher", se.Code)
		default:
			return StatusRejected, se.Code, fmt.Sprintf("HTTP %d from Rancher", se.Code)
		}
	}
	return StatusUnavailable, 0, shortErr(err)
}

// shortErr trims a transport error to something safe and readable.
func shortErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && strings.Contains(msg, "://") {
		// Drop the "Post \"https://...\"" prefix and keep the cause.
		msg = msg[i+2:]
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func (a *Authenticator) loginSite(ctx context.Context, c *rancher.Client, username, password string) SiteResult {
	start := time.Now()
	tok, err := c.Login(ctx, username, password, a.ttl)
	r := SiteResult{Site: c.Name, Latency: time.Since(start)}
	if err != nil {
		r.Status, r.HTTPCode, r.Error = classify(err)
		var se *rancher.StatusError
		if errors.As(err, &se) && se.Code != 401 {
			// A 401 is a plain wrong password; anything else needs the body
			// to tell e.g. a Rancher 500 apart from bad credentials.
			a.log.Warn("unexpected login status", "event", "login_http_error", "site", c.Name,
				"http_code", se.Code, "rancher_body", se.Body)
		}
		return r
	}
	r.Status, r.HTTPCode, r.Token = StatusOK, 200, tok
	return r
}

// Login authenticates username/password against all sites. See package doc.
func (a *Authenticator) Login(ctx context.Context, username, password string) Result {
	res := Result{Sites: make([]SiteResult, len(a.clients))}
	for i, c := range a.clients {
		res.Sites[i] = SiteResult{Site: c.Name, Status: StatusSkipped}
	}

	// Phase 1: sequential until a definite answer.
	first := -1
	for i, c := range a.clients {
		r := a.loginSite(ctx, c, username, password)
		res.Sites[i] = r
		if r.Status == StatusOK {
			first = i
			break
		}
		if r.Status == StatusRejected || r.Status == StatusBadResponse {
			break
		}
	}

	// Phase 2: remaining sites in parallel.
	if first >= 0 {
		var wg sync.WaitGroup
		for i := first + 1; i < len(a.clients); i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res.Sites[i] = a.loginSite(ctx, a.clients[i], username, password)
			}(i)
		}
		wg.Wait()
		for i := first + 1; i < len(a.clients); i++ {
			if res.Sites[i].Status == StatusRejected {
				a.log.Warn("credentials rejected by one site after another accepted them",
					"event", "site_credential_drift", "site", res.Sites[i].Site, "http_code", res.Sites[i].HTTPCode)
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s rejected the credentials that %s accepted (configuration drift between sites?)",
					res.Sites[i].Site, res.Sites[first].Site))
			}
		}
		res.OK = true
		a.fetchUsers(ctx, &res)
		a.choosePrincipal(&res)
	}

	a.log.Info("login", "event", "login", "username", username, "ok", res.OK,
		"principal", res.PrincipalID, "sites", logSites(res.Sites))
	return res
}

func (a *Authenticator) fetchUsers(ctx context.Context, res *Result) {
	var wg sync.WaitGroup
	for i := range res.Sites {
		if res.Sites[i].Status != StatusOK {
			continue
		}
		wg.Add(1)
		go func(s *SiteResult, c *rancher.Client) {
			defer wg.Done()
			u, err := c.Me(ctx, s.Token.Bearer)
			if err != nil {
				s.MeError = shortErr(err)
				return
			}
			s.User = &u
		}(&res.Sites[i], a.clients[i])
	}
	wg.Wait()
}

func (a *Authenticator) choosePrincipal(res *Result) {
	var mismatch bool
	for _, s := range res.Sites {
		if s.User == nil {
			continue
		}
		p := adPrincipal(s.User.PrincipalIDs)
		if p == "" {
			continue
		}
		if res.PrincipalID == "" {
			res.PrincipalID = p
		} else if p != res.PrincipalID {
			mismatch = true
		}
	}
	if mismatch {
		a.log.Warn("principal ID differs between sites", "event", "principal_mismatch", "sites", logSites(res.Sites))
		res.Warnings = append(res.Warnings, "principal ID differs between sites; using "+res.Sites[firstOK(res.Sites)].Site+"'s value")
	}
	if res.PrincipalID == "" {
		res.Warnings = append(res.Warnings, "no activedirectory_user principal found on any site")
	}
}

func firstOK(sites []SiteResult) int {
	for i, s := range sites {
		if s.User != nil && adPrincipal(s.User.PrincipalIDs) != "" {
			return i
		}
	}
	return 0
}

func adPrincipal(ids []string) string {
	for _, id := range ids {
		if strings.HasPrefix(id, adPrefix) {
			return id
		}
	}
	return ""
}

// Logout logs out of every site that has a token, then checks the token is
// no longer accepted.
func (a *Authenticator) Logout(ctx context.Context, sites []SiteResult) []LogoutResult {
	var out []LogoutResult
	var idx []int
	for i, s := range sites {
		if s.Status == StatusOK && i < len(a.clients) {
			out = append(out, LogoutResult{Site: s.Site})
			idx = append(idx, i)
		}
	}
	var wg sync.WaitGroup
	for j, i := range idx {
		wg.Add(1)
		go func(lr *LogoutResult, c *rancher.Client, bearer string) {
			defer wg.Done()
			if err := c.Logout(ctx, bearer); err != nil {
				lr.Error = "logout: " + shortErr(err)
			} else {
				lr.LoggedOut = true
			}
			for lr.Attempts < a.revokeTries {
				if lr.Attempts > 0 {
					select {
					case <-time.After(a.revokeDelay):
					case <-ctx.Done():
					}
				}
				lr.Attempts++
				_, err := c.Me(ctx, bearer)
				var se *rancher.StatusError
				if errors.As(err, &se) && (se.Code == 401 || se.Code == 403) {
					lr.Revoked = true
					return
				}
				if err != nil {
					// Not a clear answer; report it and stop checking.
					if lr.Error == "" {
						lr.Error = "revocation check: " + shortErr(err)
					}
					return
				}
			}
			if lr.Error == "" {
				lr.Error = fmt.Sprintf("token still accepted after logout (%d checks)", lr.Attempts)
			}
		}(&out[j], a.clients[i], sites[i].Token.Bearer)
	}
	wg.Wait()
	a.log.Info("logout", "event", "logout", "results", out)
	return out
}

// logSite is the log-safe view of a SiteResult: no bearer.
type logSite struct {
	Site      string `json:"site"`
	Status    Status `json:"status"`
	HTTPCode  int    `json:"http_code"`
	Error     string `json:"error,omitempty"`
	TokenID   string `json:"token_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Principal string `json:"principal,omitempty"`
	MeError   string `json:"me_error,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

func logSites(sites []SiteResult) []logSite {
	out := make([]logSite, len(sites))
	for i, s := range sites {
		out[i] = logSite{Site: s.Site, Status: s.Status, HTTPCode: s.HTTPCode, Error: s.Error,
			TokenID: s.Token.ID, ExpiresAt: s.Token.ExpiresAt, UserID: s.Token.UserID,
			MeError: s.MeError, LatencyMS: s.Latency.Milliseconds()}
		if s.User != nil {
			out[i].Principal = adPrincipal(s.User.PrincipalIDs)
		}
	}
	return out
}
