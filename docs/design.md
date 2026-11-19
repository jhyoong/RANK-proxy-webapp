# Kagent Chat Portal via Rancher: Design

As of 2026-11-19. Original plan, unchanged apart from formatting. Known inaccuracies and the checks to run against the target environment are in [verification.md](verification.md).

## Overview and core concept

The portal is a thin, Rancher-aware chat proxy: users log in with their AD admin account, pick a cluster they can access, and chat with the custom kagent agent in that cluster over A2A.

**Core concept: every request to a cluster goes through Rancher.** The portal never connects to a downstream cluster directly. It sends each A2A request to the Rancher site that manages the cluster, through Rancher's cluster proxy (`/k8s/clusters/<cluster-id>/...`), using the logged-in user's own Rancher token. Rancher and the downstream Kubernetes API server then decide whether that user may reach the kagent service.

This gives three properties by design:

- **Access control without changing kagent.** kagent v0.9.6 has no authentication. Kubernetes RBAC on the `services/proxy` permission becomes the gate.
- **No new network exposure.** No ingress, port or tunnel is added to any of the 100+ clusters. Traffic uses Rancher's existing agent tunnel.
- **No privileged portal identity.** The portal holds no service account with access to clusters. It can only do what the logged-in user can do.

## Goals, non-goals and environment

The goal is one safe entry point to the read-only troubleshooting agent across all clusters, replacing the current practice of opening a Rancher shell to run kagent.

**Goals**

- Users log in once and see one combined cluster list from all three Rancher sites.
- Users can only chat with kagent on clusters where Rancher grants them access.
- Chat uses A2A to the kagent controller in the chosen cluster.
- Runs as a container image deployed to Kubernetes.

**Non-goals (this stage)**

- Linking regular accounts to admin accounts. Users log in with the admin account.
- MFA.
- Private chat history. kagent identifies users by a header the caller sets, so history separation is best-effort only.
- Limiting what the agent itself can read. Its ServiceAccount defines that, and it is accepted as-is for now.

**Environment**

| Item | Value |
| --- | --- |
| Rancher sites | 3, each managing different clusters |
| Downstream clusters | 100+ in total |
| Rancher versions | v2.13 and v2.14 |
| Identity source | On-premises Active Directory over LDAP, same directory for all sites; Rancher is bound to admin accounts |
| kagent version | v0.9.6, pushed to every cluster by Fleet |
| Agent | One custom Declarative agent, read-only, defined in YAML and delivered by Fleet; no default agents installed |
| MFA | Not required |

## Phase 0: research before building

**Streaming through the Rancher proxy chain must be researched first.** It decides how the chat feature is built, so no chat code should be written until R1 has a result. The other items can run in parallel.

| ID | Item | What to test | If it fails |
| --- | --- | --- | --- |
| R1 | A2A streaming through the proxy chain (highest priority) | Send an A2A `message/stream` request through `/k8s/clusters/<id>/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/<ns>/<agent>/` with a user token. Check that events arrive one by one, not all at the end. Measure time to first event. Run a long investigation (5+ minutes) to find timeouts. Repeat on all three sites, since each may have a different load balancer or ingress in front of Rancher (check buffering and read-timeout settings there). | Use non-streaming `message/send` and show a "working" indicator. If that times out too, send the task without waiting and poll `tasks/get` every few seconds. Confirm which of these kagent v0.9.6 supports. |
| R2 | kagent multi-turn session bug | In kagent v0.9.4, inbound A2A `message/send` to a Declarative agent fails with `SessionNotFoundError` because the session is created under one user ID and looked up under another (issue #1882). Send two or more messages with the same `contextId` to the agent on v0.9.6 and check the agent pod logs. | Check kagent releases after v0.9.6 for a fix and plan an upgrade. Interim workaround: single-turn chats (a new `contextId` per message), with the portal re-sending short context in the prompt if needed. |
| R3 | Rancher login endpoint on v2.13 and v2.14 | Confirm the request body for `/v1-public/login` with the Active Directory provider, the shape of the token returned (legacy v3 or `ext.cattle.io/v1`), and how to delete the token on logout. | Fall back to `/v3-public/activeDirectoryProviders/activedirectory?action=login`, which is older but still in use. |
| R4 | RBAC name matching for `services/proxy` | Confirm which resource name RBAC matches when the URL uses `http:kagent-controller:8083` (the plain service name or the full string). Test with a SelfSubjectAccessReview and with real calls. Also confirm verbs: GET maps to `get`, POST maps to `create`. | Grant both name forms in the role, or drop `resourceNames` and rely on the namespace scope. |
| R5 | Header pass-through | Confirm `X-User-Id` and `Content-Type` reach kagent unchanged through the chain. | Accept shared history (non-goal) or pass the user ID in the A2A message metadata if kagent reads it there. |

## Architecture

The portal sits between the user and Rancher; it never connects to a downstream cluster directly.

```
Browser ──HTTPS──▶ Portal (Go) ──user's token──▶ Rancher site (1 of 3)
                                                   │ /k8s/clusters/<id>
                                                   ▼ existing agent tunnel
                                         Downstream cluster API server
                                                   │ RBAC: services/proxy
                                                   ▼
                                         kagent-controller:8083 ──▶ agent
```

Each chat request goes to the Rancher site that manages the chosen cluster, with the user's own token. Rancher forwards it through its existing tunnel, and Kubernetes RBAC on the cluster decides whether it reaches the kagent controller.

## Authentication across three sites

The portal never checks passwords itself: it passes the login to Rancher's Active Directory provider and keeps the resulting per-user tokens on the server.

**Login flow**

1. The user submits their AD admin username and password to the portal over HTTPS.
2. The backend logs in to site 1 only (endpoint confirmed in R3).
3. If site 1 rejects the login, stop. Show a generic "login failed" message and make no calls to sites 2 and 3. This keeps one wrong password to one failed AD attempt, not three, which protects users from AD account lockout.
4. If site 1 succeeds, log in to sites 2 and 3 in parallel.
5. If a site is unreachable, continue with the others and mark that site unavailable for this session. The password is not kept, so the user picks the site up again at their next login.
6. If site 1 succeeds but site 2 or 3 rejects the same credentials, log an alert. It points to configuration drift between sites.
7. Discard the password from memory. Never log request bodies.

**Token handling**

- Each session holds up to three Rancher tokens, one per site, stored server-side and encrypted at rest. The browser only receives an opaque session cookie (`HttpOnly`, `Secure`, `SameSite=Strict`).
- Rancher v2.13 introduced `ext.cattle.io/v1` tokens and v2.14 began phasing out legacy v3 tokens. The login code must accept both response shapes and live in one module so it can change without touching the rest.
- Logout deletes the token at every site, then destroys the session.

**Session lifetime**

- Rancher controls token lifetime with `auth-user-session-ttl-minutes` and `auth-user-session-idle-ttl-minutes`. Set these the same on all three sites.
- The portal's absolute and idle timeouts must be equal to or shorter than Rancher's. Suggested starting point: 30 minutes idle, 8 hours absolute (to be confirmed).

**User identity across sites**

The same person has a different Rancher user ID on each site. The portal uses the AD principal ID (`activedirectory_user://<DN>`) as the person's identity, since it is the same on all three sites. It is used for the `X-User-Id` header, the portal's own audit log, and rate limiting.

**Login protection**

Rate-limit login attempts per username and per source IP, below the AD lockout threshold.

## Cluster discovery and permission check

The cluster list comes from Rancher using the user's own tokens, so each user only ever sees clusters Rancher already lets them see.

**Listing clusters**

- For each site in the session, call the Rancher Kubernetes API (`/v1/management.cattle.io.clusters`) with that site's user token. Avoid the v3 API: Rancher treats it as internal and unsupported.
- Merge the results into one list, labelled by site. Show each cluster's display name, site and connection state.
- Exclude each site's `local` management cluster unless kagent is deployed there.
- Grey out clusters Rancher reports as disconnected.
- Cache the list per session for a few minutes, with a manual refresh button.

**Checking kagent access**

- Being able to see a cluster does not mean the user may use kagent there. The portal checks this with a SelfSubjectAccessReview sent through Rancher: `/k8s/clusters/<id>/apis/authorization.k8s.io/v1/selfsubjectaccessreviews`, asking whether the user can `create` on `services/proxy` for the kagent controller service in the `kagent` namespace (name format confirmed in R4).
- With 100+ clusters, run the check when the user selects a cluster, not for every cluster at login. Optionally pre-check in the background with a concurrency limit (for example, 10 at a time) and cache results for 5 minutes.
- If the check is denied, show "You don't have kagent access on this cluster" and do not open the chat.
- This check is for user experience only. The real enforcement happens on every chat request, when Kubernetes evaluates RBAC again.

## Chat flow over A2A through Rancher

Each chat message becomes one A2A JSON-RPC request from the portal backend, sent through the Rancher site that manages the chosen cluster, with that site's user token.

**Request path (kagent v0.9.6)**

```
https://<rancher-site>/k8s/clusters/<cluster-id>/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/<agent-namespace>/<agent-name>/
```

- Agent namespace and name are portal configuration, since Fleet deploys the same agent everywhere.
- Before the first message, the portal fetches the agent card from the same path plus `.well-known/agent.json` to confirm the agent is up and whether it supports streaming.

**Request**

- Method: `message/stream`, or `message/send` if R1 shows streaming does not work.
- Headers: `Authorization: Bearer <user's token for that site>`, `Content-Type: application/json`, `X-User-Id: <AD principal ID>`.
- `contextId`: the portal creates one per conversation and reuses it for follow-up messages. Depends on the outcome of R2.

```json
{
  "jsonrpc": "2.0",
  "id": "<request-id>",
  "method": "message/stream",
  "params": {
    "message": {
      "kind": "message",
      "messageId": "<uuid>",
      "contextId": "<conversation-uuid>",
      "role": "user",
      "parts": [{ "kind": "text", "text": "Why is pod X restarting?" }]
    }
  }
}
```

**Browser to portal**

The browser talks only to the portal. The portal relays agent output to the browser over Server-Sent Events. The Rancher token never leaves the backend.

**Error handling**

| Response | Meaning | Portal action |
| --- | --- | --- |
| 401 | Rancher token expired or deleted | End the session and ask the user to log in again |
| 403 | User lacks kagent access on this cluster | Show "no kagent access" |
| 502, 503 or connection error | Cluster disconnected from Rancher, or kagent controller down | Show "cluster or agent unavailable, try later" |
| Timeout | Long investigation exceeded a timeout in the chain | Handle per the R1 fallback |

**Limits**

Cap message size, concurrent chats per user, and request duration. Exact values come out of R1.

**Audit**

The portal logs user principal, site, cluster, time and `contextId` for every request. Whether to also log message content is an open item.

## Access control model

Access to kagent is granted with one Rancher role, "kagent user", that allows the `services/proxy` permission on the kagent controller service and nothing else.

**Role template** (example; names confirmed in R4)

```yaml
apiVersion: management.cattle.io/v3
kind: RoleTemplate
metadata:
  name: kagent-user
displayName: kagent user
context: cluster
rules:
  - apiGroups: [""]
    resources: ["services/proxy"]
    resourceNames: ["kagent-controller", "http:kagent-controller:8083"]
    verbs: ["get", "create"]
```

- `get` covers reading the agent card. `create` covers sending A2A messages (HTTP POST).
- It is a cluster-scoped role, so it matches any service named `kagent-controller` in any namespace. That is acceptable: kagent is the only such service. A project-scoped role would be tighter but needs the `kagent` namespace placed in a project on all 100+ clusters.
- Role templates are per Rancher site, so the role must exist on all three sites. Manage it as code (Terraform `rancher2_role_template`, or the YAML above applied to each site's local cluster) so the three copies stay identical.

**Granting access**

- Bind the role to a user's admin account on each cluster where they should have kagent access (a ClusterRoleTemplateBinding).
- Users who already hold broad roles, such as cluster owner, already include this permission and need no extra binding.

**What this does and does not protect**

- Protected: users without the role cannot reach kagent on that cluster, whether through the portal or directly through Rancher.
- Accepted: users with the role could call the proxy directly and bypass the portal. They reach the same single read-only agent either way, so this is not a risk at this stage.

## Security controls

The portal handles admin passwords in transit and holds user tokens, so it is secured like any system that brokers privileged credentials.

| Area | Control |
| --- | --- |
| Transport | HTTPS only between browser and portal. Portal verifies each Rancher site's TLS certificate against a configured CA bundle; no "skip verify". |
| Passwords | Passed through to Rancher once, then discarded. Never stored or logged. Request-body logging disabled on login routes. |
| Tokens | Kept server-side only, encrypted at rest with a key from a Kubernetes Secret. Deleted at Rancher on logout. |
| Session cookie | Opaque ID, `HttpOnly`, `Secure`, `SameSite=Strict`. Idle and absolute timeouts no longer than Rancher's. |
| Request forgery | CSRF token on all state-changing requests. |
| Browser hardening | Strict Content Security Policy (no inline scripts, no third-party hosts). Agent output rendered as escaped text or sanitised Markdown, never raw HTML. |
| Login abuse | Rate limits per username and per source IP; only site 1 is tried on a failed login (see Authentication). |
| Portal identity | The portal's own pod has no Rancher or cluster credentials. Every Rancher call uses the logged-in user's token. |
| Network | NetworkPolicy allows egress only to the three Rancher sites, the session store, and DNS. |
| Container | Non-root user, read-only root filesystem, no added capabilities, minimal base image, image scanning in CI. |
| Audit | Portal logs logins, logouts, cluster selections and chat requests with user principal, site, cluster and time. Rancher's own audit log records the same calls under the real user. |

## Technology and deployment

Recommended stack: a Go backend that also serves a small static web UI, packaged as one container image.

**Why Go**

- Its standard HTTP server handles long-lived streaming responses (Server-Sent Events) well, which is the core of the chat relay.
- Builds to a single static binary, so the image can be a minimal distroless image of roughly 20 MB with no shell.
- Rancher and kagent are both written in Go, and the Kubernetes client libraries provide ready-made types (for example, SelfSubjectAccessReview).
- Calling three sites in parallel is simple with Go's concurrency.

**Front end**

Plain HTML with a small amount of JavaScript (or htmx), served by the same binary. No separate front-end build or third-party scripts, which keeps the Content Security Policy strict.

**Module layout**

| Module | Responsibility |
| --- | --- |
| `auth` | Login and logout against Rancher; handles both token shapes |
| `sites` | Per-site configuration, TLS, and HTTP clients |
| `clusters` | Cluster listing and kagent access checks |
| `a2a` | All kagent A2A calls; the only module that changes for kagent 1.x |
| `session` | Encrypted server-side session and token storage |
| `web` | UI, Server-Sent Events relay, CSRF, security headers |

**Kubernetes deployment**

- Deploy to a cluster that can reach all three Rancher sites. It can be delivered by Fleet like other workloads.
- Deployment with 2 replicas, a PodDisruptionBudget, and Redis (or Valkey) as the shared session store so either replica can serve any user. A single replica with in-memory sessions is acceptable for a first test.
- ConfigMap: site names, URLs and CA bundles; agent namespace and name; timeouts and limits.
- Secrets: session encryption key and session store password.
- Ingress for the portal must have response buffering disabled and a read timeout long enough for chat streams (values from R1).
- NetworkPolicy and container security settings as listed in Security controls.

## Version compatibility

The design works on Rancher v2.13 and v2.14 with kagent v0.9.6; two known upstream changes are isolated in their own modules so upgrades stay contained.

| Component | Change | Impact on the portal |
| --- | --- | --- |
| Rancher v2.13 | New `ext.cattle.io/v1` token resource introduced | `auth` module must accept both token shapes |
| Rancher v2.14 | Legacy v3 tokens being phased out, scheduled for removal in a future release; `/v1-public/login` will return the new token shape | Use `/v1-public/login`; avoid v3 API calls elsewhere |
| Rancher (general) | v3 API is internal and unsupported; no new features | Use the Rancher Kubernetes API and `/k8s/clusters/` proxy only |
| kagent 1.x | A2A moves to a gRPC service with header-based agent routing; the `/api/a2a/<namespace>/<agent>/` HTTP paths are replaced | Rewrite only the `a2a` module; re-test streaming through Rancher, since gRPC through the Kubernetes service proxy is a different problem |

Keep all three Rancher sites on the latest patch release of their minor version. Recent v2.13 patches fixed serious authorization issues.

**Sources**

- [Rancher v2.14 tokens and v3 deprecation](https://ranchermanager.docs.rancher.com/v2.14/api/workflows/tokens)
- [Rancher session token migration and /v1-public/login (issue #55988)](https://github.com/rancher/rancher/issues/55988)
- [Rancher v2.11.1 release notes (v3 API status)](https://github.com/rancher/rancher/releases/tag/v2.11.1)
- [Rancher v2.13.11 release notes](https://documentation.suse.com/cloudnative/rancher-manager/v2.13/en/release-notes/v2.13.11.html)
- [kagent 0.x A2A agents](https://kagent.dev/docs/kagent/examples/a2a-agents)
- [kagent 1.x A2A agents](https://kagent.dev/docs/kagent/latest/examples/a2a-agents/)
- [kagent session bug (issue #1882)](https://github.com/kagent-dev/kagent/issues/1882)

## Delivery phases and open items

Work starts with Phase 0 research; its streaming result (R1) gates how chat is built in Phase 1.

1. **Phase 0: Research.** Complete R1 to R5 on one test cluster per site. Gate: R1 decides streaming or the fallback; R2 decides multi-turn or single-turn.
2. **Phase 1: Minimum working portal.** Login to three sites, combined cluster list, access check, and chat with one test cluster per site. Single replica, in-memory sessions.
3. **Phase 2: Hardening and pilot.** Shared session store, 2 replicas, rate limits, audit log, NetworkPolicy, `kagent-user` role rolled out to all three sites. Pilot with a small group of users.
4. **Phase 3: Rollout.** Open to all users, with a short user guide and a process for requesting the `kagent-user` role.

**Open items**

- [ ] Should the portal's audit log include chat message content, or only metadata?
- [ ] Final session timeouts (suggested: 30 minutes idle, 8 hours absolute), aligned with Rancher settings on all sites.
- [ ] Which cluster hosts the portal, and can it reach all three Rancher sites?
- [ ] Who approves and grants the `kagent-user` role, and through what process?
- [ ] Is kagent deployed on any site's `local` cluster, and should it appear in the list?
