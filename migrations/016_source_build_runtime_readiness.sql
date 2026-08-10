-- A fresh worker observation is required before the public source-build API
-- reports operational readiness. The digest binds all operator-owned runtime
-- settings; no credential material or provider token is stored here.

CREATE TABLE IF NOT EXISTS source_build_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    github_app_id bigint NOT NULL CHECK (github_app_id > 0),
    builder_namespace text NOT NULL CHECK (
        builder_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    builder_agent_image text NOT NULL CHECK (
        length(builder_agent_image) BETWEEN 80 AND 512 AND
        builder_agent_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
    ),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    CHECK (observed_at >= started_at)
);

CREATE INDEX IF NOT EXISTS source_build_runtime_readiness_match_idx
    ON source_build_runtime_readiness(config_digest,observed_at DESC);
