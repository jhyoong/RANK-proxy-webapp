package web

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jhyoong/RANK-proxy-webapp/internal/session"
)

const revokeConcurrency = 8

// Revoke logs out the Rancher tokens of every ended session, at most
// revokeConcurrency sessions at a time. It returns when all are done.
func Revoke(ctx context.Context, a Authn, ended []session.Ended, log *slog.Logger) {
	sem := make(chan struct{}, revokeConcurrency)
	var wg sync.WaitGroup
	for _, e := range ended {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			log.Info("session ended", "event", "session_ended", "reason", e.Reason,
				"username", e.Session.Username, "principal", e.Session.Principal)
			a.Logout(ctx, e.Session.Sites)
		}()
	}
	wg.Wait()
}
