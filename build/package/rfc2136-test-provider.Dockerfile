# syntax=docker/dockerfile:1.7
FROM docker.io/library/golang:1.26.5-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/kuberploy-rfc2136-test-provider ./cmd/kuberploy-rfc2136-test-provider
COPY internal/rfc2136test ./internal/rfc2136test
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/provider ./cmd/kuberploy-rfc2136-test-provider

FROM docker.io/library/alpine:3.24.1
COPY --from=build --chown=65532:65532 /out/provider /provider
USER 65532:65532
EXPOSE 5353/udp
EXPOSE 5353/tcp
ENTRYPOINT ["/provider"]
