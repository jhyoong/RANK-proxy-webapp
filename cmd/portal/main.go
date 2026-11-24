// Command portal is the kagent chat portal: multi-site Rancher login, the
// cluster list with a per-cluster kagent check, and a chat panel that
// streams kagent replies through Rancher as the logged-in user. See the
// plans in docs/plans.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/chat"
	"github.com/jhyoong/RANK-proxy-webapp/internal/clusters"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/history"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
	"github.com/jhyoong/RANK-proxy-webapp/internal/session"
	"github.com/jhyoong/RANK-proxy-webapp/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fmt.Fprintln(os.Stderr, "portal:", err)
		os.Exit(1)
	}
}

// run starts the portal and blocks until ctx is cancelled. If ready is not
// nil, the bound listen address is sent on it once the server accepts
// connections.
func run(ctx context.Context, args []string, stdout io.Writer, ready chan<- string) error {
	defaultPath := os.Getenv("PORTAL_CONFIG")
	if defaultPath == "" {
		defaultPath = "/etc/portal/config.yaml"
	}
	fsFlags := flag.NewFlagSet("portal", flag.ContinueOnError)
	cfgPath := fsFlags.String("config", defaultPath, "path to config file (env PORTAL_CONFIG)")
	if err := fsFlags.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(stdout, nil))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	var clients, apiClients []*rancher.Client
	var siteAttrs []any
	for _, s := range cfg.Sites {
		c, err := rancher.NewClient(s, cfg.Login.Timeout)
		if err != nil {
			return err
		}
		clients = append(clients, c)
		ac, err := rancher.NewClient(s, cfg.API.Timeout)
		if err != nil {
			return err
		}
		apiClients = append(apiClients, ac)
		siteAttrs = append(siteAttrs, slog.Group(s.Name, "url", s.URL, "ca_file", s.CAFile))
	}

	store := session.NewStore(cfg.Session.IdleTimeout, cfg.Session.AbsoluteTimeout, cfg.Session.MaxPerUser, time.Now)
	authn := auth.New(clients, cfg.Session.AbsoluteTimeout, log)
	handler, err := web.New(cfg, authn, clusters.New(apiClients, cfg.Kagent, cfg.Clusters.IncludeLocal, log),
		chat.New(apiClients, cfg.Kagent, cfg.Chat, log), history.New(apiClients, cfg.Kagent, cfg.Chat.MaxDuration, log), store, version, log)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	// No ReadTimeout or WriteTimeout: a server-wide read deadline would cancel
	// the request context during a long SSE reply, and a write timeout would
	// cut the reply off. Handlers set per-request body read deadlines instead.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	useTLS := cfg.TLS.CertFile != ""
	log.Info("portal listening", "version", version, "listen", ln.Addr().String(), "tls", useTLS,
		"idle_timeout", cfg.Session.IdleTimeout.String(), "absolute_timeout", cfg.Session.AbsoluteTimeout.String(),
		"cookie_secure", cfg.SecureCookies(), "kagent.agent_name", cfg.Kagent.AgentName, slog.Group("sites", siteAttrs...))
	if len(cfg.Login.TrustedProxies) == 0 && cfg.SecureCookies() {
		// Secure cookies imply HTTPS, usually terminated by an ingress.
		log.Warn("login.trusted_proxies is empty: behind an ingress the per-IP login limit sees the ingress address for every user, so all users share one bucket",
			"event", "config_warning")
	}

	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ended := store.Sweep()
				if len(ended) > 0 {
					go func() {
						rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						web.Revoke(rctx, authn, ended, log)
					}()
				}
			}
		}
	}()

	errc := make(chan error, 1)
	go func() {
		if useTLS {
			errc <- srv.ServeTLS(ln, cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	if ready != nil {
		ready <- ln.Addr().String()
	}

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	// Revoke every remaining session's tokens. The 15s + 10s budgets stay
	// inside Kubernetes' default 30s grace period.
	revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRevoke()
	web.Revoke(revokeCtx, authn, store.Drain(), log)
	return shutdownErr
}
