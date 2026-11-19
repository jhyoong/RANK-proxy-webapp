# RANK-proxy-webapp

Chat portal for kagent across Rancher-managed clusters. See [docs/design.md](docs/design.md) and [docs/verification.md](docs/verification.md).

## Build locally

```sh
go build ./cmd/portal
docker build -t portal:local --build-arg VERSION=dev .
docker run --rm -p 8080:8080 portal:local   # GET /healthz, /version
```

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
