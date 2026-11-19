# On-site Verification: Brief for Agents

As of 2026-11-19. For agents (human or AI) running checks in the air-gapped target environment before Phase 1 of the Kagent Chat Portal.

This file is self-contained: the environment has no internet access, so upstream links won't open. It covers only what still needs checking. Decisions that are already made are left out on purpose. If [design.md](design.md) disagrees with this file, **trust this file**. design.md is the original plan and is wrong in places.

---

## 1. Background

### What the portal does

A Go web app. Users log in with their Active Directory (AD) admin account, see a combined list of clusters from three Rancher sites, and chat with a read-only kagent troubleshooting agent on the cluster they pick.

**Every request to a cluster goes through Rancher, using the user's own Rancher token.** The portal never connects to a downstream cluster directly and holds no credentials of its own. Kubernetes RBAC on `services/proxy` decides who may reach kagent.

```
Browser ──HTTPS──▶ Portal ──user's token──▶ Rancher site (1 of 3)
                                              │ /k8s/clusters/<cluster-id>
                                              ▼ Rancher agent tunnel
                                    Downstream cluster API server
                                              │ RBAC: services/proxy
                                              ▼
                                    kagent-controller:8083 ──▶ agent pod
```

### Environment

| Item | Value |
| --- | --- |
| Rancher sites | 3, each managing different clusters; versions v2.13 and v2.14 (exact patch per site unknown) |
| Downstream clusters | 100+ |
| Identity | One on-prem AD over LDAP, shared by all sites |
| kagent | v0.9.6, deployed by Fleet; one custom read-only Declarative agent, no default agents |
| Network | Air-gapped. An internal LLM endpoint serves kagent. |

### The request path under test

```
https://<rancher-site>/k8s/clusters/<cluster-id>/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/<agent-ns>/<agent-name>/
```

Agent card: the same path plus `.well-known/agent-card.json`. Keep the trailing `/` on the A2A path.

### What upstream source says (all unconfirmed on site)

These come from reading upstream source code. **Nothing has been tested against a live cluster.** Treat each one as a hypothesis that your checks confirm or disprove.

- **Login.** `POST /v1-public/login` with body `{"type":"activeDirectoryProvider","username":"…","password":"…","responseType":"json","ttl":<ms>}`. On both v2.13 and v2.14 it returns a legacy v3 token: `{"token":"<name>:<key>","expiresAt":…,"id":…,"type":"token"}`.
- **Logout.** `POST /v3/tokens?action=logout` with the token (or `DELETE /v3/tokens/<id>`).
- **RBAC name.** RBAC matches the full path segment `http:kagent-controller:8083` as the resource name, not the plain `kagent-controller`. GET maps to verb `get`, POST maps to `create`.
- **Default roles.** cluster-owner already grants this proxy access. cluster-member does not. Project roles grant it only if the `kagent` namespace is in their project.
- **API server timeout.** The `proxy` subresource counts as long-running, so the API server's 60s request timeout does not apply.
- **kagent timeout.** The controller's client to the agent has a hard total timeout, Helm value `controller.streaming.timeout`, default **600s**. It caps streaming *and* blocking `message/send`. kagent sends no SSE heartbeats and batches events for up to 50 ms.
- **kagent identity.** In the default `unsecure` auth mode, kagent reads the user from the `user_id` query parameter first, then the `X-User-Id` header, else falls back to `admin@kagent.dev`. A `user_id` containing `+` causes 404s (kagent issue #2441). `Authorization` never reaches kagent, because Rancher swaps the token and the API server strips the header.
- **Multi-turn bug.** The `SessionNotFoundError` bug (kagent #1882) was fixed before v0.9.5, so v0.9.6 should already include the fix.
- **Ingress in front of Rancher.** The v2.13 Rancher chart sets 1800s read/send timeouts on its ingress. Later charts may not (unverified). Without those annotations, ingress-nginx defaults to **60s**.

### A2A request bodies (kagent v0.9.6, A2A 0.3)

Streaming:

```json
{"jsonrpc":"2.0","id":"1","method":"message/stream","params":{"message":{
  "kind":"message","messageId":"<uuid>","contextId":"<uuid>","role":"user",
  "parts":[{"kind":"text","text":"List pods in kube-system that restarted today"}]}}}
```

Non-blocking send (same body, method `message/send`, plus `"configuration":{"blocking":false}` inside `params`). Then poll:

```json
{"jsonrpc":"2.0","id":"2","method":"tasks/get","params":{"id":"<task-id>"}}
```

---

## 2. Ground rules

- **AD lockout.** Login tests (E4) use a dedicated test account. Make one attempt, read the result, then decide. Never loop or retry a failed login. Find the lockout threshold (E11) before any repeated login testing.
- **Secrets.** Never print, log or paste passwords or tokens into reports, shell history or chat. Pass them through files with `0600` permissions or environment variables. Redact tokens in output (`token-abc12:****`).
- **Read-only by default.** Don't change Rancher settings, feature flags, Helm values or RBAC. The one exception is the test RoleTemplate and binding in E18, and only if a human has approved it for the test clusters.
- **Logout test.** E6 deletes a token. Use only a token you created for the test.
- **Load.** Long-running tests (R1) run on test clusters only, one at a time per site.
- **Report, don't decide.** If a result contradicts this file, record what you saw and flag it. Don't work around it or change the design.
- **Stop early.** If E14 fails on a cluster (the API server can't reach pods), stop and report immediately. The whole design depends on it.

---

## 3. Checklist

Run Rancher checks against each site's `local` cluster, and cluster checks against one test cluster per site (then sample more distros and network types). Commands are examples; adapt them to what is installed.

Priority order: **E14 → R1 → R4/E18 → everything else.**

### A. Rancher (each of the 3 sites)

- ☐ **E1 Version.** `kubectl get settings.management.cattle.io server-version -o jsonpath='{.value}'`. Record the exact patch version. Flag any v2.13 site below v2.13.11 (authorization CVEs fixed there).
- ☐ **E2 Session TTLs.** `kubectl get settings.management.cattle.io auth-user-session-ttl-minutes auth-user-session-idle-ttl-minutes`. Record the values and flag any difference between sites.
- ☐ **E3 Proxies in front of Rancher.** `kubectl -n cattle-system get ingress rancher -o yaml`. Record `proxy-read-timeout`, `proxy-send-timeout` and `proxy-buffering`. Then find any external load balancer or reverse proxy in front of it, and record its idle timeout and buffering settings. This is often outside Kubernetes, so ask the owning team if needed.
- ☐ **E4 Login.** Test account only, one attempt:
  `curl --cacert <ca> -X POST https://<site>/v1-public/login -H 'Content-Type: application/json' -d @login.json`
  Record the response shape (field names only, redact values). Check whether `expiresAt` reflects the `ttl` you sent.
- ☐ **E5 Feature flags.** `kubectl get features.management.cattle.io v3-public ext-tokens -o custom-columns=NAME:.metadata.name,VALUE:.spec.value,DEFAULT:.status.default`
- ☐ **E6 Logout.** `POST /v3/tokens?action=logout` with the E4 token. Then confirm `GET /v3/tokens/<id>` returns 401 or 404.
- ☐ **E7 Cluster list for a normal user.** `GET /v1/management.cattle.io.clusters` with a non-admin token returns only that user's clusters. Confirm these fields exist: `spec.displayName`, and `status.conditions` of type `Connected` / `Ready` (reason `Disconnected` when down).
- ☐ **E8 Principal IDs match across sites.** For the same person on every site:
  `kubectl get users.management.cattle.io -o jsonpath='{range .items[*]}{.principalIds}{"\n"}{end}' | grep -i <name>`
  The `activedirectory_user://…` value must be byte-identical across sites, including DN case and spacing. Also note any GUID-based IDs. Report a few sample DNs (redacted if needed) showing which special characters occur (`,` `=` space `+`).
- ☐ **E9 Default roles.** `kubectl get roletemplate cluster-member -o yaml`: confirm it has no `services/proxy`. List who holds cluster-owner today (counts only, or role names).
- ☐ **E10 Rate limits.** Does Rancher, or the load balancer in front of it, rate-limit API calls per user or per source IP? Record the limits.
- ☐ **E11 AD lockout policy.** Record the lockout threshold and observation window, and which other systems also count failed logins against it.
- ☐ **E20 Site 1 coverage.** Can every intended user authenticate on site 1? (The login flow tries site 1 first.)

### B. Downstream clusters (one test cluster per site, then sample)

- ☐ **E12 kagent Helm values.** From the Fleet bundle or `helm -n kagent get values kagent`. Record `controller.auth.mode` (expect `unsecure`), `controller.streaming.timeout` (default 600s), the release name (it sets the service name `<release>-controller`), and any OTEL or tracing settings.
- ☐ **E13 Service and port.** `kubectl -n kagent get svc kagent-controller -o jsonpath='{.spec.ports}'` includes 8083.
- ☐ **E14 API server can reach pods (critical).** As an admin:
  `kubectl get --raw '/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/<ns>/<agent>/.well-known/agent-card.json'`
  A 503 "no endpoints available" or a dial timeout means the control plane can't route to pod IPs, so the design fails on that cluster. Repeat on every Kubernetes distribution and network type in use.
- ☐ **E15 Agent ServiceAccount rights.** `kubectl auth can-i --list --as=system:serviceaccount:<ns>:<agent-sa>`. Record especially `secrets`, `configmaps` in system namespaces, `pods/exec` and `pods/log`.
- ☐ **E16 Only one matching service.** `kubectl get svc -A | grep kagent-controller` shows exactly one.
- ☐ **E17 `local` clusters.** Is kagent installed on any site's `local` cluster?
- ☐ **E18 Through Rancher as a normal user.** Needs the test role (approval required, see ground rules):
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
      resourceNames: ["http:kagent-controller:8083"]
      verbs: ["get", "create"]
  ```
  Bind it to a test user on the test cluster only. Repeat E14 through `https://<site>/k8s/clusters/<id>/…` with that user's token and expect 200. Remove the binding and expect 403.
- ☐ **E19 Outbound calls from kagent.** Check controller and agent pod logs for failing or hanging connections to internet hosts (telemetry, tracing exporters).

### C. Chat path through Rancher (Phase 0 research)

- ☐ **R1 Streaming and timeouts (highest priority).** On each site, through Rancher with a test user token:
  1. Send `message/stream`. Use `curl -N` so output isn't buffered. Record time to first event and the gaps between events. Events must arrive one by one, not all at the end.
  2. Run a deliberately long investigation (well over 10 minutes if possible, including long silent tool calls). Record **which** limit cuts it off and when: ingress (E3), external load balancer idle timeout, or the kagent 600s cap (E12).
  3. Test the fallback: `message/send` with `"configuration":{"blocking":false}`, then poll `tasks/get`. Does v0.9.6 return immediately with a task ID? Does polling still work after 600s?
  4. Record the typical and worst-case duration of real investigations against the internal LLM endpoint.
- ☐ **R2 Multi-turn.** Send 3 messages with the same `contextId`. Expect success; check agent pod logs for `SessionNotFoundError`. Repeat with a user ID shaped like a real DN containing `,` `=` space and `+`, and record the result for each.
- ☐ **R4 RBAC name.** Through Rancher as the E18 test user, send a SelfSubjectAccessReview to `/k8s/clusters/<id>/apis/authorization.k8s.io/v1/selfsubjectaccessreviews` with `resource=services, subresource=proxy, namespace=kagent, name=http:kagent-controller:8083, verb=create`. Expect `allowed: true`. Repeat with `name=kagent-controller` and record that result too.
- ☐ **R5 Headers.** With controller debug logging, confirm `X-User-Id` and `Content-Type` arrive unchanged and `Authorization` does not. Send both `?user_id=A` and `X-User-Id: B`, and confirm which one kagent uses.

### D. Portal hosting environment

- ☐ **H1 Registry.** Record the internal registry path that Fleet and the portal's host cluster pull from, and whether Redis/Valkey images are already mirrored there.
- ☐ **H2 Reachability.** From the portal's host cluster, confirm DNS resolves all three Rancher hostnames and HTTPS connects to each.
- ☐ **H3 TLS.** For each Rancher hostname, collect the full CA chain and confirm the hostname is in the certificate SANs. Ask how and when the certificates rotate.
- ☐ **H4 Egress policy inputs.** Record the IPs/CIDRs (and load balancer VIPs) of each Rancher site. Does the host cluster's CNI support FQDN-based network policies?
- ☐ **H5 Time.** Confirm NTP is running on the portal's host cluster and on all three Rancher sites.
- ☐ **H6 Terraform.** Is there an internal Terraform provider mirror with `rancher/rancher2`? If not, record that.

---

## 4. Reporting results

Mark each item ☐ → ☑ (or ☒ if it failed) in this file, and add an indented result line with the date, site and finding. Keep raw output in a separate notes file if it's long, with secrets redacted.

```
- ☑ **E13 Service and port.** …
  - 2026-11-20, site 2 (cluster `c-abc12`, RKE2): port 8083 present.
```

For every result, state:

- **Site and cluster** (ID plus distro or network type for cluster checks).
- **Expected vs observed.** Quote status codes and error text exactly.
- **Confidence.** Was it observed directly, inferred, or reported by someone else (and who, by role)?
- **Contradictions.** Anything that disagrees with section 1, flagged at the top of your report.
