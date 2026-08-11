# syntax=docker/dockerfile:1.7
FROM docker.io/library/node:26-alpine

WORKDIR /opt/kuberploy/migrations

COPY migrations/package.json migrations/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund && \
    npm cache clean --force

COPY migrations/prisma.config.ts ./prisma.config.ts
COPY migrations/check-schema-drift.mjs ./check-schema-drift.mjs
COPY migrations/run.mjs ./run.mjs
COPY migrations/prisma ./prisma

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="Kuberploy Database Migration" \
      org.opencontainers.image.description="Prisma migration-only runtime for Kuberploy PostgreSQL" \
      org.opencontainers.image.source="https://github.com/kuberploy/kuberploy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      io.kuberploy.database.migration-engine="prisma-7.9.1"

ENV HOME=/tmp/prisma \
    XDG_CACHE_HOME=/tmp/prisma/.cache

USER 65532:65532
ENTRYPOINT ["node", "run.mjs"]
