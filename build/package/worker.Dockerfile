# syntax=docker/dockerfile:1.7
FROM docker.io/library/golang:1.26.5-alpine3.24@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
COPY schema ./schema

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/kuberploy-worker ./cmd/kuberploy-worker && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags="-s -w -buildid=" \
      -o /out/kuberploy-upgrade-runner ./cmd/kuberploy-upgrade-runner && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags="-s -w -buildid=" \
      -o /out/kuberploy-git-askpass ./cmd/kuberploy-git-askpass

# The exact Distribution binary is used only by the stopped-registry helper
# Job. It is never invoked by the long-running worker process.
FROM docker.io/library/registry:3.1.1@sha256:1be55279f18a2fe1a74edf2664cac61c1bea305b7b4642dab412e7affdcb3e33 AS registry-cli

FROM cgr.dev/chainguard/git@sha256:9e0818dd94a49dbe025951b02ab90603ba5aa3dbf2b2a300cfac3d84121b5ccc

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="Kuberploy Worker" \
      org.opencontainers.image.description="Kuberploy durable GitOps and build worker" \
      org.opencontainers.image.source="https://github.com/kuberploy/kuberploy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build --chown=65532:65532 /out/kuberploy-worker /kuberploy-worker
COPY --from=build --chown=65532:65532 /out/kuberploy-upgrade-runner /usr/local/bin/kuberploy-upgrade-runner
COPY --from=build --chown=65532:65532 /out/kuberploy-git-askpass /usr/local/bin/kuberploy-git-askpass
COPY --from=registry-cli --chown=65532:65532 /bin/registry /usr/local/bin/registry
USER 65532:65532
ENTRYPOINT ["/kuberploy-worker"]
