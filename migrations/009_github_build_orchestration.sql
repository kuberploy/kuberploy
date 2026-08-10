-- Durable GitHub App and source-build orchestration. Provider credentials are
-- deliberately absent: only immutable provider identities and Kubernetes
-- Secret references may be stored.
ALTER TABLE github_installations
    ADD COLUMN IF NOT EXISTS github_app_id bigint,
    ADD COLUMN IF NOT EXISTS github_account_id bigint,
    ADD COLUMN IF NOT EXISTS lifecycle text NOT NULL DEFAULT 'pending-verification',
    ADD COLUMN IF NOT EXISTS permissions jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS suspended_at timestamptz,
    ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_verified_at timestamptz;

ALTER TABLE github_installations
    DROP CONSTRAINT IF EXISTS github_installations_lifecycle_check;
ALTER TABLE github_installations
    ADD CONSTRAINT github_installations_lifecycle_check
        CHECK (lifecycle IN ('pending-verification','active','suspended','deleted')),
    ADD CONSTRAINT github_installations_app_id_check
        CHECK (github_app_id IS NULL OR github_app_id > 0),
    ADD CONSTRAINT github_installations_account_id_check
        CHECK (github_account_id IS NULL OR github_account_id > 0),
    ADD CONSTRAINT github_installations_permissions_object_check
        CHECK (jsonb_typeof(permissions)='object'),
    ADD CONSTRAINT github_installations_verified_identity_check
        CHECK ((lifecycle='pending-verification') OR
            (github_app_id IS NOT NULL AND github_account_id IS NOT NULL AND last_verified_at IS NOT NULL)),
    ADD CONSTRAINT github_installations_lifecycle_timestamp_check
        CHECK ((lifecycle='suspended' AND suspended_at IS NOT NULL AND deleted_at IS NULL)
            OR (lifecycle='deleted' AND deleted_at IS NOT NULL)
            OR (lifecycle IN ('pending-verification','active') AND suspended_at IS NULL AND deleted_at IS NULL));

CREATE INDEX IF NOT EXISTS github_installations_provider_idx
    ON github_installations(github_app_id,github_installation_id,lifecycle);

CREATE TABLE IF NOT EXISTS github_repositories (
    id uuid PRIMARY KEY,
    installation_id uuid NOT NULL REFERENCES github_installations(id) ON DELETE CASCADE,
    github_repository_id bigint NOT NULL CHECK (github_repository_id > 0),
    github_owner_id bigint NOT NULL CHECK (github_owner_id > 0),
    owner_login text NOT NULL,
    name text NOT NULL,
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','removed')),
    last_verified_at timestamptz NOT NULL,
    removed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (installation_id,github_repository_id),
    UNIQUE (id,installation_id),
    CHECK ((lifecycle='active' AND removed_at IS NULL) OR
        (lifecycle='removed' AND removed_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS github_repositories_provider_idx
    ON github_repositories(github_repository_id,installation_id,lifecycle);

-- Generic one-time claims support setup/OAuth nonces and permanent delivery
-- tombstones. The trigger below forbids tombstone deletion or demotion.
CREATE TABLE IF NOT EXISTS github_one_time_claims (
    kind text NOT NULL,
    claim_key text NOT NULL,
    retain_until timestamptz NOT NULL,
    permanent boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind,claim_key),
    CHECK (claim_key ~ '^[0-9a-f]{64}$'),
    CHECK (kind IN ('github-state','github-delivery'))
);
CREATE INDEX IF NOT EXISTS github_one_time_claims_expiry_idx
    ON github_one_time_claims(retain_until) WHERE permanent=false;

CREATE OR REPLACE FUNCTION protect_permanent_github_claim()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.permanent THEN
        RAISE EXCEPTION 'permanent GitHub claim tombstones cannot be changed or deleted'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND (NEW.kind <> OLD.kind OR NEW.claim_key <> OLD.claim_key) THEN
        RAISE EXCEPTION 'GitHub claim identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS github_one_time_claims_protect ON github_one_time_claims;
CREATE TRIGGER github_one_time_claims_protect
    BEFORE UPDATE OR DELETE ON github_one_time_claims
    FOR EACH ROW EXECUTE FUNCTION protect_permanent_github_claim();

CREATE TABLE IF NOT EXISTS github_webhook_receipts (
    claim_key text PRIMARY KEY,
    claim_kind text NOT NULL DEFAULT 'github-delivery' CHECK (claim_kind='github-delivery'),
    github_app_id bigint NOT NULL CHECK (github_app_id > 0),
    github_installation_id bigint NOT NULL CHECK (github_installation_id > 0),
    delivery_id uuid NOT NULL,
    event text NOT NULL,
    body_sha256 text NOT NULL,
    -- The closed typed event is resumable work, not the replay tombstone.
    -- It may be purged after retain_until only once processing is terminal.
    typed_event jsonb,
    repository_id bigint,
    git_ref text NOT NULL DEFAULT '',
    state text NOT NULL CHECK (state IN ('claimed','processing','enqueued','ignored','failed')),
    failure_code text NOT NULL DEFAULT '',
    lease_owner text,
    lease_until timestamptz,
    available_at timestamptz NOT NULL DEFAULT now(),
    received_at timestamptz NOT NULL,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (github_app_id,github_installation_id,delivery_id),
    FOREIGN KEY (claim_kind,claim_key)
        REFERENCES github_one_time_claims(kind,claim_key) ON DELETE RESTRICT,
    CHECK (body_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    CHECK (typed_event IS NULL OR jsonb_typeof(typed_event)='object'),
    CHECK (typed_event IS NOT NULL OR state IN ('enqueued','ignored','failed')),
    CHECK (event IN ('push','installation','installation_repositories')),
    CHECK ((event='push' AND repository_id IS NOT NULL AND git_ref <> '') OR
        (event<>'push' AND repository_id IS NULL AND git_ref='')),
    CHECK ((state IN ('enqueued','ignored','failed') AND completed_at IS NOT NULL AND lease_owner IS NULL AND lease_until IS NULL)
        OR (state IN ('claimed','processing') AND completed_at IS NULL)),
    CHECK ((lease_owner IS NULL)=(lease_until IS NULL))
);
CREATE INDEX IF NOT EXISTS github_webhook_receipts_work_idx
    ON github_webhook_receipts(state,available_at,received_at)
    WHERE state IN ('claimed','processing');

-- The application id is unique today but is made explicitly composite so a
-- definition can enforce that its service belongs to its project in one FK.
ALTER TABLE applications
    ADD CONSTRAINT applications_id_project_unique UNIQUE (id,project_id);

CREATE TABLE IF NOT EXISTS build_definitions (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    service_id uuid NOT NULL,
    installation_id uuid NOT NULL REFERENCES github_installations(id) ON DELETE RESTRICT,
    repository_id uuid NOT NULL,
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    trigger_ref text NOT NULL,
    spec jsonb NOT NULL,
    definition_digest text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id,service_id,repository_id,trigger_ref),
    FOREIGN KEY (service_id,project_id) REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (repository_id,installation_id) REFERENCES github_repositories(id,installation_id) ON DELETE RESTRICT,
    CHECK (jsonb_typeof(spec)='object'),
    CHECK (definition_digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS build_definitions_push_idx
    ON build_definitions(installation_id,repository_id,trigger_ref)
    WHERE enabled=true;

CREATE TABLE IF NOT EXISTS build_service_generations (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    service_id uuid NOT NULL,
    last_generation bigint NOT NULL CHECK (last_generation >= 0),
    PRIMARY KEY (project_id,service_id),
    FOREIGN KEY (service_id,project_id) REFERENCES applications(id,project_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS build_attempts (
    id uuid PRIMARY KEY,
    definition_id uuid NOT NULL REFERENCES build_definitions(id) ON DELETE RESTRICT,
    delivery_claim_key text NOT NULL REFERENCES github_webhook_receipts(claim_key) ON DELETE RESTRICT,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    service_id uuid NOT NULL,
    commit_sha text NOT NULL,
    git_ref text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    definition_digest text NOT NULL,
    plan_request jsonb NOT NULL,
    checkout_request jsonb NOT NULL,
    input_digest text NOT NULL,
    registry_mode text NOT NULL CHECK (registry_mode IN ('managed','external')),
    state text NOT NULL CHECK (state IN ('queued','preparing','running','cancelling','succeeded','failed','cancelled')),
    execution_attempts integer NOT NULL DEFAULT 0 CHECK (execution_attempts >= 0),
    max_attempts integer NOT NULL CHECK (max_attempts BETWEEN 1 AND 5),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    job_namespace text NOT NULL,
    job_name text NOT NULL,
    cache_candidate text NOT NULL,
    cache_reference text NOT NULL DEFAULT '',
    result jsonb,
    log_reference text NOT NULL DEFAULT '',
    failure_code text NOT NULL DEFAULT '',
    cancel_requested_at timestamptz,
    started_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (delivery_claim_key,definition_id),
    UNIQUE (project_id,service_id,generation),
    FOREIGN KEY (service_id,project_id) REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    CHECK (definition_digest ~ '^sha256:[0-9a-f]{64}$'),
    CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
    CHECK (jsonb_typeof(plan_request)='object' AND jsonb_typeof(checkout_request)='object'),
    CHECK ((lease_owner IS NULL)=(lease_until IS NULL)),
    CHECK ((state IN ('succeeded','failed','cancelled') AND completed_at IS NOT NULL AND lease_owner IS NULL AND lease_until IS NULL)
        OR (state NOT IN ('succeeded','failed','cancelled') AND completed_at IS NULL)),
    CHECK ((state='succeeded' AND result IS NOT NULL AND failure_code='') OR state<>'succeeded')
);
CREATE INDEX IF NOT EXISTS build_attempts_work_idx
    ON build_attempts(state,available_at,created_at)
    WHERE state IN ('queued','preparing','running','cancelling');
CREATE INDEX IF NOT EXISTS build_attempts_service_cache_idx
    ON build_attempts(project_id,service_id,definition_digest,generation DESC)
    WHERE state='succeeded' AND cache_reference<>'';

CREATE TABLE IF NOT EXISTS build_outbox (
    attempt_id uuid PRIMARY KEY REFERENCES build_attempts(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind='source-build'),
    trace_id text NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error_code text NOT NULL DEFAULT '',
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS build_outbox_pending_idx
    ON build_outbox(available_at,created_at) WHERE published_at IS NULL;
