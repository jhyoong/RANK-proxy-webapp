package clusters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
)

// Outcome is the classified result of an access check.
type Outcome string

const (
	OutcomeReady        Outcome = "ready"
	OutcomeDisconnected Outcome = "disconnected"
	OutcomeNoAccess     Outcome = "no-access"
	OutcomeNotInstalled Outcome = "not-installed"
	OutcomeNotRunning   Outcome = "not-running"
	OutcomeUnreachable  Outcome = "unreachable"
	OutcomeDenied       Outcome = "denied"
	OutcomeExpired      Outcome = "expired"
	OutcomeBadCard      Outcome = "bad-card"
	OutcomeUnexpected   Outcome = "unexpected"
	OutcomeCheckFailed  Outcome = "check-failed"
)

// CheckResult is the outcome of a two-step kagent access check.
type CheckResult struct {
	Outcome     Outcome
	Message     string // user-facing copy
	SSARAllowed *bool  // nil if not run or failed
	SSARReason  string
	SSARCode    int // HTTP code if the SSAR call failed with one
	SSARLatency time.Duration
	CardCode    int // 0 if not run / transport error
	CardLatency time.Duration
	Card        *rancher.Card
	Error       string // short technical detail for the logs
}

func expiredMsg(site string) string {
	return fmt.Sprintf("Your session on %s has expired: log in again", site)
}

// Check runs the permission check, then the agent card fetch, as the user.
// Results are never cached.
func (s *Service) Check(ctx context.Context, sites []auth.SiteResult, row Row, principal string) CheckResult {
	var res CheckResult
	var rancherBody string
	defer func() {
		allowed := "none"
		if res.SSARAllowed != nil {
			allowed = fmt.Sprint(*res.SSARAllowed)
		}
		args := []any{"event", "cluster_check", "site", row.Site, "cluster", row.ID, "principal", principal,
			"ssar_allowed", allowed, "ssar_ms", res.SSARLatency.Milliseconds(),
			"ssar_reason", res.SSARReason, "ssar_http_code", res.SSARCode,
			"card_http_code", res.CardCode, "card_ms", res.CardLatency.Milliseconds(), "result", string(res.Outcome),
			"error", res.Error}
		if c := res.Card; c != nil {
			args = append(args, "card_name", c.Name, "card_version", c.Version, "card_streaming", c.Streaming)
		}
		if rancherBody != "" {
			args = append(args, "rancher_body", rancherBody)
		}
		s.log.Info("cluster check", args...)
	}()

	var sr *auth.SiteResult
	for i := range sites {
		if sites[i].Site == row.Site && sites[i].Status == auth.StatusOK {
			sr = &sites[i]
		}
	}
	c := s.clients[row.Site]
	if sr == nil || c == nil {
		res.Outcome, res.Message, res.Error = OutcomeExpired, expiredMsg(row.Site), "not logged in"
		return res
	}
	if !row.Connected {
		res.Outcome, res.Message = OutcomeDisconnected, "Cluster is disconnected in Rancher"
		return res
	}
	bearer := sr.Token.Bearer

	start := time.Now()
	allowed, reason, err := c.CanI(ctx, bearer, row.ID, rancher.AccessAttrs{
		Namespace: s.kagent.Namespace, Verb: "create", Resource: "services",
		Subresource: "proxy", Name: s.kagent.Service,
	})
	res.SSARLatency = time.Since(start)
	if err != nil {
		var se *rancher.StatusError
		if errors.As(err, &se) {
			res.SSARCode = se.Code
			if se.Code != 401 && se.Code != 403 && se.Code != 404 {
				rancherBody = se.Body
			}
			res.Error = fmt.Sprintf("HTTP %d from Rancher", se.Code)
			if se.Code == 401 {
				res.Outcome, res.Message = OutcomeExpired, expiredMsg(row.Site)
				return res
			}
		} else {
			res.Error = auth.ShortErr(err)
		}
		res.Outcome, res.Message = OutcomeCheckFailed, "Permission check failed: "+res.Error
		return res
	}
	res.SSARAllowed, res.SSARReason = &allowed, reason
	if !allowed {
		res.Outcome, res.Message = OutcomeNoAccess, "You don't have kagent access on this cluster"
		return res
	}

	start = time.Now()
	card, err := c.AgentCard(ctx, bearer, row.ID, s.kagent.Namespace, s.kagent.Service, s.kagent.AgentNamespace, s.kagent.AgentName)
	res.CardLatency = time.Since(start)
	if err == nil {
		res.CardCode = 200
		res.Card = &card
		res.Outcome, res.Message = OutcomeReady, "kagent is ready"
		return res
	}
	var se *rancher.StatusError
	switch {
	case errors.As(err, &se):
		res.CardCode = se.Code
		res.Error = fmt.Sprintf("HTTP %d from Rancher", se.Code)
		if se.Code != 401 && se.Code != 403 && se.Code != 404 {
			rancherBody = se.Body
		}
		switch se.Code {
		case 404:
			res.Outcome, res.Message = OutcomeNotInstalled, "kagent or the agent isn't installed on this cluster"
		case 503:
			res.Outcome, res.Message = OutcomeNotRunning, "kagent is installed but not running"
		case 502:
			res.Outcome, res.Message = OutcomeUnreachable, "Cluster unreachable through Rancher"
		case 403:
			res.Outcome, res.Message = OutcomeDenied, "Access denied by the cluster (the permission check said yes)"
		case 401:
			res.Outcome, res.Message = OutcomeExpired, expiredMsg(row.Site)
		default:
			res.Outcome, res.Message = OutcomeUnexpected, fmt.Sprintf("Unexpected response (HTTP %d)", se.Code)
		}
	case errors.Is(err, rancher.ErrBadResponse):
		var be *rancher.BadResponseError
		if errors.As(err, &be) {
			res.CardCode = be.Code
		}
		res.Error = auth.ShortErr(err)
		res.Outcome, res.Message = OutcomeBadCard, "Unexpected agent card format"
	default:
		res.Error = auth.ShortErr(err)
		res.Outcome, res.Message = OutcomeUnreachable, "Cluster unreachable through Rancher"
	}
	return res
}
