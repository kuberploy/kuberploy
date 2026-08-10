CREATE TABLE IF NOT EXISTS service_accounts (
    id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100 AND name=btrim(name) AND name !~ '[[:cntrl:]]'),
    role text NOT NULL CHECK (role IN ('viewer','developer','project-admin')),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz,
    CHECK (disabled_at IS NULL OR disabled_at >= created_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS service_accounts_project_name_idx
    ON service_accounts (project_id,lower(name));
CREATE INDEX IF NOT EXISTS service_accounts_project_created_idx
    ON service_accounts (project_id,created_at,id);

CREATE TABLE IF NOT EXISTS service_account_tokens (
    id uuid PRIMARY KEY,
    service_account_id uuid NOT NULL REFERENCES service_accounts(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100 AND name=btrim(name) AND name !~ '[[:cntrl:]]'),
    token_prefix text NOT NULL CHECK (token_prefix ~ '^kp_sa_[A-Za-z0-9_-]{8}$'),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash)=32),
    scopes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (cardinality(scopes) BETWEEN 1 AND 4),
    CHECK (scopes <@ ARRAY['app.read','app.edit','build.create','logs.read']::text[]),
    CHECK (array_position(scopes,NULL) IS NULL),
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '90 days'),
    CHECK (last_used_at IS NULL OR last_used_at >= created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);
CREATE INDEX IF NOT EXISTS service_account_tokens_account_created_idx
    ON service_account_tokens (service_account_id,created_at,id);
CREATE INDEX IF NOT EXISTS service_account_tokens_active_expiry_idx
    ON service_account_tokens (expires_at) WHERE revoked_at IS NULL;

ALTER TABLE access_grants
    DROP CONSTRAINT IF EXISTS access_grants_source_check;
ALTER TABLE access_grants
    ADD CONSTRAINT access_grants_source_check
    CHECK (source IN ('explicit','bootstrap','service-account'));
