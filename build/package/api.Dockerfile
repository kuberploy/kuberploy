# syntax=docker/dockerfile:1.7
FROM docker.io/library/golang:1.26.5-alpine3.24 AS build

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
      -o /out/kuberploy-api ./cmd/kuberploy-api
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags="-s -w -buildid=" \
      -o /out/kuberploy-bootstrap-token ./cmd/kuberploy-bootstrap-token

FROM docker.io/alpine/helm:4.2.3 AS helm-runtime

FROM docker.io/library/alpine:3.24.1

RUN apk add --no-cache ca-certificates=20260611-r0

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="Kuberploy API" \
      org.opencontainers.image.description="Kuberploy Kubernetes-native PaaS control-plane API" \
      org.opencontainers.image.source="https://github.com/kuberploy/kuberploy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build --chown=65532:65532 /out/kuberploy-api /kuberploy-api
COPY --from=build --chown=65532:65532 /out/kuberploy-bootstrap-token /kuberploy-bootstrap-token
COPY --from=helm-runtime --chown=65532:65532 /usr/bin/helm /usr/local/bin/helm
COPY --chown=65532:65532 charts/kuberploy-runtime /opt/kuberploy/charts/kuberploy-runtime
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/kuberploy-api"]
