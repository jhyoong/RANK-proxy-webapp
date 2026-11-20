# RANK-proxy-webapp

RANcher Kagent webapp.
Chat portal for kagent across Rancher-managed clusters. See [docs/design.md](docs/design.md) and [docs/verification.md](docs/verification.md).

## Build locally

```sh
go test -race ./...
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

Test users: `admin1`/`pass1` (all sites ok), `drift`/`pass1` (site-2 rejects: drift warning), `nopr`/`pass1` (no AD principal warning), `limited`/`pass1` (logs in everywhere, but `no-access` on every `prod` cluster). A wrong password should show only site-1 hit in the fakerancher log.

Pages:

- `/home`: the cluster list across sites (about 120 rows; each site's `local` cluster is hidden). Filter with `?q=prod`; the Refresh button reloads the list from Rancher (the list is otherwise cached for `clusters.cache_ttl`). Disconnected clusters are greyed out.
- `/clusters/{site}/{id}`: runs the kagent check for one cluster and shows the result (`ready`, `no-access`, `not-installed`, `not-running`, `unreachable`, ...). In the fake, each site has one cluster whose agent card returns 404 (`not-installed`), one 503 (`not-running`) and one that hangs (`unreachable` after the 5s API timeout).
- `/diagnostics`: the per-site login table plus the raw cluster list with states and conditions.

With Docker, mount the config and CA files:

```sh
docker run --rm -p 8080:8080 -v $PWD/my-config:/etc/portal:ro portal:local
```

For the on-site live test (the `kagent`, `clusters` and `api` blocks in the ConfigMap are the ones to adjust), see [docs/plans/PLAN1-live-test.md](docs/plans/PLAN1-live-test.md), [docs/plans/PLAN2-live-test.md](docs/plans/PLAN2-live-test.md) and [deploy/portal.yaml](deploy/portal.yaml).

## Image pipeline

`.github/workflows/image.yml` runs `go vet` and `go test`, builds a `linux/amd64` image, scans it with Trivy (fails on fixable HIGH/CRITICAL), then pushes.

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
