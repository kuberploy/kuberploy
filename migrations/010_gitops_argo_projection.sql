-- Git authority bindings, provider-verified head observations, rebuildable
-- shadow projections and Argo observed state. Repository URLs, credentials and
-- tenant-selected destination namespaces/projects are deliberately absent.
ALTER TABLE environments
    ADD CONSTRAINT environments_id_project_unique UNIQUE (id,project_id);

CREATE TABLE IF NOT EXISTS git_repository_bindings (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('platform','environment')),
    scope_id uuid NOT NULL,
    project_id uuid REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid,
    cluster_id uuid,
    provider text NOT NULL CHECK (provider='github'),
    installation_id bigint NOT NULL CHECK (installation_id > 0),
    repository_id bigint NOT NULL CHECK (repository_id > 0),
    repository_owner text NOT NULL CHECK (repository_owner ~ '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$'),
    repository_name text NOT NULL CHECK (repository_name ~ '^[A-Za-z0-9_.-]{1,100}$' AND repository_name NOT IN ('.','..')),
    target_ref text NOT NULL CHECK (target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$' AND target_ref !~ '\.\.' AND target_ref !~ '//'),
    path_prefix text NOT NULL CHECK (path_prefix !~ '(^/|/\.\.?(/|$)|//|\\)' AND length(path_prefix) BETWEEN 1 AND 1024),
    credential_secret_name text NOT NULL CHECK (credential_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$'),
    state text NOT NULL CHECK (state IN ('ready','indexing','waiting-for-git','diverged','missing-ref')),
    target_head_revision text CHECK (target_head_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    indexed_revision text CHECK (indexed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    projection_generation bigint NOT NULL DEFAULT 0 CHECK (projection_generation >= 0),
    parser_version text NOT NULL CHECK (length(parser_version) BETWEEN 1 AND 64),
    target_head_observed_at timestamptz,
    indexed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (provider,installation_id,repository_id,target_ref,scope_id),
    UNIQUE (id,target_ref),
    UNIQUE (id,project_id,environment_id),
    FOREIGN KEY (environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    CHECK ((kind='environment' AND scope_id=environment_id AND project_id IS NOT NULL AND cluster_id IS NULL
               AND path_prefix='tenants/'||project_id::text||'/environments/'||environment_id::text)
        OR (kind='platform' AND scope_id=cluster_id AND project_id IS NULL AND environment_id IS NULL
               AND path_prefix='clusters/'||cluster_id::text)),
    CHECK ((indexed_revision IS NULL AND projection_generation=0 AND indexed_at IS NULL)
        OR (indexed_revision IS NOT NULL AND projection_generation>0 AND indexed_at IS NOT NULL)),
    CHECK ((target_head_revision IS NULL)=(target_head_observed_at IS NULL)),
    CHECK (target_head_observed_at IS NULL OR (target_head_observed_at>=created_at AND target_head_observed_at<=updated_at)),
    CHECK (indexed_at IS NULL OR (indexed_at>=created_at AND indexed_at<=updated_at)),
    CHECK (state<>'ready' OR (target_head_revision IS NOT NULL AND target_head_revision=indexed_revision)),
    CHECK (state NOT IN ('indexing','diverged') OR target_head_revision IS NOT NULL),
    CHECK (updated_at>=created_at)
);
CREATE INDEX IF NOT EXISTS git_repository_bindings_work_idx
    ON git_repository_bindings(state,updated_at,id) WHERE state<>'ready';

CREATE OR REPLACE FUNCTION protect_git_binding_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.kind,NEW.scope_id,NEW.project_id,NEW.environment_id,NEW.cluster_id,
           NEW.provider,NEW.installation_id,NEW.repository_id,NEW.repository_owner,
           NEW.repository_name,NEW.target_ref,NEW.path_prefix,NEW.credential_secret_name)
       IS DISTINCT FROM
       ROW(OLD.kind,OLD.scope_id,OLD.project_id,OLD.environment_id,OLD.cluster_id,
           OLD.provider,OLD.installation_id,OLD.repository_id,OLD.repository_owner,
           OLD.repository_name,OLD.target_ref,OLD.path_prefix,OLD.credential_secret_name) THEN
        RAISE EXCEPTION 'Git binding identity is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS git_repository_bindings_identity ON git_repository_bindings;
CREATE TRIGGER git_repository_bindings_identity
    BEFORE UPDATE ON git_repository_bindings
    FOR EACH ROW EXECUTE FUNCTION protect_git_binding_identity();

CREATE TABLE IF NOT EXISTS git_verified_head_observations (
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE RESTRICT,
    provider text NOT NULL CHECK (provider='github'),
    installation_id bigint NOT NULL CHECK (installation_id>0),
    repository_id bigint NOT NULL CHECK (repository_id>0),
    repository_owner text NOT NULL,
    repository_name text NOT NULL,
    target_ref text NOT NULL,
    commit_revision text NOT NULL CHECK (commit_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    source text NOT NULL CHECK (source IN ('verified-webhook','safety-poll','write-finalization')),
    provider_request text NOT NULL CHECK (length(provider_request) BETWEEN 1 AND 256),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (binding_id,commit_revision,source,provider_request),
    FOREIGN KEY (binding_id,target_ref) REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS git_verified_head_observations_latest_idx
    ON git_verified_head_observations(binding_id,observed_at DESC);

-- Push payloads only wake the verifier. Permanent tombstones prevent duplicate
-- delivery after retry/restart but do not make the advertised SHA authoritative.
CREATE TABLE IF NOT EXISTS git_webhook_tombstones (
    provider text NOT NULL CHECK (provider='github'),
    repository_id bigint NOT NULL CHECK (repository_id>0),
    target_ref text NOT NULL,
    after_commit text NOT NULL CHECK (after_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    delivery_hash text NOT NULL CHECK (delivery_hash ~ '^sha256:[0-9a-f]{64}$'),
    received_at timestamptz NOT NULL,
    PRIMARY KEY (provider,delivery_hash),
    UNIQUE (provider,repository_id,target_ref,after_commit)
);

CREATE OR REPLACE FUNCTION protect_git_webhook_tombstone()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Git webhook tombstones are permanent'
        USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS git_webhook_tombstones_protect ON git_webhook_tombstones;
CREATE TRIGGER git_webhook_tombstones_protect
    BEFORE UPDATE OR DELETE ON git_webhook_tombstones
    FOR EACH ROW EXECUTE FUNCTION protect_git_webhook_tombstone();

CREATE TABLE IF NOT EXISTS git_safety_poll_cursors (
    binding_id uuid PRIMARY KEY REFERENCES git_repository_bindings(id) ON DELETE CASCADE,
    last_commit text CHECK (last_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    provider_cursor text NOT NULL DEFAULT '' CHECK (length(provider_cursor)<=512),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 32),
    next_poll_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS git_safety_poll_due_idx ON git_safety_poll_cursors(next_poll_at,binding_id);

CREATE TABLE IF NOT EXISTS git_projection_generations (
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE CASCADE,
    generation bigint NOT NULL CHECK (generation>0),
    head_revision text NOT NULL CHECK (head_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    parser_version text NOT NULL CHECK (length(parser_version) BETWEEN 1 AND 64),
    state text NOT NULL CHECK (state IN ('staging','active','failed')),
    started_at timestamptz NOT NULL,
    activated_at timestamptz,
    PRIMARY KEY (binding_id,generation),
    CHECK ((state='active' AND activated_at IS NOT NULL AND activated_at>=started_at) OR (state<>'active' AND activated_at IS NULL))
);

CREATE TABLE IF NOT EXISTS git_projected_documents (
    binding_id uuid NOT NULL,
    generation bigint NOT NULL,
    path text NOT NULL CHECK (path !~ '(^/|/\.\.?(/|$)|//|\\)' AND length(path) BETWEEN 1 AND 1024),
    application_id uuid,
    source_revision text NOT NULL CHECK (source_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    config_revision text NOT NULL CHECK (config_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    blob_id text NOT NULL CHECK (blob_id ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    raw bytea NOT NULL CHECK (octet_length(raw) BETWEEN 1 AND 262144),
    parsed jsonb,
    valid boolean NOT NULL,
    diagnostics jsonb NOT NULL CHECK (
        CASE WHEN jsonb_typeof(diagnostics)='array' THEN jsonb_array_length(diagnostics)<=64 ELSE false END
    ),
    schema_version text NOT NULL,
    parser_version text NOT NULL,
    indexed_at timestamptz NOT NULL,
    PRIMARY KEY (binding_id,generation,path),
    FOREIGN KEY (binding_id,generation) REFERENCES git_projection_generations(binding_id,generation) ON DELETE CASCADE,
    CHECK (
        CASE WHEN jsonb_typeof(diagnostics)='array'
            THEN (valid AND jsonb_array_length(diagnostics)=0) OR (NOT valid AND jsonb_array_length(diagnostics)>0)
            ELSE false
        END
    )
);
CREATE INDEX IF NOT EXISTS git_projected_documents_application_idx
    ON git_projected_documents(application_id,binding_id,generation) WHERE application_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS git_path_reservations (
    binding_id uuid NOT NULL,
    target_ref text NOT NULL,
    path text NOT NULL CHECK (path !~ '(^/|/\.\.?(/|$)|//|\\)' AND length(path) BETWEEN 1 AND 1024),
    operation_id uuid NOT NULL,
    owner text NOT NULL CHECK (length(owner) BETWEEN 1 AND 128),
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    committed_revision text CHECK (committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    state text NOT NULL CHECK (state IN ('candidate','committed-pending-index')),
    lease_until timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (binding_id,target_ref,path),
    FOREIGN KEY (binding_id,target_ref) REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    CHECK ((state='candidate' AND lease_until IS NOT NULL AND committed_revision IS NULL)
        OR (state='committed-pending-index' AND lease_until IS NULL AND committed_revision IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS git_path_reservations_repair_idx
    ON git_path_reservations(lease_until) WHERE state='candidate';

-- Argo observations are runtime projections. They never advance desired Git
-- state and cannot be used as an imperative sync/rollback command channel.
CREATE TABLE IF NOT EXISTS argo_application_observations (
    application_id uuid PRIMARY KEY REFERENCES applications(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    argo_uid uuid NOT NULL,
    argo_namespace text NOT NULL,
    argo_name text NOT NULL,
    destination_namespace text NOT NULL,
    desired_revision text NOT NULL CHECK (desired_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    observed_revision text NOT NULL CHECK (observed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    sync_status text NOT NULL CHECK (sync_status IN ('unknown','synced','out-of-sync')),
    health_status text NOT NULL CHECK (health_status IN ('unknown','progressing','healthy','degraded','suspended','missing')),
    operation_phase text NOT NULL CHECK (operation_phase IN ('','running','succeeded','failed','error','terminating')),
    message text NOT NULL DEFAULT '' CHECK (length(message)<=1024),
    resources jsonb NOT NULL CHECK (
        CASE WHEN jsonb_typeof(resources)='array' THEN jsonb_array_length(resources)<=512 ELSE false END
    ),
    observed_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (application_id,project_id) REFERENCES applications(id,project_id) ON DELETE CASCADE,
    FOREIGN KEY (environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE,
    CHECK (updated_at>=observed_at)
);

CREATE TABLE IF NOT EXISTS argo_rollback_commands (
    id uuid PRIMARY KEY,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE RESTRICT,
    operation_id uuid NOT NULL UNIQUE,
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    expected_etag text NOT NULL CHECK (expected_etag ~ '^"sha256:[0-9a-f]{64}"$'),
    release_repository text NOT NULL,
    release_digest text NOT NULL CHECK (release_digest ~ '^sha256:[0-9a-f]{64}$'),
    path text NOT NULL CHECK (path !~ '(^/|/\.\.?(/|$)|//|\\)' AND length(path) BETWEEN 1 AND 1024),
    candidate bytea NOT NULL CHECK (octet_length(candidate) BETWEEN 1 AND 262144),
    candidate_sha256 text NOT NULL CHECK (candidate_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    commit_message text NOT NULL CHECK (length(commit_message) BETWEEN 1 AND 512),
    state text NOT NULL CHECK (state IN ('pending-git','git-committed','failed')),
    git_revision text CHECK (git_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    failure_code text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (application_id,project_id) REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (binding_id,project_id,environment_id) REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE RESTRICT,
    CHECK ((state='git-committed' AND git_revision IS NOT NULL AND failure_code='')
        OR (state='failed' AND git_revision IS NULL AND failure_code<>'')
        OR (state='pending-git' AND git_revision IS NULL AND failure_code='')),
    CHECK (updated_at>=created_at)
);
