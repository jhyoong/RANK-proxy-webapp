// Package clusters builds the merged, per-session cluster list across sites.
package clusters

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/RANK-proxy-webapp/internal/auth"
	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
	"github.com/jhyoong/RANK-proxy-webapp/internal/rancher"
)

const localCluster = "local"

// Service lists clusters (and, later, checks kagent) using each user's own token.
type Service struct {
	clients      map[string]*rancher.Client
	kagent       config.KagentConfig
	includeLocal bool
	log          *slog.Logger
}

// New builds a Service over the configured site clients. Each site's "local"
// cluster is dropped from lists unless includeLocal is set.
func New(clients []*rancher.Client, kagent config.KagentConfig, includeLocal bool, log *slog.Logger) *Service {
	m := make(map[string]*rancher.Client, len(clients))
	for _, c := range clients {
		m[c.Name] = c
	}
	return &Service{clients: m, kagent: kagent, includeLocal: includeLocal, log: log}
}

// Row is one cluster on one site.
type Row struct {
	Site string
	rancher.Cluster
}

// SiteList is the list outcome for one site.
type SiteList struct {
	Site     string
	Status   string // "ok", "error", "skipped" (site not logged in)
	HTTPCode int
	Count    int
	Latency  time.Duration
	Error    string
}

// Listing is immutable once returned; callers replace it, never mutate it.
type Listing struct {
	Fetched time.Time
	Sites   []SiteList // config order
	Rows    []Row      // merged, local dropped (unless included), sorted
}

// List fetches every logged-in site in parallel and merges the results.
func (s *Service) List(ctx context.Context, sites []auth.SiteResult) *Listing {
	l := &Listing{Fetched: time.Now(), Sites: make([]SiteList, len(sites))}
	perSite := make([][]Row, len(sites))
	var wg sync.WaitGroup
	for i, sr := range sites {
		l.Sites[i] = SiteList{Site: sr.Site, Status: "skipped"}
		c := s.clients[sr.Site]
		if sr.Status != auth.StatusOK || c == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			cs, err := c.ListClusters(ctx, sr.Token.Bearer)
			sl := SiteList{Site: sr.Site, Latency: time.Since(start)}
			if err != nil {
				sl.Status = "error"
				var se *rancher.StatusError
				var be *rancher.BadResponseError
				switch {
				case errors.As(err, &se):
					sl.HTTPCode = se.Code
					if se.Code == 401 {
						sl.Error = "token expired or revoked: log in again"
					} else {
						sl.Error = fmt.Sprintf("HTTP %d from Rancher", se.Code)
					}
				case errors.As(err, &be):
					sl.HTTPCode = be.Code
					sl.Error = auth.ShortErr(err)
				default:
					sl.Error = auth.ShortErr(err)
				}
			} else {
				sl.Status = "ok"
				for _, cc := range cs {
					if cc.ID == localCluster && !s.includeLocal {
						continue
					}
					perSite[i] = append(perSite[i], Row{Site: sr.Site, Cluster: cc})
				}
				sl.Count = len(perSite[i])
			}
			l.Sites[i] = sl
		}()
	}
	wg.Wait()

	for _, rows := range perSite {
		sort.SliceStable(rows, func(a, b int) bool {
			na, nb := strings.ToLower(rows[a].DisplayName), strings.ToLower(rows[b].DisplayName)
			if na != nb {
				return na < nb
			}
			return rows[a].ID < rows[b].ID
		})
		l.Rows = append(l.Rows, rows...)
	}

	type logSite struct {
		Site     string `json:"site"`
		Status   string `json:"status"`
		HTTPCode int    `json:"http_code"`
		Count    int    `json:"count"`
		MS       int64  `json:"ms"`
	}
	ls := make([]logSite, len(l.Sites))
	for i, sl := range l.Sites {
		ls[i] = logSite{sl.Site, sl.Status, sl.HTTPCode, sl.Count, sl.Latency.Milliseconds()}
	}
	// Only unhealthy clusters are listed, which keeps the line small.
	type notConnected struct {
		Site            string `json:"site"`
		ID              string `json:"id"`
		Name            string `json:"name"`
		State           string `json:"state"`
		ConnectedReason string `json:"connected_reason"`
		ReadyReason     string `json:"ready_reason"`
	}
	nc := []notConnected{}
	for _, r := range l.Rows {
		if r.Connected {
			continue
		}
		e := notConnected{Site: r.Site, ID: r.ID, Name: r.DisplayName, State: r.State}
		for _, c := range r.Conditions {
			switch c.Type {
			case "Connected":
				e.ConnectedReason = c.Reason
			case "Ready":
				e.ReadyReason = c.Reason
			}
		}
		nc = append(nc, e)
	}
	s.log.Info("clusters", "event", "clusters_list", "sites", ls, "not_connected", nc)
	// One WARN per failed site, after Wait so the order is config order.
	for _, sl := range l.Sites {
		if sl.Status == "error" {
			s.log.Warn("clusters", "event", "clusters_list_error", "site", sl.Site,
				"http_code", sl.HTTPCode, "ms", sl.Latency.Milliseconds(), "error", sl.Error)
		}
	}
	return l
}

// Find returns the row for site and cluster id, if listed.
func (l *Listing) Find(site, id string) (Row, bool) {
	for _, r := range l.Rows {
		if r.Site == site && r.ID == id {
			return r, true
		}
	}
	return Row{}, false
}

// Filter returns rows whose DisplayName or ID contains q, case-insensitively.
// An empty q returns all rows.
func (l *Listing) Filter(q string) []Row {
	if q == "" {
		return l.Rows
	}
	q = strings.ToLower(q)
	var out []Row
	for _, r := range l.Rows {
		if strings.Contains(strings.ToLower(r.DisplayName), q) || strings.Contains(strings.ToLower(r.ID), q) {
			out = append(out, r)
		}
	}
	return out
}
