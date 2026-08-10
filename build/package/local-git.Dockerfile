# syntax=docker/dockerfile:1.7
# OrbStack integration-only Git daemon. It is deliberately not part of the
# production Kuberploy control plane or release chart.
FROM docker.io/library/alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

RUN apk add --no-cache \
      ca-certificates=20260611-r0 \
      git=2.54.0-r0 \
      git-daemon=2.54.0-r0 \
    && addgroup -S -g 10001 git \
    && adduser -S -D -H -u 10001 -G git git \
    && test -x "$(git --exec-path)/git-daemon"

LABEL org.opencontainers.image.title="Kuberploy local Git test server" \
      org.opencontainers.image.description="Run-scoped Git daemon for OrbStack integration tests only" \
      org.opencontainers.image.source="https://github.com/kuberploy/kuberploy" \
      org.opencontainers.image.licenses="Apache-2.0"

USER 10001:10001
ENTRYPOINT ["/usr/bin/git"]
