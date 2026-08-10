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
      -o /out/kuberploy-api ./cmd/kuberploy-api
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags="-s -w -buildid=" \
      -o /out/kuberploy-bootstrap-token ./cmd/kuberploy-bootstrap-token

FROM docker.io/alpine/helm:4.2.3@sha256:b97ba4f9b27fe7af16ee3d37e6815783c9d4a51289b6240a9024ec471611ae9b AS helm-runtime

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

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
