# syntax=docker/dockerfile:1.7
FROM docker.io/alpine/helm:4.2.3

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="Kuberploy Upgrader" \
      org.opencontainers.image.description="Pinned Helm runtime for namespace-scoped Kuberploy control-plane upgrades" \
      org.opencontainers.image.source="https://github.com/kuberploy/kuberploy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}"

# Keep Helm as the only entrypoint: the worker supplies a validated,
# digest-pinned chart package and the exact namespace-scoped upgrade arguments
# recorded in the Operation. HOME lives on the pod's writable /tmp volume.
ENV HOME=/tmp/helm
WORKDIR /tmp
USER 65532:65532
ENTRYPOINT ["helm"]
