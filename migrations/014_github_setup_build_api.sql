-- Public GitHub App setup and build API coordination. OAuth/user tokens,
-- webhook secrets, App keys, registry credentials and raw webhook bodies are
-- deliberately absent. Setup handoffs retain only provider identities and a
-- domain-separated digest of the one-time browser token.

CREATE TABLE IF NOT EXISTS github_user_bindings (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    github_user_id bigint NOT NULL UNIQUE CHECK (github_user_id > 0),
    github_login text NOT NULL,
    bound_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (github_login ~ '^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$')
);

CREATE TABLE IF NOT EXISTS github_setup_authorizations (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    idempotency_key text NOT NULL,
    request_fingerprint text NOT NULL CHECK (request_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    state_value text NOT NULL CHECK (length(state_value) BETWEEN 64 AND 4096),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id,idempotency_key),
    CHECK (length(idempotency_key) BETWEEN 16 AND 128)
);
CREATE INDEX IF NOT EXISTS github_setup_authorizations_expiry_idx
    ON github_setup_authorizations(expires_at);

CREATE TABLE IF NOT EXISTS github_setup_handoffs (
    digest bytea PRIMARY KEY CHECK (octet_length(digest)=32),
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    github_user_id bigint NOT NULL CHECK (github_user_id > 0),
    github_user_login text NOT NULL,
    installation jsonb NOT NULL CHECK (jsonb_typeof(installation)='object'),
    repositories jsonb NOT NULL CHECK (jsonb_typeof(repositories)='array'),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    link_idempotency_key text,
    link_request_fingerprint text,
    linked_installation_id uuid REFERENCES github_installations(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((consumed_at IS NULL AND link_idempotency_key IS NULL AND link_request_fingerprint IS NULL AND linked_installation_id IS NULL)
        OR (consumed_at IS NOT NULL AND length(link_idempotency_key) BETWEEN 16 AND 128
            AND link_request_fingerprint ~ '^sha256:[0-9a-f]{64}$'))
);
CREATE INDEX IF NOT EXISTS github_setup_handoffs_actor_idx
    ON github_setup_handoffs(actor_id,expires_at);

CREATE TABLE IF NOT EXISTS build_api_idempotency (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operation text NOT NULL CHECK (operation IN ('definition.create','attempt.cancel','attempt.retry')),
    scope_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    request_fingerprint text NOT NULL CHECK (request_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    resource_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id,operation,scope_id,idempotency_key),
    CHECK (length(idempotency_key) BETWEEN 16 AND 128)
);
