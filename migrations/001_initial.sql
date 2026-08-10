CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bootstrap_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    consumed_at timestamptz
);
INSERT INTO bootstrap_state (singleton) VALUES (true) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS users (
    id uuid PRIMARY KEY,
    login text NOT NULL,
    role text NOT NULL CHECK (role IN ('platform-admin')),
    issuer text NOT NULL,
    subject text NOT NULL,
    grant_revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash bytea PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    grant_revision bigint NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_expires_idx ON sessions (expires_at);

CREATE TABLE IF NOT EXISTS projects (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    slug text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS environments (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    name text NOT NULL,
    slug text NOT NULL,
    namespace text NOT NULL UNIQUE,
    argo_project text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug)
);

CREATE TABLE IF NOT EXISTS applications (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    name text NOT NULL,
    slug text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug)
);

CREATE TABLE IF NOT EXISTS operations (
    id uuid PRIMARY KEY,
    kind text NOT NULL,
    status text NOT NULL CHECK (status IN ('queued','running','succeeded','failed','cancelled','superseded')),
    target_type text NOT NULL,
    target_id uuid NOT NULL,
    request_id text NOT NULL,
    generation bigint NOT NULL DEFAULT 1,
    progress jsonb NOT NULL DEFAULT '[]'::jsonb,
    git_revision text NOT NULL DEFAULT '',
    problem jsonb,
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX IF NOT EXISTS operations_status_lease_idx ON operations (status, lease_until, created_at);

CREATE TABLE IF NOT EXISTS deployments (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    image text NOT NULL,
    replicas integer NOT NULL CHECK (replicas BETWEEN 1 AND 100),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    environment jsonb NOT NULL DEFAULT '{}'::jsonb,
    route jsonb,
    state text NOT NULL,
    operation_id uuid NOT NULL UNIQUE REFERENCES operations(id) DEFERRABLE INITIALLY DEFERRED,
    desired_revision text NOT NULL DEFAULT '',
    observed_revision text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (environment_id, application_id)
);

CREATE TABLE IF NOT EXISTS outbox (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    kind text NOT NULL,
    scope_id uuid NOT NULL,
    generation bigint NOT NULL,
    trace_id text NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    last_error text,
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox (available_at, created_at) WHERE published_at IS NULL;

CREATE TABLE IF NOT EXISTS idempotency_keys (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope text NOT NULL,
    key text NOT NULL,
    fingerprint text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid NOT NULL,
    operation_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id, scope, key)
);

CREATE TABLE IF NOT EXISTS audit_events (
    id uuid PRIMARY KEY,
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action text NOT NULL,
    target_type text NOT NULL,
    target_id uuid NOT NULL,
    request_id text NOT NULL,
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_target_idx ON audit_events (target_type, target_id, created_at);
