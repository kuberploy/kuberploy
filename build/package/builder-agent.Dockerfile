# syntax=docker/dockerfile:1.7
FROM docker.io/library/golang:1.26.5-alpine3.24 AS build

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

FROM docker.io/library/alpine:3.24.1 AS buildx

ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) checksum='48af8a397ebd60178778bf63611dbcebe5f5e7a9be90eb9147b24b9587455778' ;; \
      arm64) checksum='5d0cafd9d16afe1a0f0d9529885344ace2cc99efdd531b6c783c5455a6001569' ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    wget -q -O /docker-buildx "https://github.com/docker/buildx/releases/download/v0.36.1/buildx-v0.36.1.linux-${TARGETARCH}"; \
    echo "${checksum}  /docker-buildx" | sha256sum -c -; \
    chmod 0755 /docker-buildx

FROM docker.io/library/docker:29.7.1-dind AS docker-cli

FROM docker.io/library/alpine:3.24.1

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
COPY --from=buildx /docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --from=build --chown=65532:65532 /out/kuberploy-build-agent /usr/local/bin/kuberploy-build-agent

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/kuberploy-build-agent"]
