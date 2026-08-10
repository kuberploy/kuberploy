-- Durable, optimistic-concurrency-controlled AppConfig editing. Git/network
-- work remains behind the existing operation outbox.
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS config_raw bytea,
    ADD COLUMN IF NOT EXISTS config_etag text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS config_version bigint NOT NULL DEFAULT 0;

-- Existing pre-007 deployments intentionally remain an incomplete projection
-- (NULL/empty/0). Reads fail closed until an explicit repair or a newly
-- accepted release writes an exact server-rendered AppConfig; GET never repairs.

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_config_projection_complete;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_config_projection_complete CHECK (
        (config_raw IS NULL AND config_etag = '' AND config_version = 0)
        OR
        (octet_length(config_raw) BETWEEN 1 AND 262144 AND config_etag <> '' AND config_version > 0)
    );

ALTER TABLE deployment_operation_inputs
    ADD COLUMN IF NOT EXISTS config_raw bytea;
ALTER TABLE deployment_operation_inputs
    DROP CONSTRAINT IF EXISTS deployment_operation_inputs_config_size;
ALTER TABLE deployment_operation_inputs
    ADD CONSTRAINT deployment_operation_inputs_config_size CHECK (
        config_raw IS NULL OR octet_length(config_raw) BETWEEN 1 AND 262144
    );

CREATE TABLE IF NOT EXISTS deployment_config_previews (
    id uuid PRIMARY KEY,
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    base_etag text NOT NULL,
    candidate_hash bytea NOT NULL CHECK (octet_length(candidate_hash) = 32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deployment_config_previews_lookup_idx
    ON deployment_config_previews (deployment_id, actor_id, expires_at DESC);
CREATE INDEX IF NOT EXISTS deployment_config_previews_expiry_idx
    ON deployment_config_previews (expires_at) WHERE consumed_at IS NULL;
