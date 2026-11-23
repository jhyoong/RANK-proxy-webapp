# RANK-proxy-webapp

RANcher Kagent webapp.
Chat portal for kagent across Rancher-managed clusters. See [docs/design.md](docs/design.md) and [docs/verification.md](docs/verification.md).

## Build locally

```sh
go test -race ./...
node --test internal/web/jstest/   # Markdown renderer tests (needs Node)
go build ./cmd/portal
docker build -t portal:local --build-arg VERSION=dev .
```

## Config file

The portal reads one YAML file: `-config <path>`, else `$PORTAL_CONFIG`, else `/etc/portal/config.yaml`. See [config.example.yaml](config.example.yaml) for every key. Sites are tried in the order listed; the first site is "site 1" in the login flow.

## Run locally with the fake Rancher

The login flow, cluster list and kagent access check can be clicked through without a real Rancher:

```sh
go run ./cmd/fakerancher                      # 3 fake sites on 127.0.0.1:9441-9443 (40 clusters each), writes dev/config.yaml
go run ./cmd/portal -config dev/config.yaml   # then open http://127.0.0.1:8080
```

Test users: `admin1`/`pass1` (all sites ok), `drift`/`pass1` (site-2 rejects the login; `/home` says "site-2 didn't accept your sign-in"), `nopr`/`pass1` (logs in normally; the portal logs a `no_principal` warning), `limited`/`pass1` (logs in everywhere, but `no-access` on every `prod` cluster). A wrong password should show only site-1 hit in the fakerancher log.

Pages:

- `/home`: the cluster list across sites (about 120 rows; each site's `local` cluster is hidden unless `clusters.include_local: true`). Filter with `?q=prod`; the Refresh button reloads the list from Rancher (the list is otherwise cached for `clusters.cache_ttl`). Disconnected clusters are greyed out.
- `/clusters/{site}/{id}`: runs the kagent check for one cluster. A `ready` cluster shows the chat; any other outcome (`no-access`, `not-installed`, `not-running`, `unreachable`, ...) shows the "kagent isn't available" card with a `Reference: <outcome> · <cluster name>` line. In the fake, each site has one cluster whose agent card returns 404 (`not-installed`), one 503 (`not-running`) and one that hangs (`unreachable` after the 5s API timeout).
- Conversations sidebar (on a `ready` cluster page): lists past conversations read from kagent (through Rancher, newest first, up to 50). `?c=<uuid>` opens one and resumes it; the transcript is also served as JSON at `GET /clusters/{site}/{id}/conversations/{cid}`. History is shared per cluster: the portal sends no user id to kagent, so everyone with access to the cluster sees the same list. A conversation still running shows as "working". Session expiry ends the whole portal session. In the fake, each site's first ready cluster (`c-<site>-prod-01`) is seeded with two conversations (one completed, one still "working").
- Chat panel (on a `ready` cluster page): sends your message to the kagent agent through Rancher and streams the reply (`POST /clusters/{site}/{id}/chat`). One reply at a time per session; a second tab gets "A reply is already running in another tab". In the fake, ready clusters stream a scripted reply at 300ms per event (follow-ups say `turn 2`). Per site, these clusters misbehave in chat: `<site>-stg-20` is silent for 20s before answering (heartbeat pings keep the browser connection alive), `<site>-prod-22` returns a JSON-RPC error, `<site>-stg-23` ends with `state: failed`, `<site>-prod-25` never answers (stops after the 30s `chat.idle_timeout` set in the fake config), `<site>-stg-26` is cut off after `working`. Chat settings are in the `chat:` block of the config; set `chat.log_events: true` to log the shape of each upstream event (never message or agent text).

An ingress in front of the portal must disable response buffering (SSE; for ingress-nginx, `proxy-buffering: "off"`) and set a read timeout above `chat.heartbeat` (default 15s): once the reply stream is open the portal sends a ping every heartbeat. To be safe, set it above `chat.idle_timeout` (default 10m), because no pings flow while the portal waits for Rancher's response headers. On the Rancher leg, the Rancher ingress read timeout (`nginx.ingress.kubernetes.io/proxy-read-timeout`) should be at least `chat.idle_timeout`; otherwise a silent tool call ends as a `cut-off` instead of an `idle-timeout`. `chat.max_duration` (default 30m) caps the whole reply.

The `ui:` block customises what users see: `banner` (with `banner_level` `info`|`warning`) and `login_help` appear on the login page only; `support_email` appears on the login page, in the user menu and on the unavailable screen; see [config.example.yaml](config.example.yaml). The former `/diagnostics` data is now in the logs: `cluster_check` (`ssar_reason`, `ssar_http_code`, `error`, `card_*`), `clusters_list` (`not_connected`) and a `no_principal` warning.

kagent must run in its default `unsecure` auth mode (the portal sends no user id). Adding or removing a site needs a config edit, a NetworkPolicy egress entry and a restart.

With Docker, mount the config and CA files:

```sh
docker run --rm -p 8080:8080 -v $PWD/my-config:/etc/portal:ro portal:local
```

For the on-site live test (the `kagent`, `clusters` and `api` blocks in the ConfigMap are the ones to adjust), see [docs/plans/PLAN1-live-test.md](docs/plans/PLAN1-live-test.md), [docs/plans/PLAN2-live-test.md](docs/plans/PLAN2-live-test.md), [docs/plans/PLAN5-live-test.md](docs/plans/PLAN5-live-test.md) and [deploy/portal.yaml](deploy/portal.yaml).

## Security

- **Login limits** (`login:` block): `max_attempts_per_user: 3`, `max_attempts_per_ip: 20`, `attempt_window: 15m`. Each counted attempt restarts that key's window, so set `attempt_window` at least as long as AD's lockout observation window. A blocked login never reaches Rancher. Keep `max_attempts_per_user` below the AD lockout threshold minus attempts made in the Rancher UI directly.
- **`login.trusted_proxies`**: CIDRs of the ingress controller's pods. Behind an ingress, `RemoteAddr` is the ingress pod, so without this every user shares one IP bucket. A peer in these CIDRs has its `X-Forwarded-For` trusted for the real client IP; from any other peer it is ignored.
- **`session.max_per_user: 5`**: a 6th login ends the user's oldest session and revokes its Rancher tokens.
- **Token revocation**: Rancher tokens are revoked when a session expires (swept every minute), is evicted, on re-login, and on pod shutdown (a 10s pass after the HTTP server stops; the Deployment sets `terminationGracePeriodSeconds: 30`).
- **Browser protections**: cookie is `HttpOnly`, `SameSite=Strict`, and `Secure` when `cookie_secure` is true; non-safe requests pass through `http.CrossOriginProtection`; HSTS is sent when `cookie_secure` is true.
- **NetworkPolicy** in [deploy/portal.yaml](deploy/portal.yaml): ingress only on TCP 8080 from `<ingress-controller-namespace>`; egress only TCP 443 to `<rancher-site-1-cidr>`, `<rancher-site-2-cidr>`, `<rancher-site-3-cidr>`, and DNS (UDP/TCP 53) to `kube-system` pods labelled `k8s-app: kube-dns`. Replace the placeholders. `kubectl port-forward` bypasses NetworkPolicy.
- **TLS**: the ingress must terminate TLS. HSTS and `Secure` cookies assume HTTPS.
- **Supply chain**: CI runs `govulncheck` and a Trivy scan; the Dockerfile pins the distroless base by digest.

## Image pipeline

`.github/workflows/image.yml` runs `go vet`, `go test` and `govulncheck`, builds a `linux/amd64` image, scans it with Trivy (fails on fixable HIGH/CRITICAL), then pushes.

| Trigger | Pushed tags |
| --- | --- |
| Pull request | nothing (build + scan only) |
| Push to `main` | `main`, `sha-<short>` |
| Tag `vX.Y.Z` | `X.Y.Z`, `X.Y`, `latest`, `sha-<short>` |

Images go to `ghcr.io/jhyoong/rank-proxy-webapp` using the built-in `GITHUB_TOKEN`; no secrets needed.

To also push to Docker Hub, set in the repo settings: variables `DOCKERHUB_USERNAME` and `DOCKERHUB_IMAGE` (e.g. `youruser/rank-proxy-webapp`), and secret `DOCKERHUB_TOKEN` (a Docker Hub access token).

## Moving an image into the air-gapped network

```sh
# outside
skopeo copy docker://ghcr.io/jhyoong/rank-proxy-webapp:X.Y.Z docker-archive:portal-X.Y.Z.tar
# inside
skopeo copy docker-archive:portal-X.Y.Z.tar docker://<internal-registry>/rank-proxy-webapp:X.Y.Z
```

Deploy by digest. If the GHCR package is private, run `skopeo login ghcr.io` first.
