package rancher

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

const (
	clustersPath    = "/v1/management.cattle.io.clusters"
	clustersLimit   = "500"
	maxClusterPages = 50
)

// Condition is a Rancher cluster status condition.
type Condition struct{ Type, Status, Reason string }

// Cluster is one downstream cluster as listed by Rancher.
type Cluster struct {
	ID          string // Steve id (== metadata.name)
	DisplayName string // spec.displayName, falling back to ID
	State       string // metadata.state.name, raw
	Connected   bool   // Connected condition if present, else State == "active"
	Conditions  []Condition
}

// ListClusters lists clusters via Steve, following the continue token (top-level or pagination.continue; never
// pagination.next) on the same path, for at most 50 pages.
func (c *Client) ListClusters(ctx context.Context, bearer string) ([]Cluster, error) {
	var out []Cluster
	cont := ""
	for page := 0; page < maxClusterPages; page++ {
		q := url.Values{"limit": {clustersLimit}}
		if cont != "" {
			q.Set("continue", cont)
		}
		var resp struct {
			Data []struct {
				ID       string `json:"id"`
				Metadata struct {
					Name  string `json:"name"`
					State struct {
						Name string `json:"name"`
					} `json:"state"`
				} `json:"metadata"`
				Spec struct {
					DisplayName string `json:"displayName"`
				} `json:"spec"`
				Status struct {
					Conditions []struct {
						Type   string `json:"type"`
						Status string `json:"status"`
						Reason string `json:"reason"`
					} `json:"conditions"`
				} `json:"status"`
			} `json:"data"`
			Continue   string `json:"continue"`
			Pagination struct {
				Continue string `json:"continue"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, clustersPath, q, bearer, nil, &resp); err != nil {
			return nil, err
		}
		for _, d := range resp.Data {
			cl := Cluster{ID: d.ID, DisplayName: d.Spec.DisplayName, State: d.Metadata.State.Name}
			if cl.ID == "" {
				cl.ID = d.Metadata.Name
			}
			if cl.DisplayName == "" {
				cl.DisplayName = cl.ID
			}
			cl.Connected = cl.State == "active"
			for _, cd := range d.Status.Conditions {
				switch cd.Type {
				case "Connected":
					cl.Connected = cd.Status == "True"
				case "Ready":
				default:
					continue
				}
				cl.Conditions = append(cl.Conditions, Condition{Type: cd.Type, Status: cd.Status, Reason: cd.Reason})
			}
			out = append(out, cl)
		}
		// Steve carries the token at the top level; tolerate it under pagination too.
		cont = resp.Continue
		if cont == "" {
			cont = resp.Pagination.Continue
		}
		if cont == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("cluster list exceeded %d pages", maxClusterPages)
}

// AccessAttrs are the resourceAttributes of a SelfSubjectAccessReview.
type AccessAttrs struct{ Namespace, Verb, Resource, Subresource, Name string }

// Card is the subset of an A2A agent card the portal uses.
type Card struct {
	Name, Description, Version string
	Streaming                  bool // capabilities.streaming
}

// CanI asks the cluster, through Rancher's proxy and as the bearer's user,
// whether the user may perform the action described by a.
func (c *Client) CanI(ctx context.Context, bearer, clusterID string, a AccessAttrs) (allowed bool, reason string, err error) {
	path := "/k8s/clusters/" + url.PathEscape(clusterID) + "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	body := map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": map[string]string{
			"namespace": a.Namespace, "verb": a.Verb, "resource": a.Resource,
			"subresource": a.Subresource, "name": a.Name,
		}},
	}
	var resp struct {
		Status struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		} `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, path, nil, bearer, body, &resp); err != nil {
		return false, "", err
	}
	return resp.Status.Allowed, resp.Status.Reason, nil
}

// AgentCard fetches an agent's A2A card through the Kubernetes service proxy.
// url.PathEscape leaves ':' alone, so a service like http:name:8083 reaches
// Rancher unescaped.
func (c *Client) AgentCard(ctx context.Context, bearer, clusterID, namespace, service, agentNamespace, agentName string) (Card, error) {
	e := url.PathEscape
	path := "/k8s/clusters/" + e(clusterID) + "/api/v1/namespaces/" + e(namespace) + "/services/" + e(service) +
		"/proxy/api/a2a/" + e(agentNamespace) + "/" + e(agentName) + "/.well-known/agent-card.json"
	var resp struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Version      string `json:"version"`
		Capabilities struct {
			Streaming bool `json:"streaming"`
		} `json:"capabilities"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, bearer, nil, &resp); err != nil {
		return Card{}, err
	}
	return Card{Name: resp.Name, Description: resp.Description, Version: resp.Version, Streaming: resp.Capabilities.Streaming}, nil
}
