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

Plan 1 (login flow only) can be clicked through without a real Rancher:

```sh
go run ./cmd/fakerancher                      # 3 fake sites on 127.0.0.1:9441-9443, writes dev/config.yaml
go run ./cmd/portal -config dev/config.yaml   # then open http://127.0.0.1:8080
```

Test users: `admin1`/`pass1` (all sites ok), `drift`/`pass1` (site-2 rejects: drift warning), `nopr`/`pass1` (no AD principal warning). A wrong password should show only site-1 hit in the fakerancher log.

With Docker, mount the config and CA files:

```sh
docker run --rm -p 8080:8080 -v $PWD/my-config:/etc/portal:ro portal:local
```

For the on-site live test, see [docs/plans/PLAN1-live-test.md](docs/plans/PLAN1-live-test.md) and [deploy/portal.yaml](deploy/portal.yaml).

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
