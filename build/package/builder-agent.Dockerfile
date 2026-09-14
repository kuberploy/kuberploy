# syntax=docker/dockerfile:1.7
FROM docker.io/library/golang:1.27-alpine3.24 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd/kuberploy-build-agent ./cmd/kuberploy-build-agent
COPY internal/builder ./internal/builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags="-s -w -buildid=" \
      -o /out/kuberploy-build-agent ./cmd/kuberploy-build-agent

FROM docker.io/docker/buildx-bin:0.36.1 AS buildx

FROM docker.io/library/docker:29-dind AS docker-cli

FROM docker.io/library/alpine:3.24

RUN apk add --no-cache \
      ca-certificates=20260611-r0 \
      git=2.54.0-r0

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="Kuberploy Build Agent" \
      org.opencontainers.image.description="Trusted Kuberploy Buildx agent and checkout runtime" \
      org.opencontainers.image.source="https://github.com/kuberploy/kuberploy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      io.kuberploy.builder.docker="29.7.1" \
      io.kuberploy.builder.buildx="0.36.1" \
      io.kuberploy.builder.buildkit="0.32.2"

COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=buildx /buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --from=build --chown=65532:65532 /out/kuberploy-build-agent /usr/local/bin/kuberploy-build-agent

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kuberploy-build-agent"]
