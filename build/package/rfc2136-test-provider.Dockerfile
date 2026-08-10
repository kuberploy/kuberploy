# syntax=docker/dockerfile:1.7
FROM docker.io/library/golang:1.26.5-alpine3.24@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
COPY --from=build --chown=65532:65532 /out/provider /provider
USER 65532:65532
EXPOSE 5353/udp
EXPOSE 5353/tcp
ENTRYPOINT ["/provider"]
