CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now(),
    baseline text NOT NULL DEFAULT '0.1.0' CHECK (baseline = '0.1.0')
);
ALTER TABLE schema_migrations
    ADD COLUMN IF NOT EXISTS baseline text NOT NULL DEFAULT '0.1.0'
    CHECK (baseline = '0.1.0');

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
    detail jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (
        jsonb_typeof(detail) = 'object' AND octet_length(detail::text) <= 65536
    ),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_target_idx ON audit_events (target_type, target_id, created_at);

ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_environment_id_application_id_key;

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS generation bigint NOT NULL DEFAULT 1;

CREATE UNIQUE INDEX IF NOT EXISTS deployments_environment_application_unique
    ON deployments (environment_id, application_id);

CREATE TABLE IF NOT EXISTS deployment_operation_inputs (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    image text NOT NULL,
    replicas integer NOT NULL CHECK (replicas BETWEEN 1 AND 100),
    port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    environment jsonb NOT NULL DEFAULT '{}'::jsonb,
    route jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deployment_operation_inputs_deployment_idx
    ON deployment_operation_inputs (deployment_id, created_at);

INSERT INTO deployment_operation_inputs
    (operation_id, deployment_id, image, replicas, port, environment, route, created_at)
SELECT operation_id, id, image, replicas, port, environment, route, created_at
FROM deployments
ON CONFLICT (operation_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS platform_upgrades (
    id uuid PRIMARY KEY,
    version text NOT NULL,
    manifest_digest text NOT NULL,
    manifest jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('queued','running','succeeded','failed','cancelled')),
    operation_id uuid NOT NULL UNIQUE REFERENCES operations(id) DEFERRABLE INITIALLY DEFERRED,
    runner_ref text NOT NULL DEFAULT '',
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS platform_upgrades_one_active
    ON platform_upgrades ((true))
    WHERE state IN ('queued','running');

ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users
    ADD CONSTRAINT users_role_check CHECK (role IN ('platform-admin','developer'));

CREATE TABLE IF NOT EXISTS user_invitations (
    id uuid PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    display_name text NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_user_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((accepted_at IS NULL) = (accepted_user_id IS NULL))
);
CREATE INDEX IF NOT EXISTS user_invitations_expires_idx
    ON user_invitations (expires_at) WHERE accepted_at IS NULL;

CREATE TABLE IF NOT EXISTS teams (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    slug text NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS team_memberships (
    team_id uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner','member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id,user_id)
);
CREATE INDEX IF NOT EXISTS team_memberships_user_idx
    ON team_memberships (user_id,team_id);

CREATE TABLE IF NOT EXISTS github_installations (
    id uuid PRIMARY KEY,
    github_installation_id bigint NOT NULL UNIQUE CHECK (github_installation_id > 0),
    account_login text NOT NULL,
    account_type text NOT NULL CHECK (account_type IN ('User','Organization')),
    owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    visibility text NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','team')),
    team_id uuid REFERENCES teams(id) ON DELETE RESTRICT,
    repository_selection text NOT NULL CHECK (repository_selection IN ('all','selected')),
    repository_count integer NOT NULL CHECK (repository_count >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((visibility='private' AND team_id IS NULL) OR (visibility='team' AND team_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS github_installations_owner_idx
    ON github_installations (owner_user_id,created_at);
CREATE INDEX IF NOT EXISTS github_installations_team_idx
    ON github_installations (team_id,created_at) WHERE visibility='team';

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS team_id uuid REFERENCES teams(id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS projects_team_idx ON projects (team_id,created_at);

-- Exact asset bytes, rather than a JSONB reserialization, bind every durable
-- upgrade to the release-manifest digest that the API verified.
ALTER TABLE platform_upgrades
    ADD COLUMN IF NOT EXISTS manifest_bytes bytea NOT NULL DEFAULT ''::bytea;

-- New upgrades must always provide the exact bytes. The temporary default
-- above only permits migration of installations with historical rows; those
-- rows fail closed in the runner because an empty manifest cannot verify.
ALTER TABLE platform_upgrades
    ALTER COLUMN manifest_bytes DROP DEFAULT;

-- Registry targets deliberately have no delete credential. Kuberploy only
-- obtains repository-scoped pull, push, and cache credentials; deletion and
-- garbage collection are capabilities of the managed-registry controller.
CREATE TABLE IF NOT EXISTS registry_targets (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE,
    mode text NOT NULL CHECK (mode IN ('managed','external')),
    endpoint text NOT NULL,
    repository_prefix text NOT NULL,
    pull_credential_ref text NOT NULL DEFAULT '',
    push_credential_ref text NOT NULL DEFAULT '',
    cache_credential_ref text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (endpoint <> ''),
    CHECK (repository_prefix <> '')
);

CREATE OR REPLACE FUNCTION reject_registry_target_mode_change()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.mode <> NEW.mode THEN
        RAISE EXCEPTION 'registry target mode is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS registry_targets_mode_immutable ON registry_targets;
CREATE TRIGGER registry_targets_mode_immutable
    BEFORE UPDATE OF mode ON registry_targets
    FOR EACH ROW EXECUTE FUNCTION reject_registry_target_mode_change();

CREATE TABLE IF NOT EXISTS service_registry_policies (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    service_id text NOT NULL,
    repository text NOT NULL,
    keep_last_successful integer NOT NULL DEFAULT 10
        CHECK (keep_last_successful BETWEEN 1 AND 100),
    minimum_safety_age_seconds bigint NOT NULL DEFAULT 86400
        CHECK (minimum_safety_age_seconds >= 60),
    cache_keep_generations integer NOT NULL DEFAULT 2
        CHECK (cache_keep_generations BETWEEN 1 AND 20),
    cache_unused_expiry_seconds bigint NOT NULL DEFAULT 604800
        CHECK (cache_unused_expiry_seconds >= 60),
    cache_byte_quota bigint NOT NULL DEFAULT 10737418240
        CHECK (cache_byte_quota > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (registry_target_id, service_id),
    UNIQUE (registry_target_id, repository),
    CHECK (service_id <> ''),
    CHECK (repository <> '')
);

-- The catalog is a materialized observation of an OCI repository. A cleanup
-- plan is valid only when the most recent observation is explicitly complete.
CREATE TABLE IF NOT EXISTS registry_catalog_observations (
    id uuid PRIMARY KEY,
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE CASCADE,
    repository text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    complete boolean NOT NULL,
    snapshot_digest text NOT NULL,
    observed_at timestamptz NOT NULL,
    manifest_count integer NOT NULL CHECK (manifest_count >= 0),
    blob_count integer NOT NULL CHECK (blob_count >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (registry_target_id, repository, revision),
    CHECK (snapshot_digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS registry_catalog_observations_latest_idx
    ON registry_catalog_observations (registry_target_id, repository, revision DESC);

-- A repository enumeration checkpoint closes the registry-wide reachability
-- set. Without it Kuberploy may delete manifests, but it must never claim that
-- a blob is globally unreachable.
CREATE TABLE IF NOT EXISTS registry_inventory_observations (
    registry_target_id uuid PRIMARY KEY REFERENCES registry_targets(id) ON DELETE CASCADE,
    revision text NOT NULL,
    complete boolean NOT NULL,
    repositories jsonb NOT NULL,
    observed_at timestamptz NOT NULL,
    CHECK (revision <> ''),
    CHECK (jsonb_typeof(repositories)='array')
);

CREATE TABLE IF NOT EXISTS registry_manifests (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE CASCADE,
    repository text NOT NULL,
    digest text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('index','manifest')),
    media_type text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    platform_os text NOT NULL DEFAULT '',
    platform_architecture text NOT NULL DEFAULT '',
    platform_variant text NOT NULL DEFAULT '',
    present boolean NOT NULL DEFAULT true,
    first_observed_at timestamptz NOT NULL,
    last_observed_at timestamptz NOT NULL,
    last_observation_revision bigint NOT NULL CHECK (last_observation_revision > 0),
    deleted_at timestamptz,
    PRIMARY KEY (registry_target_id, repository, digest),
    CHECK (digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS registry_manifests_present_idx
    ON registry_manifests (registry_target_id, repository, present, last_observed_at);

CREATE TABLE IF NOT EXISTS registry_manifest_children (
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    parent_digest text NOT NULL,
    child_digest text NOT NULL,
    PRIMARY KEY (registry_target_id, repository, parent_digest, child_digest),
    FOREIGN KEY (registry_target_id, repository, parent_digest)
        REFERENCES registry_manifests(registry_target_id, repository, digest) ON DELETE CASCADE,
    FOREIGN KEY (registry_target_id, repository, child_digest)
        REFERENCES registry_manifests(registry_target_id, repository, digest) ON DELETE RESTRICT,
    CHECK (parent_digest <> child_digest)
);
CREATE INDEX IF NOT EXISTS registry_manifest_children_child_idx
    ON registry_manifest_children (registry_target_id, repository, child_digest);

CREATE TABLE IF NOT EXISTS registry_blobs (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE CASCADE,
    repository text NOT NULL,
    digest text NOT NULL,
    media_type text NOT NULL DEFAULT '',
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    present boolean NOT NULL DEFAULT true,
    first_observed_at timestamptz NOT NULL,
    last_observed_at timestamptz NOT NULL,
    last_observation_revision bigint NOT NULL CHECK (last_observation_revision > 0),
    deleted_at timestamptz,
    PRIMARY KEY (registry_target_id, repository, digest),
    CHECK (digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS registry_blobs_present_idx
    ON registry_blobs (registry_target_id, repository, present, last_observed_at);

CREATE TABLE IF NOT EXISTS registry_manifest_blobs (
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    manifest_digest text NOT NULL,
    blob_digest text NOT NULL,
    PRIMARY KEY (registry_target_id, repository, manifest_digest, blob_digest),
    FOREIGN KEY (registry_target_id, repository, manifest_digest)
        REFERENCES registry_manifests(registry_target_id, repository, digest) ON DELETE CASCADE,
    FOREIGN KEY (registry_target_id, repository, blob_digest)
        REFERENCES registry_blobs(registry_target_id, repository, digest) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS registry_manifest_blobs_blob_idx
    ON registry_manifest_blobs (registry_target_id, repository, blob_digest);

-- Git intent, observed Pods and nonterminal operations are asynchronous
-- authorities. Each must publish a complete checkpoint before deletion can be
-- planned. Pins and releases live in this database and need no checkpoint.
CREATE TABLE IF NOT EXISTS registry_authority_observations (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE CASCADE,
    service_id text NOT NULL,
    authority text NOT NULL CHECK (authority IN ('git-intent','runtime','operations')),
    revision text NOT NULL,
    complete boolean NOT NULL,
    snapshot_digest text NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (registry_target_id, service_id, authority),
    CHECK (service_id <> ''),
    CHECK (revision <> ''),
    CHECK (snapshot_digest ~ '^sha256:[0-9a-f]{64}$')
);

CREATE TABLE IF NOT EXISTS registry_artifact_references (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE CASCADE,
    service_id text NOT NULL,
    repository text NOT NULL,
    digest text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('current-git-intent','observed-running','pin','active-operation')),
    reference_key text NOT NULL,
    source_revision text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (registry_target_id, service_id, kind, reference_key),
    CHECK (service_id <> ''),
    CHECK (repository <> ''),
    CHECK (reference_key <> ''),
    CHECK (digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS registry_artifact_references_digest_idx
    ON registry_artifact_references (registry_target_id, repository, digest);

CREATE TABLE IF NOT EXISTS registry_releases (
    id uuid PRIMARY KEY,
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    service_id text NOT NULL,
    repository text NOT NULL,
    root_digest text NOT NULL,
    created_at timestamptz NOT NULL,
    succeeded_at timestamptz,
    availability text NOT NULL DEFAULT 'present'
        CHECK (availability IN ('present','expired','missing')),
    availability_observed_at timestamptz,
    CHECK (service_id <> ''),
    CHECK (repository <> ''),
    CHECK (root_digest ~ '^sha256:[0-9a-f]{64}$'),
    CHECK ((availability='present' AND availability_observed_at IS NULL)
        OR (availability<>'present' AND availability_observed_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS registry_releases_retention_idx
    ON registry_releases (registry_target_id, service_id, succeeded_at DESC, id DESC)
    WHERE succeeded_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS registry_releases_digest_idx
    ON registry_releases (registry_target_id, repository, root_digest);

CREATE TABLE IF NOT EXISTS registry_cache_generations (
    id uuid PRIMARY KEY,
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    service_id text NOT NULL,
    repository text NOT NULL,
    platform_set text NOT NULL,
    trust_lane text NOT NULL,
    cache_schema text NOT NULL,
    build_definition_hash text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    root_digest text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    state text NOT NULL CHECK (state IN ('exporting','succeeded','failed','deleted','missing')),
    active_imports integer NOT NULL DEFAULT 0 CHECK (active_imports >= 0),
    active_exports integer NOT NULL DEFAULT 0 CHECK (active_exports >= 0),
    created_at timestamptz NOT NULL,
    completed_at timestamptz,
    last_used_at timestamptz NOT NULL,
    UNIQUE (registry_target_id, service_id, platform_set, trust_lane, cache_schema,
        build_definition_hash, generation),
    CHECK (service_id <> ''),
    CHECK (repository <> ''),
    CHECK (platform_set <> ''),
    CHECK (trust_lane <> ''),
    CHECK (cache_schema <> ''),
    CHECK (build_definition_hash <> ''),
    CHECK (root_digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS registry_cache_generations_lifecycle_idx
    ON registry_cache_generations (
        registry_target_id, service_id, platform_set, trust_lane,
        cache_schema, build_definition_hash, state, generation DESC
    );

CREATE TABLE IF NOT EXISTS registry_cleanup_plans (
    id uuid PRIMARY KEY,
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    service_id text NOT NULL,
    snapshot_token text NOT NULL,
    authority_token text NOT NULL,
    plan_digest text NOT NULL,
    state text NOT NULL DEFAULT 'preview'
        CHECK (state IN ('preview','executing','succeeded','failed','superseded')),
    policy jsonb NOT NULL,
    observations jsonb NOT NULL,
    summary jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    claimed_at timestamptz,
    completed_at timestamptz,
    failure text NOT NULL DEFAULT '',
    UNIQUE (registry_target_id, service_id, plan_digest),
    CHECK (service_id <> ''),
    CHECK (snapshot_token <> ''),
    CHECK (authority_token <> ''),
    CHECK (plan_digest ~ '^sha256:[0-9a-f]{64}$')
);
CREATE UNIQUE INDEX IF NOT EXISTS registry_cleanup_plans_one_executing_idx
    ON registry_cleanup_plans (registry_target_id, service_id)
    WHERE state='executing';

CREATE TABLE IF NOT EXISTS registry_cleanup_items (
    plan_id uuid NOT NULL REFERENCES registry_cleanup_plans(id) ON DELETE CASCADE,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    repository text NOT NULL,
    resource_kind text NOT NULL CHECK (resource_kind IN ('release-manifest','cache-manifest','blob')),
    digest text NOT NULL,
    disposition text NOT NULL CHECK (disposition IN ('protect','delete')),
    action text NOT NULL CHECK (action IN ('none','delete-manifest','garbage-collect-blob')),
    estimated_bytes bigint NOT NULL CHECK (estimated_bytes >= 0),
    reasons jsonb NOT NULL DEFAULT '[]'::jsonb,
    state text NOT NULL DEFAULT 'planned'
        CHECK (state IN ('planned','protected','deleting','deleted','skipped','failed')),
    provider_message text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plan_id, ordinal),
    UNIQUE (plan_id, repository, resource_kind, digest),
    CHECK (repository <> ''),
    CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    CHECK ((disposition='protect' AND action='none' AND state IN ('planned','protected'))
        OR (disposition='delete' AND action<>'none' AND state<>'protected'))
);

CREATE TABLE IF NOT EXISTS registry_cleanup_leases (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE CASCADE,
    repository text NOT NULL,
    plan_id uuid NOT NULL REFERENCES registry_cleanup_plans(id) ON DELETE CASCADE,
    owner text NOT NULL,
    lease_until timestamptz NOT NULL,
    PRIMARY KEY (registry_target_id, repository),
    CHECK (repository <> ''),
    CHECK (owner <> '')
);
CREATE INDEX IF NOT EXISTS registry_cleanup_leases_expiry_idx
    ON registry_cleanup_leases (lease_until);

-- A database-level guard makes it impossible for a programming mistake to
-- persist a deletion or GC plan for an external registry.
CREATE OR REPLACE FUNCTION reject_external_registry_cleanup_plan()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM registry_targets
        WHERE id=NEW.registry_target_id AND mode<>'managed'
    ) THEN
        RAISE EXCEPTION 'cleanup is forbidden for external registry targets'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS registry_cleanup_plans_managed_only ON registry_cleanup_plans;
CREATE TRIGGER registry_cleanup_plans_managed_only
    BEFORE INSERT OR UPDATE OF registry_target_id ON registry_cleanup_plans
    FOR EACH ROW EXECUTE FUNCTION reject_external_registry_cleanup_plan();

-- Preserve the complete, policy-controlled workload contract for the current
-- desired deployment and for each immutable operation input. The legacy
-- columns remain during the compatibility window for older readers.
ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS runtime jsonb;

UPDATE deployments d
SET runtime = jsonb_build_object(
    'replicas', d.replicas,
    'ports', jsonb_build_array(jsonb_build_object(
        'name', 'http',
        'containerPort', d.port,
        'protocol', 'TCP'
    )),
    'env', COALESCE((
        SELECT jsonb_agg(jsonb_build_object('name', entry.key, 'value', entry.value) ORDER BY entry.key)
        FROM jsonb_each_text(d.environment) AS entry
    ), '[]'::jsonb),
    'resources', jsonb_build_object(
        'requests', jsonb_build_object('cpu', '50m', 'memory', '100Mi')
    )
)
WHERE runtime IS NULL;

ALTER TABLE deployments
    ALTER COLUMN runtime SET NOT NULL;
ALTER TABLE deployments
    DROP CONSTRAINT IF EXISTS deployments_runtime_object;
ALTER TABLE deployments
    ADD CONSTRAINT deployments_runtime_object CHECK (jsonb_typeof(runtime) = 'object');

ALTER TABLE deployment_operation_inputs
    ADD COLUMN IF NOT EXISTS runtime jsonb;

UPDATE deployment_operation_inputs i
SET runtime = jsonb_build_object(
    'replicas', i.replicas,
    'ports', jsonb_build_array(jsonb_build_object(
        'name', 'http',
        'containerPort', i.port,
        'protocol', 'TCP'
    )),
    'env', COALESCE((
        SELECT jsonb_agg(jsonb_build_object('name', entry.key, 'value', entry.value) ORDER BY entry.key)
        FROM jsonb_each_text(i.environment) AS entry
    ), '[]'::jsonb),
    'resources', jsonb_build_object(
        'requests', jsonb_build_object('cpu', '50m', 'memory', '100Mi')
    )
)
WHERE runtime IS NULL;

ALTER TABLE deployment_operation_inputs
    ALTER COLUMN runtime SET NOT NULL;
ALTER TABLE deployment_operation_inputs
    DROP CONSTRAINT IF EXISTS deployment_operation_inputs_runtime_object;
ALTER TABLE deployment_operation_inputs
    ADD CONSTRAINT deployment_operation_inputs_runtime_object CHECK (jsonb_typeof(runtime) = 'object');

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

CREATE TABLE IF NOT EXISTS access_grants (
    id uuid PRIMARY KEY,
    subject_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('viewer','developer','project-admin','organization-admin','platform-admin')),
    scope_type text NOT NULL CHECK (scope_type IN ('platform','team','project','environment','namespace','application')),
    scope_id text NOT NULL CHECK (length(scope_id) BETWEEN 1 AND 253),
    permissions text[] NOT NULL DEFAULT ARRAY[]::text[],
    source text NOT NULL DEFAULT 'explicit' CHECK (source IN ('explicit','bootstrap')),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subject_user_id,role,scope_type,scope_id),
    CHECK (permissions <@ ARRAY['logs.read']::text[]),
    CHECK (cardinality(permissions) <= 1),
    CHECK (role<>'organization-admin' OR scope_type='team'),
    CHECK (role<>'project-admin' OR scope_type='project'),
    CHECK ((role='platform-admin') = (scope_type='platform' AND scope_id='platform')),
    CHECK (scope_type<>'platform' OR (role='platform-admin' AND scope_id='platform')),
    CHECK ((source='bootstrap') = (role='platform-admin')),
    CHECK (
        (scope_type='platform' AND scope_id='platform') OR
        (scope_type='namespace' AND scope_id ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$') OR
        (scope_type IN ('team','project','environment','application') AND
            scope_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
    )
);
CREATE INDEX IF NOT EXISTS access_grants_subject_idx
    ON access_grants (subject_user_id,scope_type,scope_id);
CREATE INDEX IF NOT EXISTS access_grants_scope_idx
    ON access_grants (scope_type,scope_id,subject_user_id);

-- Installations upgraded from schema 007 retain their administrator through
-- an explicit platform grant. New bootstrap transactions insert the same
-- durable grant before exposing the session.
INSERT INTO access_grants(id,subject_user_id,role,scope_type,scope_id,source,created_by,created_at)
SELECT gen_random_uuid(),u.id,'platform-admin','platform','platform','bootstrap',u.id,u.created_at
FROM users u
WHERE u.role='platform-admin'
ON CONFLICT (subject_user_id,role,scope_type,scope_id) DO NOTHING;

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

-- Runtime secret values are deliberately absent from this schema. Providers
-- receive write-only material in memory; PostgreSQL keeps only keyed HMAC
-- fingerprints, immutable delivery metadata and opaque provider identities.
ALTER TABLE projects
    ADD CONSTRAINT projects_id_team_unique UNIQUE (id,team_id);
ALTER TABLE environments
    ADD CONSTRAINT environments_id_project_namespace_unique UNIQUE (id,project_id,namespace);

CREATE TABLE IF NOT EXISTS secret_bindings (
    id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES teams(id) ON DELETE RESTRICT,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    target_namespace text NOT NULL,
    name text NOT NULL CHECK (
        length(name) BETWEEN 1 AND 63 AND
        name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    provider text NOT NULL CHECK (provider IN ('external-secrets','sealed-secrets')),
    state text NOT NULL CHECK (state IN ('provisioning','ready','deleting','deleted','failed')),
    active_version bigint NOT NULL DEFAULT 0 CHECK (active_version >= 0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    delete_started_at timestamptz,
    deleted_at timestamptz,
    UNIQUE (project_id,environment_id,application_id,name),
    UNIQUE (id,provider),
    FOREIGN KEY (project_id,organization_id) REFERENCES projects(id,team_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id,project_id,target_namespace)
        REFERENCES environments(id,project_id,namespace) ON DELETE RESTRICT,
    FOREIGN KEY (application_id,project_id) REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    CHECK (updated_at >= created_at),
    CHECK (
        (state='ready' AND active_version>0) OR
        (state IN ('provisioning','failed','deleted') AND active_version=0) OR
        state='deleting'
    ),
    CHECK (
        (state='deleting' AND delete_started_at IS NOT NULL AND deleted_at IS NULL) OR
        (state='deleted' AND delete_started_at IS NOT NULL AND deleted_at IS NOT NULL AND deleted_at>=delete_started_at) OR
        (state NOT IN ('deleting','deleted') AND delete_started_at IS NULL AND deleted_at IS NULL)
    )
);
CREATE INDEX IF NOT EXISTS secret_bindings_scope_idx
    ON secret_bindings (organization_id,project_id,environment_id,application_id,created_at,id);

CREATE OR REPLACE FUNCTION protect_secret_binding_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.organization_id,NEW.project_id,NEW.environment_id,NEW.application_id,
           NEW.target_namespace,NEW.name,NEW.provider,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.organization_id,OLD.project_id,OLD.environment_id,OLD.application_id,
           OLD.target_namespace,OLD.name,OLD.provider,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'secret binding identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'secret binding time cannot move backwards' USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='provisioning' AND NEW.state IN ('ready','failed')) OR
        (OLD.state='ready' AND NEW.state IN ('ready','deleting')) OR
        (OLD.state='failed' AND NEW.state='deleting') OR
        (OLD.state='deleting' AND NEW.state='deleted') OR
        OLD.state=NEW.state
    ) THEN
        RAISE EXCEPTION 'invalid secret binding transition' USING ERRCODE='23514';
    END IF;
    IF NEW.state='ready' AND NOT EXISTS (
        SELECT 1 FROM secret_binding_versions v
        WHERE v.binding_id=NEW.id AND v.version_number=NEW.active_version AND v.state='active'
    ) THEN
        RAISE EXCEPTION 'active secret binding version is not ready' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS secret_bindings_identity ON secret_bindings;
CREATE TRIGGER secret_bindings_identity
    BEFORE UPDATE ON secret_bindings
    FOR EACH ROW EXECUTE FUNCTION protect_secret_binding_identity();

CREATE TABLE IF NOT EXISTS secret_binding_versions (
    id uuid PRIMARY KEY,
    binding_id uuid NOT NULL,
    version_number bigint NOT NULL CHECK (version_number > 0),
    provider text NOT NULL CHECK (provider IN ('external-secrets','sealed-secrets')),
    state text NOT NULL CHECK (state IN ('staging','awaiting-readiness','active','retained','failed','deleted')),
    fingerprint_key_id text NOT NULL CHECK (
        length(fingerprint_key_id) BETWEEN 1 AND 128 AND
        fingerprint_key_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
    ),
    content_fingerprint bytea NOT NULL CHECK (octet_length(content_fingerprint)=32),
    provider_object_name text,
    target_secret_name text,
    provider_revision text CHECK (provider_revision IS NULL OR (
        length(provider_revision) BETWEEN 1 AND 256 AND provider_revision=btrim(provider_revision) AND provider_revision !~ '[[:cntrl:]]'
    )),
    manifest_digest text,
    sealed_key_fingerprint text,
    ciphertext_digest text,
    failure_code text NOT NULL DEFAULT '' CHECK (
        failure_code='' OR failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    staged_at timestamptz,
    readiness_observed_at timestamptz,
    activated_at timestamptz,
    retained_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (binding_id,version_number),
    UNIQUE (id,binding_id),
    FOREIGN KEY (binding_id,provider) REFERENCES secret_bindings(id,provider) ON DELETE RESTRICT,
    CHECK (updated_at >= created_at),
    CHECK (
        (provider_object_name IS NULL AND target_secret_name IS NULL AND provider_revision IS NULL AND
         manifest_digest IS NULL AND sealed_key_fingerprint IS NULL AND ciphertext_digest IS NULL) OR
        (provider_object_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$' AND
         target_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$' AND
         length(provider_revision) BETWEEN 1 AND 256 AND
         manifest_digest ~ '^sha256:[0-9a-f]{64}$' AND
         ((provider='external-secrets' AND sealed_key_fingerprint IS NULL AND ciphertext_digest IS NULL) OR
          (provider='sealed-secrets' AND sealed_key_fingerprint ~ '^sha256:[0-9a-f]{64}$' AND
           ciphertext_digest ~ '^sha256:[0-9a-f]{64}$')))
    ),
    CHECK (state IN ('staging','failed','deleted') OR provider_object_name IS NOT NULL),
    CHECK ((state='staging' AND staged_at IS NULL) OR (state<>'staging' AND (staged_at IS NOT NULL OR state='failed'))),
    CHECK ((state IN ('active','retained') AND activated_at IS NOT NULL) OR state NOT IN ('active','retained')),
    CHECK ((state='retained' AND retained_at IS NOT NULL) OR state<>'retained'),
    CHECK ((state='failed' AND failure_code<>'') OR (state NOT IN ('failed','deleted') AND failure_code='') OR state='deleted')
);
CREATE UNIQUE INDEX IF NOT EXISTS secret_binding_one_pending_version
    ON secret_binding_versions(binding_id) WHERE state IN ('staging','awaiting-readiness');
CREATE UNIQUE INDEX IF NOT EXISTS secret_binding_one_active_version
    ON secret_binding_versions(binding_id) WHERE state='active';

CREATE OR REPLACE FUNCTION protect_secret_binding_version()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.binding_id,NEW.version_number,NEW.provider,NEW.fingerprint_key_id,
           NEW.content_fingerprint,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.binding_id,OLD.version_number,OLD.provider,OLD.fingerprint_key_id,
           OLD.content_fingerprint,OLD.created_at) THEN
        RAISE EXCEPTION 'secret binding version identity is immutable' USING ERRCODE='23514';
    END IF;
    IF ROW(NEW.provider_object_name,NEW.target_secret_name,NEW.provider_revision,
           NEW.manifest_digest,NEW.sealed_key_fingerprint,NEW.ciphertext_digest)
       IS DISTINCT FROM
       ROW(OLD.provider_object_name,OLD.target_secret_name,OLD.provider_revision,
           OLD.manifest_digest,OLD.sealed_key_fingerprint,OLD.ciphertext_digest)
       AND NOT (OLD.state='staging' AND NEW.state='awaiting-readiness'
                AND OLD.provider_object_name IS NULL) THEN
        RAISE EXCEPTION 'secret binding provider artifact is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'secret binding version time cannot move backwards' USING ERRCODE='23514';
    END IF;
    IF NEW.failure_code IS DISTINCT FROM OLD.failure_code
       AND NOT (NEW.state='failed' AND OLD.failure_code='') THEN
        RAISE EXCEPTION 'secret binding failure is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.staged_at IS DISTINCT FROM OLD.staged_at
       AND NOT (OLD.state='staging' AND NEW.state='awaiting-readiness' AND OLD.staged_at IS NULL AND NEW.staged_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding staged time is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.readiness_observed_at IS DISTINCT FROM OLD.readiness_observed_at
       AND NOT (OLD.state IN ('staging','awaiting-readiness') AND NEW.state IN ('active','failed')
                AND OLD.readiness_observed_at IS NULL AND NEW.readiness_observed_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding readiness time is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.activated_at IS DISTINCT FROM OLD.activated_at
       AND NOT (OLD.state='awaiting-readiness' AND NEW.state='active' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding activation time is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.retained_at IS DISTINCT FROM OLD.retained_at
       AND NOT (OLD.state='active' AND NEW.state='retained' AND OLD.retained_at IS NULL AND NEW.retained_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding retention time is immutable' USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='staging' AND NEW.state IN ('awaiting-readiness','failed','deleted')) OR
        (OLD.state='awaiting-readiness' AND NEW.state IN ('active','failed','deleted')) OR
        (OLD.state='active' AND NEW.state IN ('retained','deleted')) OR
        (OLD.state='retained' AND NEW.state='deleted') OR
        (OLD.state='failed' AND NEW.state='deleted') OR
        OLD.state=NEW.state
    ) THEN
        RAISE EXCEPTION 'invalid secret binding version transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS secret_binding_versions_protect ON secret_binding_versions;
CREATE TRIGGER secret_binding_versions_protect
    BEFORE UPDATE ON secret_binding_versions
    FOR EACH ROW EXECUTE FUNCTION protect_secret_binding_version();

CREATE TABLE IF NOT EXISTS secret_binding_deliveries (
    version_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 127),
    source_key text NOT NULL CHECK (
        length(source_key) BETWEEN 1 AND 253 AND source_key ~ '^[A-Za-z0-9._-]+$'
    ),
    kind text NOT NULL CHECK (kind IN ('environment','file')),
    environment_name text,
    file_path text,
    file_mode integer,
    PRIMARY KEY (version_id,ordinal),
    FOREIGN KEY (version_id,binding_id) REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT,
    CHECK (
        (kind='environment' AND environment_name ~ '^[A-Za-z_][A-Za-z0-9_]{0,252}$' AND file_path IS NULL AND file_mode IS NULL) OR
        (kind='file' AND environment_name IS NULL AND
         file_path ~ '^/var/run/secrets/kuberploy/[A-Za-z0-9._/-]+$' AND
         file_path !~ '/\\.?\\.?(/|$)' AND file_path !~ '//' AND
         file_mode IN (256,288))
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS secret_binding_delivery_environment_unique
    ON secret_binding_deliveries(version_id,environment_name) WHERE kind='environment';
CREATE UNIQUE INDEX IF NOT EXISTS secret_binding_delivery_file_unique
    ON secret_binding_deliveries(version_id,file_path) WHERE kind='file';

CREATE OR REPLACE FUNCTION reject_secret_binding_delivery_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'secret binding deliveries are immutable' USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS secret_binding_deliveries_immutable ON secret_binding_deliveries;
CREATE TRIGGER secret_binding_deliveries_immutable
    BEFORE UPDATE OR DELETE ON secret_binding_deliveries
    FOR EACH ROW EXECUTE FUNCTION reject_secret_binding_delivery_mutation();

CREATE TABLE IF NOT EXISTS secret_binding_idempotency (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    operation text NOT NULL CHECK (operation IN ('create','rotate')),
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    binding_id uuid NOT NULL REFERENCES secret_bindings(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'),
    request_fingerprint bytea NOT NULL CHECK (octet_length(request_fingerprint)=32),
    version_id uuid NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,operation,application_id,idempotency_key),
    FOREIGN KEY (version_id,binding_id) REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION reject_secret_binding_idempotency_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'secret binding idempotency records are permanent' USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS secret_binding_idempotency_immutable ON secret_binding_idempotency;
CREATE TRIGGER secret_binding_idempotency_immutable
    BEFORE UPDATE OR DELETE ON secret_binding_idempotency
    FOR EACH ROW EXECUTE FUNCTION reject_secret_binding_idempotency_mutation();

CREATE TABLE IF NOT EXISTS secret_binding_references (
    binding_id uuid NOT NULL REFERENCES secret_bindings(id) ON DELETE RESTRICT,
    version_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('git-current','current-release','retained-release')),
    reference_id text NOT NULL CHECK (
        length(reference_id) BETWEEN 1 AND 256 AND reference_id !~ '[[:cntrl:]]'
    ),
    revision text NOT NULL CHECK (
        revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64}|sha256:[0-9a-f]{64})$'
    ),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (binding_id,kind,reference_id),
    FOREIGN KEY (version_id,binding_id) REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS secret_binding_references_version_idx
    ON secret_binding_references(version_id,kind,reference_id);

CREATE OR REPLACE FUNCTION reject_secret_binding_reference_update()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'secret binding references cannot be rebound' USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS secret_binding_references_no_update ON secret_binding_references;
CREATE TRIGGER secret_binding_references_no_update
    BEFORE UPDATE ON secret_binding_references
    FOR EACH ROW EXECUTE FUNCTION reject_secret_binding_reference_update();

CREATE TABLE IF NOT EXISTS secret_binding_events (
    id uuid PRIMARY KEY,
    binding_id uuid NOT NULL REFERENCES secret_bindings(id) ON DELETE RESTRICT,
    version_id uuid,
    actor_id uuid REFERENCES users(id) ON DELETE RESTRICT,
    kind text NOT NULL CHECK (kind IN (
        'version-staging','version-awaiting-readiness','version-active','version-failed',
        'reference-added','reference-removed','binding-deleting','binding-deleted'
    )),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'),
    occurred_at timestamptz NOT NULL,
    published_at timestamptz,
    FOREIGN KEY (version_id,binding_id) REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT,
    CHECK (published_at IS NULL OR published_at>=occurred_at)
);
CREATE INDEX IF NOT EXISTS secret_binding_events_pending_idx
    ON secret_binding_events(occurred_at,id) WHERE published_at IS NULL;

CREATE OR REPLACE FUNCTION protect_secret_binding_event()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'secret binding events are permanent' USING ERRCODE='23514';
    END IF;
    IF ROW(NEW.id,NEW.binding_id,NEW.version_id,NEW.actor_id,NEW.kind,NEW.request_id,NEW.occurred_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.binding_id,OLD.version_id,OLD.actor_id,OLD.kind,OLD.request_id,OLD.occurred_at) OR
       (OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at) THEN
        RAISE EXCEPTION 'secret binding event is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS secret_binding_events_protect ON secret_binding_events;
CREATE TRIGGER secret_binding_events_protect
    BEFORE UPDATE OR DELETE ON secret_binding_events
    FOR EACH ROW EXECUTE FUNCTION protect_secret_binding_event();

-- Git provider verification and shadow indexing perform bounded external I/O,
-- so row locks cannot safely span the work. Add an expiring per-binding lease
-- with a monotonically increasing epoch. The epoch fences a stale process even
-- when it restarts with the same owner identity.
ALTER TABLE git_safety_poll_cursors
    ADD COLUMN lease_owner text,
    ADD COLUMN lease_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN lease_until timestamptz,
    ADD COLUMN reconciled_binding_updated_at timestamptz,
    ADD COLUMN last_error_code text NOT NULL DEFAULT '',
    ADD CONSTRAINT git_safety_poll_lease_epoch_valid CHECK (lease_epoch>=0),
    ADD CONSTRAINT git_safety_poll_lease_shape CHECK (
        (lease_owner IS NULL AND lease_until IS NULL) OR
        (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$' AND lease_epoch>0 AND lease_until>updated_at)
    ),
    ADD CONSTRAINT git_safety_poll_reconciled_time CHECK (
        reconciled_binding_updated_at IS NULL OR reconciled_binding_updated_at<=updated_at
    ),
    ADD CONSTRAINT git_safety_poll_error_code CHECK (
        last_error_code='' OR last_error_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    );

CREATE INDEX IF NOT EXISTS git_safety_poll_reconcile_due_idx
    ON git_safety_poll_cursors(lease_until,next_poll_at,binding_id);

CREATE OR REPLACE FUNCTION protect_git_reconciliation_lease_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'Git reconciliation lease epoch cannot move backwards'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND OLD.lease_owner IS NULL
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Git reconciliation acquisition must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND OLD.lease_owner IS NOT NULL
       AND (NEW.lease_owner<>OLD.lease_owner OR NEW.lease_epoch<>OLD.lease_epoch)
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Git reconciliation replacement must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS git_safety_poll_lease_epoch ON git_safety_poll_cursors;
CREATE TRIGGER git_safety_poll_lease_epoch
    BEFORE UPDATE ON git_safety_poll_cursors
    FOR EACH ROW EXECUTE FUNCTION protect_git_reconciliation_lease_epoch();

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

-- Metadata-only external-dns integration profiles. Provider credentials,
-- Secret data, API endpoints, arbitrary provider JSON, webhook configuration,
-- controller observations and rendered Kubernetes objects are deliberately
-- absent. Platform operators bind profiles to exact central environments.

CREATE OR REPLACE FUNCTION external_dns_domain_suffixes_valid(value jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    IF jsonb_typeof(value) <> 'array' OR jsonb_array_length(value) NOT BETWEEN 1 AND 64 THEN
        RETURN false;
    END IF;
    RETURN NOT EXISTS (
        SELECT 1
        FROM jsonb_array_elements_text(value) AS suffix(value)
        WHERE length(suffix.value) NOT BETWEEN 1 AND 253
           OR suffix.value <> lower(suffix.value)
           OR suffix.value ~ '[[:cntrl:]]'
           OR suffix.value !~ '^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$'
    ) AND (
        SELECT count(*) = count(DISTINCT suffix.value)
        FROM jsonb_array_elements_text(value) AS suffix(value)
    );
END;
$$;

CREATE TABLE IF NOT EXISTS external_dns_integrations (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (
        length(slug) BETWEEN 1 AND 63 AND
        slug ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    name text NOT NULL CHECK (
        length(name) BETWEEN 1 AND 100 AND
        name=btrim(name) AND name !~ '[[:cntrl:]]'
    ),
    mode text NOT NULL CHECK (mode IN ('managed','adopted')),
    provider_kind text NOT NULL CHECK (
        provider_kind IN ('aws','azure','cloudflare','google','rfc2136')
    ),
    txt_owner_id text NOT NULL UNIQUE CHECK (
        length(txt_owner_id) BETWEEN 1 AND 128 AND
        txt_owner_id ~ '^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$'
    ),
    allowed_domain_suffixes jsonb NOT NULL
        CHECK (external_dns_domain_suffixes_valid(allowed_domain_suffixes)),
    sync_policy text NOT NULL DEFAULT 'upsert-only'
        CHECK (sync_policy IN ('upsert-only','sync')),
    destructive_sync_confirmed boolean NOT NULL DEFAULT false,
    credential_secret_ref text,
    provider_config_ref text,
    egress_config_ref text,
    operator_profile_ref text,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK (updated_at >= created_at),
    CHECK (
        (sync_policy='upsert-only' AND NOT destructive_sync_confirmed) OR
        (sync_policy='sync' AND destructive_sync_confirmed)
    ),
    CHECK (
        (mode='managed' AND
         credential_secret_ref IS NOT NULL AND
         provider_config_ref IS NOT NULL AND
         egress_config_ref IS NOT NULL AND
         credential_secret_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         provider_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         egress_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         operator_profile_ref IS NULL) OR
        (mode='adopted' AND operator_profile_ref IS NOT NULL AND
         credential_secret_ref IS NULL AND
         provider_config_ref IS NULL AND egress_config_ref IS NULL AND
         operator_profile_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$')
    )
);

CREATE TABLE IF NOT EXISTS external_dns_integration_environments (
    integration_id uuid NOT NULL
        REFERENCES external_dns_integrations(id) ON DELETE CASCADE,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (integration_id,environment_id)
);
CREATE INDEX IF NOT EXISTS external_dns_integration_environments_environment_idx
    ON external_dns_integration_environments(environment_id,integration_id);

CREATE OR REPLACE FUNCTION protect_external_dns_integration_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.slug,NEW.txt_owner_id,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.slug,OLD.txt_owner_id,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'external-dns integration identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'external-dns integration time cannot move backwards' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS external_dns_integrations_identity ON external_dns_integrations;
CREATE TRIGGER external_dns_integrations_identity
    BEFORE UPDATE ON external_dns_integrations
    FOR EACH ROW EXECUTE FUNCTION protect_external_dns_integration_identity();

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

-- Durable, replay-safe projection of a verified source-build result into the
-- registry release/cache lifecycle. The build result remains authoritative in
-- build_attempts; this table is only the recoverable handoff state machine.

CREATE TABLE IF NOT EXISTS build_release_projections (
    attempt_id uuid PRIMARY KEY REFERENCES build_attempts(id) ON DELETE RESTRICT,
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','processing','succeeded','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 20),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    failure_code text NOT NULL DEFAULT '',
    release_id uuid,
    cache_generation_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL)),
    CHECK ((state='processing') = (lease_owner IS NOT NULL)),
    CHECK ((state IN ('succeeded','failed')) = (completed_at IS NOT NULL)),
    CHECK (state='succeeded' OR release_id IS NULL),
    CHECK (state<>'succeeded' OR failure_code='')
);

CREATE INDEX IF NOT EXISTS build_release_projections_work_idx
    ON build_release_projections(available_at,created_at,attempt_id)
    WHERE state IN ('pending','processing');

CREATE OR REPLACE FUNCTION enqueue_build_release_projection()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'succeeded' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.state = 'succeeded' THEN
        RETURN NEW;
    END IF;
    INSERT INTO build_release_projections(attempt_id,available_at,created_at,updated_at)
    VALUES(NEW.id,NEW.completed_at,NEW.completed_at,NEW.completed_at)
    ON CONFLICT(attempt_id) DO NOTHING;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS build_attempts_enqueue_release_projection ON build_attempts;
CREATE TRIGGER build_attempts_enqueue_release_projection
    AFTER INSERT OR UPDATE OF state ON build_attempts
    FOR EACH ROW EXECUTE FUNCTION enqueue_build_release_projection();

-- Existing successful attempts are made recoverable when upgrading an older
-- installation. Deterministic release/cache identities make replay harmless.
INSERT INTO build_release_projections(attempt_id,available_at,created_at,updated_at)
SELECT id,completed_at,completed_at,completed_at
FROM build_attempts
WHERE state='succeeded' AND completed_at IS NOT NULL
ON CONFLICT(attempt_id) DO NOTHING;

-- Managed-registry observation and offline maintenance both perform bounded
-- external I/O.  These cursors keep PostgreSQL transactions short while a
-- monotonically increasing epoch fences workers whose lease expired.
CREATE TABLE registry_runtime_observation_cursors (
    registry_target_id uuid PRIMARY KEY REFERENCES registry_targets(id) ON DELETE CASCADE,
    completed_revision bigint NOT NULL DEFAULT 0 CHECK (completed_revision>=0),
    completed_at timestamptz,
    next_observe_at timestamptz NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures>=0),
    last_error_code text NOT NULL DEFAULT '' CHECK (
        last_error_code='' OR last_error_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text,
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    updated_at timestamptz NOT NULL,
    CONSTRAINT registry_runtime_observation_lease_shape CHECK (
        (lease_owner IS NULL AND lease_until IS NULL) OR
        (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$'
         AND lease_epoch>0 AND lease_until>updated_at)
    ),
    CONSTRAINT registry_runtime_observation_completed_shape CHECK (
        (completed_revision=0 AND completed_at IS NULL) OR
        (completed_revision>0 AND completed_at IS NOT NULL)
    )
);

CREATE INDEX registry_runtime_observation_due_idx
    ON registry_runtime_observation_cursors(lease_until,next_observe_at,registry_target_id);

CREATE OR REPLACE FUNCTION protect_registry_runtime_observation_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'Registry observation lease epoch cannot move backwards'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL
       AND (OLD.lease_owner IS NULL OR NEW.lease_owner<>OLD.lease_owner OR NEW.lease_epoch<>OLD.lease_epoch)
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Registry observation acquisition must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    IF NEW.completed_revision<OLD.completed_revision THEN
        RAISE EXCEPTION 'Registry observation revision cannot move backwards'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER registry_runtime_observation_epoch
    BEFORE UPDATE ON registry_runtime_observation_cursors
    FOR EACH ROW EXECUTE FUNCTION protect_registry_runtime_observation_epoch();

-- One row represents one immutable cleanup execution key.  The partial unique
-- index serializes maintenance for the complete registry target.  An expired
-- row must be reclaimed and restored/released before another execution can
-- enter maintenance.
CREATE TABLE registry_runtime_maintenance_executions (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    execution_key text NOT NULL,
    plan_id uuid NOT NULL REFERENCES registry_cleanup_plans(id) ON DELETE RESTRICT,
    candidate_set_digest text NOT NULL,
    state text NOT NULL DEFAULT 'acquired' CHECK (
        state IN ('acquired','entered','checkpointed','sweeping','swept','restored','released','failed')
    ),
    maintenance_mode text CHECK (maintenance_mode IS NULL OR maintenance_mode IN ('read_only','stopped')),
    deployment_uid text NOT NULL DEFAULT '',
    original_replicas integer CHECK (original_replicas IS NULL OR original_replicas>=0),
    checkpoint_revision text NOT NULL DEFAULT '',
    checkpoint_digest text NOT NULL DEFAULT '',
    checkpoint_observed_at timestamptz,
    sweep_job_uid text NOT NULL DEFAULT '',
    lease_owner text NOT NULL,
    lease_epoch bigint NOT NULL CHECK (lease_epoch>0),
    lease_until timestamptz NOT NULL,
    restored_at timestamptz,
    released_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY(registry_target_id,execution_key),
    CONSTRAINT registry_runtime_maintenance_digest_shape CHECK (
        execution_key ~ '^sha256:[0-9a-f]{64}$' AND
        candidate_set_digest ~ '^sha256:[0-9a-f]{64}$' AND
        (checkpoint_digest='' OR checkpoint_digest ~ '^sha256:[0-9a-f]{64}$')
    ),
    CONSTRAINT registry_runtime_maintenance_identity_shape CHECK (
        plan_id::text ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$' AND
        lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$' AND
        deployment_uid !~ '[[:space:][:cntrl:]]' AND
        sweep_job_uid !~ '[[:space:][:cntrl:]]'
    ),
    CONSTRAINT registry_runtime_maintenance_lease_shape CHECK (lease_until>updated_at),
    CONSTRAINT registry_runtime_maintenance_checkpoint_shape CHECK (
        (checkpoint_revision='' AND checkpoint_digest='' AND checkpoint_observed_at IS NULL) OR
        (checkpoint_revision<>'' AND checkpoint_digest<>'' AND checkpoint_observed_at IS NOT NULL)
    ),
    CONSTRAINT registry_runtime_maintenance_restore_shape CHECK (
        released_at IS NULL OR (restored_at IS NOT NULL AND released_at>=restored_at)
    )
);

CREATE UNIQUE INDEX registry_runtime_maintenance_target_exclusive_idx
    ON registry_runtime_maintenance_executions(registry_target_id)
    WHERE released_at IS NULL;

CREATE INDEX registry_runtime_maintenance_lease_idx
    ON registry_runtime_maintenance_executions(lease_until,registry_target_id,execution_key)
    WHERE released_at IS NULL;

-- A completed sweep receipt is append-only and is the only database fact that
-- permits replay without starting another deterministic Kubernetes Job.
CREATE TABLE registry_runtime_gc_sweep_receipts (
    registry_target_id uuid NOT NULL,
    execution_key text NOT NULL,
    plan_id uuid NOT NULL,
    candidate_set_digest text NOT NULL,
    checkpoint_revision text NOT NULL,
    provider_sweep_id text NOT NULL,
    helper_job_uid text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY(registry_target_id,execution_key),
    FOREIGN KEY(registry_target_id,execution_key)
        REFERENCES registry_runtime_maintenance_executions(registry_target_id,execution_key)
        ON DELETE RESTRICT,
    CONSTRAINT registry_runtime_gc_receipt_digest_shape CHECK (
        execution_key ~ '^sha256:[0-9a-f]{64}$' AND
        candidate_set_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    CONSTRAINT registry_runtime_gc_receipt_identity_shape CHECK (
        plan_id::text ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$' AND
        checkpoint_revision ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$' AND
        provider_sweep_id ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$' AND
        helper_job_uid !~ '[[:space:][:cntrl:]]'
    ),
    CONSTRAINT registry_runtime_gc_receipt_time_shape CHECK (
        completed_at>=started_at AND created_at>=completed_at
    )
);

CREATE OR REPLACE FUNCTION validate_registry_runtime_maintenance_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target_mode text;
    plan_target text;
BEGIN
    SELECT mode INTO target_mode FROM registry_targets WHERE id=NEW.registry_target_id;
    SELECT registry_target_id INTO plan_target FROM registry_cleanup_plans WHERE id=NEW.plan_id;
    IF target_mode IS DISTINCT FROM 'managed' OR plan_target IS DISTINCT FROM NEW.registry_target_id THEN
        RAISE EXCEPTION 'Registry maintenance is restricted to its managed target and cleanup plan'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER registry_runtime_maintenance_target
    BEFORE INSERT OR UPDATE ON registry_runtime_maintenance_executions
    FOR EACH ROW EXECUTE FUNCTION validate_registry_runtime_maintenance_target();

CREATE OR REPLACE FUNCTION protect_registry_runtime_maintenance_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.registry_target_id<>OLD.registry_target_id OR
       NEW.execution_key<>OLD.execution_key OR NEW.plan_id<>OLD.plan_id OR
       NEW.candidate_set_digest<>OLD.candidate_set_digest THEN
        RAISE EXCEPTION 'Registry maintenance immutable identity changed'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_epoch<OLD.lease_epoch OR
       (NEW.lease_owner<>OLD.lease_owner OR NEW.lease_epoch<>OLD.lease_epoch)
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Registry maintenance acquisition must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW IS DISTINCT FROM OLD AND NOT (
       NEW.state='acquired' AND NEW.lease_epoch=OLD.lease_epoch+1 AND
       NEW.lease_owner<>'' AND NEW.lease_until>NEW.updated_at AND
       NEW.maintenance_mode IS NULL AND NEW.deployment_uid='' AND
       NEW.original_replicas IS NULL AND NEW.checkpoint_revision='' AND
       NEW.checkpoint_digest='' AND NEW.checkpoint_observed_at IS NULL AND
       NEW.sweep_job_uid='' AND NEW.restored_at IS NULL AND NEW.released_at IS NULL
    ) THEN
        RAISE EXCEPTION 'Released registry maintenance may only be reacquired for a fresh replay checkpoint'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER registry_runtime_maintenance_epoch
    BEFORE UPDATE ON registry_runtime_maintenance_executions
    FOR EACH ROW EXECUTE FUNCTION protect_registry_runtime_maintenance_epoch();

CREATE OR REPLACE FUNCTION protect_registry_runtime_gc_receipt()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Registry garbage-collection receipts are immutable'
        USING ERRCODE='23514';
END;
$$;

CREATE TRIGGER registry_runtime_gc_receipt_immutable
    BEFORE UPDATE OR DELETE ON registry_runtime_gc_sweep_receipts
    FOR EACH ROW EXECUTE FUNCTION protect_registry_runtime_gc_receipt();

-- Fenced scheduler state for the namespace-wide Argo Application observer.
-- Kubernetes resourceVersion is an opaque repair cursor; correctness comes
-- from the monotonically increasing lease epoch, not lexical RV comparison.
-- One logical application may have one deployment in several environments,
-- so the Argo Application and its observation are deployment-scoped. The old
-- application-only key would collide inside a shared Argo namespace.
ALTER TABLE deployments
    ADD CONSTRAINT deployments_id_application_environment_unique
    UNIQUE (id,application_id,environment_id);

ALTER TABLE argo_application_observations
    ADD COLUMN deployment_id uuid;

UPDATE argo_application_observations observation
SET deployment_id=(
    SELECT deployment.id
    FROM deployments deployment
    WHERE deployment.application_id=observation.application_id
      AND deployment.environment_id=observation.environment_id
);

ALTER TABLE argo_application_observations
    ALTER COLUMN deployment_id SET NOT NULL;

ALTER TABLE argo_application_observations
    DROP CONSTRAINT argo_application_observations_pkey;

ALTER TABLE argo_application_observations
    ADD CONSTRAINT argo_application_observations_pkey PRIMARY KEY (deployment_id),
    ADD CONSTRAINT argo_application_observations_application_environment_unique UNIQUE (application_id,environment_id),
    ADD CONSTRAINT argo_application_observations_deployment_identity_fk
        FOREIGN KEY (deployment_id,application_id,environment_id)
        REFERENCES deployments(id,application_id,environment_id) ON DELETE CASCADE;

CREATE TABLE IF NOT EXISTS argo_observation_runtime (
    argo_namespace text PRIMARY KEY
        CHECK (argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'),
    lease_owner text NOT NULL DEFAULT ''
        CHECK (lease_owner='' OR lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$'),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    snapshot_resource_version text NOT NULL DEFAULT ''
        CHECK (length(snapshot_resource_version)<=128
            AND position(chr(10) IN snapshot_resource_version)=0
            AND position(chr(13) IN snapshot_resource_version)=0),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 32),
    last_failure_code text NOT NULL DEFAULT ''
        CHECK (last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'),
    next_poll_at timestamptz NOT NULL,
    last_completed_at timestamptz,
    updated_at timestamptz NOT NULL,
    CHECK ((lease_owner='' AND lease_until IS NULL) OR (lease_owner<>'' AND lease_epoch>0 AND lease_until IS NOT NULL)),
    CHECK ((consecutive_failures=0 AND last_failure_code='') OR (consecutive_failures>0 AND last_failure_code<>'')),
    CHECK (last_completed_at IS NULL OR last_completed_at<=updated_at)
);

CREATE INDEX IF NOT EXISTS argo_observation_runtime_due_idx
    ON argo_observation_runtime(next_poll_at,argo_namespace);

-- Make Git transport authority explicit. GitHub App bindings use short-lived,
-- repository-scoped tokens and therefore must not name a Kubernetes credential
-- Secret. Existing operator-created bindings retain their legacy Secret mode.
ALTER TABLE git_repository_bindings
    ADD COLUMN IF NOT EXISTS credential_mode text NOT NULL DEFAULT 'legacy-secret';

ALTER TABLE git_repository_bindings
    DROP CONSTRAINT IF EXISTS git_repository_bindings_credential_secret_name_check;
ALTER TABLE git_repository_bindings
    DROP CONSTRAINT IF EXISTS git_repository_bindings_credential_mode_check;
ALTER TABLE git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_credential_mode_check CHECK (
        (credential_mode='legacy-secret'
            AND credential_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$')
        OR (credential_mode='github-app' AND credential_secret_name='')
    );

-- One authoritative desired-state binding per scope. Multiple repositories for
-- one environment would make reads and Argo desired revisions ambiguous.
CREATE UNIQUE INDEX IF NOT EXISTS git_repository_bindings_environment_authority
    ON git_repository_bindings(environment_id) WHERE kind='environment';
CREATE UNIQUE INDEX IF NOT EXISTS git_repository_bindings_platform_authority
    ON git_repository_bindings(cluster_id) WHERE kind='platform';

CREATE OR REPLACE FUNCTION protect_git_binding_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.kind,NEW.scope_id,NEW.project_id,NEW.environment_id,NEW.cluster_id,
           NEW.provider,NEW.installation_id,NEW.repository_id,NEW.repository_owner,
           NEW.repository_name,NEW.target_ref,NEW.path_prefix,NEW.credential_mode,
           NEW.credential_secret_name)
       IS DISTINCT FROM
       ROW(OLD.kind,OLD.scope_id,OLD.project_id,OLD.environment_id,OLD.cluster_id,
           OLD.provider,OLD.installation_id,OLD.repository_id,OLD.repository_owner,
           OLD.repository_name,OLD.target_ref,OLD.path_prefix,OLD.credential_mode,
           OLD.credential_secret_name) THEN
        RAISE EXCEPTION 'Git binding identity is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Public managed-registry capability requires a fresh worker that implements
-- the exact observer/executor contract and operator configuration. Each
-- worker identity owns an epoch-fenced lease so a restarted process cannot be
-- kept ready by a stale heartbeat.
CREATE TABLE managed_registry_runtime_readiness (
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE CASCADE,
    worker_id text NOT NULL CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch > 0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    PRIMARY KEY (registry_target_id,worker_id),
    CHECK (observed_at >= started_at AND lease_until > observed_at)
);

CREATE INDEX managed_registry_runtime_readiness_match_idx
    ON managed_registry_runtime_readiness(
        registry_target_id,contract_version,config_digest,observed_at DESC
    );

CREATE OR REPLACE FUNCTION validate_managed_registry_runtime_readiness_target()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM registry_targets
        WHERE id=NEW.registry_target_id AND mode='managed'
    ) THEN
        RAISE EXCEPTION 'Registry runtime readiness requires its exact managed target'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER managed_registry_runtime_readiness_target
    BEFORE INSERT OR UPDATE ON managed_registry_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION validate_managed_registry_runtime_readiness_target();

CREATE OR REPLACE FUNCTION protect_managed_registry_runtime_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.registry_target_id<>OLD.registry_target_id OR NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'Registry runtime readiness identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch<OLD.worker_epoch OR NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Registry runtime readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.started_at<>OLD.started_at OR
        NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Registry runtime readiness lease identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER managed_registry_runtime_readiness_epoch
    BEFORE UPDATE ON managed_registry_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_managed_registry_runtime_readiness_epoch();

-- The production GitOps write path persists an immutable, authorized command
-- in the same transaction as its deployment operation. Git/network work is
-- retried outside that transaction; the operation trailer and these exact
-- bytes make a successful push recoverable after a process crash.
CREATE TABLE git_deployment_write_commands (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    deployment_id uuid NOT NULL REFERENCES deployments(id) DEFERRABLE INITIALLY DEFERRED,
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    target_ref text NOT NULL,
    path text NOT NULL CHECK (
        path='tenants/'||project_id::text||'/environments/'||environment_id::text||
             '/apps/'||application_id::text||'/app.yaml'
    ),
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    precondition text NOT NULL CHECK (precondition IN ('match-etag','create-if-absent')),
    expected_etag text NOT NULL DEFAULT '' CHECK (
        (precondition='match-etag' AND expected_etag ~ '^"sha256:[0-9a-f]{64}"$') OR
        (precondition='create-if-absent' AND expected_etag='')
    ),
    chart_digest text NOT NULL CHECK (chart_digest ~ '^sha256:[0-9a-f]{64}$'),
    policy_version text NOT NULL CHECK (
        length(policy_version) BETWEEN 1 AND 128 AND policy_version !~ E'[\\x00\\r\\n]'
    ),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 262144),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND message !~ E'[\\x00\\r]'
    ),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','git-committed','indexed')),
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    indexed_generation bigint NOT NULL DEFAULT 0 CHECK (indexed_generation>=0),
    indexed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (binding_id,target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at),
    CHECK (
        (state='pending' AND committed_revision='' AND committed_at IS NULL
            AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='git-committed' AND committed_revision<>'' AND committed_at IS NOT NULL
            AND committed_at>=created_at AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='indexed' AND committed_revision<>'' AND committed_at IS NOT NULL
            AND committed_at>=created_at AND indexed_generation>0 AND indexed_at IS NOT NULL
            AND indexed_at>=committed_at)
    )
);

CREATE INDEX git_deployment_write_commands_binding_state_idx
    ON git_deployment_write_commands(binding_id,state,created_at,operation_id);
CREATE INDEX git_deployment_write_commands_committed_idx
    ON git_deployment_write_commands(binding_id,committed_revision)
    WHERE state IN ('git-committed','indexed');

CREATE OR REPLACE FUNCTION protect_git_deployment_write_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.operation_id,NEW.deployment_id,NEW.actor_id,NEW.binding_id,
           NEW.project_id,NEW.environment_id,NEW.application_id,NEW.target_ref,
           NEW.path,NEW.base_revision,NEW.precondition,NEW.expected_etag,
           NEW.chart_digest,NEW.policy_version,NEW.content,NEW.content_sha256,
           NEW.message,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.deployment_id,OLD.actor_id,OLD.binding_id,
           OLD.project_id,OLD.environment_id,OLD.application_id,OLD.target_ref,
           OLD.path,OLD.base_revision,OLD.precondition,OLD.expected_etag,
           OLD.chart_digest,OLD.policy_version,OLD.content,OLD.content_sha256,
           OLD.message,OLD.created_at) THEN
        RAISE EXCEPTION 'Git deployment write command identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF (OLD.state='git-committed' AND NEW.state='pending') OR
       (OLD.state='indexed' AND NEW.state<>'indexed') OR
       NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'Git deployment write command state cannot regress'
            USING ERRCODE='23514';
    END IF;
    IF OLD.committed_revision<>'' AND
       (NEW.committed_revision<>OLD.committed_revision OR NEW.committed_at<>OLD.committed_at) THEN
        RAISE EXCEPTION 'Git deployment write result is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER git_deployment_write_commands_protect
    BEFORE UPDATE ON git_deployment_write_commands
    FOR EACH ROW EXECUTE FUNCTION protect_git_deployment_write_command();

-- A projection-backed preview binds its token to the exact indexed Git
-- snapshot. Legacy previews keep every Git field NULL; the two modes cannot be
-- mixed or partially populated.
ALTER TABLE deployment_config_previews
    ADD COLUMN git_binding_id uuid REFERENCES git_repository_bindings(id) ON DELETE CASCADE,
    ADD COLUMN git_base_revision text,
    ADD COLUMN git_path text,
    ADD COLUMN git_expected_etag text,
    ADD COLUMN git_chart_digest text,
    ADD COLUMN git_policy_version text,
    ADD CONSTRAINT deployment_config_previews_git_shape CHECK (
        (git_binding_id IS NULL AND git_base_revision IS NULL AND git_path IS NULL
            AND git_expected_etag IS NULL AND git_chart_digest IS NULL
            AND git_policy_version IS NULL) OR
        (git_binding_id IS NOT NULL
            AND git_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
            AND git_path IS NOT NULL AND length(git_path) BETWEEN 1 AND 1024
            AND git_path !~ '(^/|/\.\.?(/|$)|//|\\)'
            AND git_expected_etag ~ '^"sha256:[0-9a-f]{64}"$'
            AND git_chart_digest ~ '^sha256:[0-9a-f]{64}$'
            AND length(git_policy_version) BETWEEN 1 AND 128
            AND git_policy_version !~ E'[\\x00\\r\\n]')
    );

-- Feature-specific audit streams share the canonical timeline instead of
-- maintaining write-only shadow tables. This trigger retains the strong
-- revision/digest/actor checks those tables previously provided and makes the
-- four event families immutable without preventing test/retention handling of
-- older generic event families.
CREATE OR REPLACE FUNCTION protect_managed_audit_event()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    event_type text;
    revision_value bigint;
BEGIN
    IF TG_OP='DELETE' THEN event_type := OLD.target_type; ELSE event_type := NEW.target_type; END IF;
    IF TG_OP <> 'INSERT' THEN
        IF OLD.target_type IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy')
           OR (TG_OP='UPDATE' AND NEW.target_type IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy')) THEN
            RAISE EXCEPTION 'managed audit events are immutable' USING ERRCODE='23514';
        END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
    END IF;
    IF event_type NOT IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy') THEN
        RETURN NEW;
    END IF;
    IF NEW.detail->>'revision' !~ '^[1-9][0-9]*$' THEN
        RAISE EXCEPTION 'managed audit revision is invalid' USING ERRCODE='23514';
    END IF;
    revision_value := (NEW.detail->>'revision')::bigint;
    IF event_type IN ('scheduling-profile','middleware-profile') THEN
        IF NOT (NEW.detail ?& ARRAY['revision','idempotencyKey','specDigest','assignmentsDigest'])
           OR NEW.detail - ARRAY['revision','idempotencyKey','specDigest','assignmentsDigest'] <> '{}'::jsonb
           OR NEW.detail->>'idempotencyKey' !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
           OR NEW.detail->>'specDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NEW.detail->>'assignmentsDigest' !~ '^sha256:[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'managed profile audit detail is invalid' USING ERRCODE='23514';
        END IF;
        IF event_type='scheduling-profile' AND
           (NEW.action NOT IN ('scheduling-profile.create','scheduling-profile.revise','scheduling-profile.deactivate') OR
            NOT EXISTS (SELECT 1 FROM scheduling_profile_revisions r
                WHERE r.profile_id=NEW.target_id AND r.revision=revision_value
                  AND r.spec_digest=NEW.detail->>'specDigest'
                  AND r.assignments_digest=NEW.detail->>'assignmentsDigest')) THEN
            RAISE EXCEPTION 'scheduling audit authority mismatch' USING ERRCODE='23514';
        END IF;
        IF event_type='middleware-profile' AND
           (NEW.action NOT IN ('middleware-profile.create','middleware-profile.revise','middleware-profile.clone','middleware-profile.deactivate') OR
            NOT EXISTS (SELECT 1 FROM middleware_profile_revisions r
                WHERE r.profile_id=NEW.target_id AND r.revision=revision_value
                  AND r.spec_digest=NEW.detail->>'specDigest'
                  AND r.assignments_digest=NEW.detail->>'assignmentsDigest')) THEN
            RAISE EXCEPTION 'middleware audit authority mismatch' USING ERRCODE='23514';
        END IF;
    ELSIF event_type='certificate-issuer-profile' THEN
        IF NEW.action NOT IN ('certificate-issuer-profile.create','certificate-issuer-profile.revise','certificate-issuer-profile.deactivate')
           OR NOT (NEW.detail ?& ARRAY['revision','idempotencyKey','specDigest'])
           OR NEW.detail - ARRAY['revision','idempotencyKey','specDigest'] <> '{}'::jsonb
           OR NEW.detail->>'idempotencyKey' !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
           OR NEW.detail->>'specDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin')
           OR NOT EXISTS (SELECT 1 FROM cert_manager_issuer_profile_revisions r
                WHERE r.profile_id=NEW.target_id AND r.revision=revision_value
                  AND r.spec_digest=NEW.detail->>'specDigest') THEN
            RAISE EXCEPTION 'certificate issuer audit authority mismatch' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.action NOT IN ('auto-deploy-policy.create','auto-deploy-policy.revise','auto-deploy-policy.enable','auto-deploy-policy.disable')
           OR NOT (NEW.detail ?& ARRAY['revision','serviceActorId','templateDigest'])
           OR NEW.detail - ARRAY['revision','serviceActorId','templateDigest'] <> '{}'::jsonb
           OR NEW.detail->>'serviceActorId' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
           OR NEW.detail->>'templateDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NOT EXISTS (SELECT 1 FROM auto_deploy_policy_revisions r
                WHERE r.policy_id=NEW.target_id AND r.revision=revision_value
                  AND r.service_actor_id::text=NEW.detail->>'serviceActorId'
                  AND r.template_digest=NEW.detail->>'templateDigest') THEN
            RAISE EXCEPTION 'auto-deploy audit authority mismatch' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER managed_audit_events_protect
    BEFORE INSERT OR UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION protect_managed_audit_event();

-- Public git/gitops capability requires a fresh worker lease matching the
-- complete non-secret runtime digest and this exact writer/indexer contract.
CREATE TABLE git_projection_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    github_app_id bigint NOT NULL CHECK (github_app_id>0),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX git_projection_runtime_readiness_match_idx
    ON git_projection_runtime_readiness(
        contract_version,config_digest,github_app_id,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_git_projection_runtime_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Git projection readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.github_app_id<>OLD.github_app_id OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Git projection readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER git_projection_runtime_readiness_epoch
    BEFORE UPDATE ON git_projection_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_git_projection_runtime_readiness_epoch();

-- Runtime-secret reconciliation is deliberately metadata-only. The worker
-- observes strict SealedSecret resources by exact namespace/name and never
-- receives or persists plaintext, ciphertext, Kubernetes Secret data, or
-- base64 material.
CREATE TABLE secret_binding_runtime_reconciliations (
    version_id uuid PRIMARY KEY,
    binding_id uuid NOT NULL,
    runtime_state text NOT NULL DEFAULT 'awaiting'
        CHECK (runtime_state IN ('awaiting','ready','failed')),
    next_attempt_at timestamptz NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0
        CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_until timestamptz,
    worker_contract text CHECK (
        worker_contract IS NULL OR
        (length(worker_contract) BETWEEN 8 AND 64 AND
         worker_contract ~ '^[a-z][a-z0-9.-]{7,63}$')
    ),
    worker_config_digest text CHECK (
        worker_config_digest IS NULL OR
        worker_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (version_id,binding_id),
    FOREIGN KEY (version_id,binding_id)
        REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT,
    CHECK (updated_at >= created_at AND next_attempt_at >= created_at),
    CHECK (
        (runtime_state='awaiting' AND completed_at IS NULL) OR
        (runtime_state IN ('ready','failed') AND completed_at IS NOT NULL AND
         completed_at >= created_at)
    ),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND
         worker_contract IS NULL AND worker_config_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract IS NOT NULL AND worker_config_digest IS NOT NULL AND
         lease_epoch > 0 AND lease_until > updated_at)
    ),
    CHECK ((last_failure_code='') = (consecutive_failures=0))
);

CREATE INDEX secret_binding_runtime_reconcile_due_idx
    ON secret_binding_runtime_reconciliations(next_attempt_at,version_id)
    WHERE runtime_state='awaiting';

CREATE OR REPLACE FUNCTION validate_secret_binding_runtime_reconciliation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_provider text;
BEGIN
    SELECT provider INTO durable_provider
      FROM secret_binding_versions
     WHERE id=NEW.version_id AND binding_id=NEW.binding_id;
    IF durable_provider IS DISTINCT FROM 'sealed-secrets' THEN
        RAISE EXCEPTION 'Runtime reconciliation requires an exact SealedSecret version'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.version_id,NEW.binding_id,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.version_id,OLD.binding_id,OLD.created_at) THEN
            RAISE EXCEPTION 'Runtime reconciliation identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Runtime reconciliation epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) AND
           NEW.lease_owner IS NOT NULL THEN
            RAISE EXCEPTION 'Runtime reconciliation lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.runtime_state<>'awaiting' AND NEW.runtime_state<>OLD.runtime_state THEN
            RAISE EXCEPTION 'Runtime reconciliation terminal state is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.runtime_state='awaiting' AND NEW.runtime_state NOT IN ('awaiting','ready','failed') THEN
            RAISE EXCEPTION 'Runtime reconciliation transition is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Runtime reconciliation time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER secret_binding_runtime_reconcile_validate
    BEFORE INSERT OR UPDATE ON secret_binding_runtime_reconciliations
    FOR EACH ROW EXECUTE FUNCTION validate_secret_binding_runtime_reconciliation();

-- Backfill versions staged before this runtime migration. External Secrets is
-- intentionally excluded: production reconciliation is strict Sealed Secrets.
INSERT INTO secret_binding_runtime_reconciliations(
    version_id,binding_id,next_attempt_at,created_at,updated_at
)
SELECT id,binding_id,updated_at,updated_at,updated_at
  FROM secret_binding_versions
 WHERE provider='sealed-secrets' AND state='awaiting-readiness'
ON CONFLICT (version_id) DO NOTHING;

-- A public runtime-secret capability requires a fresh worker implementing the
-- exact observer contract and operator-owned namespace/key/certificate config.
CREATE TABLE runtime_secret_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch > 0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (
        config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    fingerprint_key_id text NOT NULL CHECK (
        length(fingerprint_key_id) BETWEEN 1 AND 128 AND
        fingerprint_key_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
    ),
    sealing_key_fingerprint text NOT NULL CHECK (
        sealing_key_fingerprint ~ '^sha256:[0-9a-f]{64}$'
    ),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at >= started_at AND lease_until > observed_at)
);

CREATE INDEX runtime_secret_runtime_readiness_match_idx
    ON runtime_secret_runtime_readiness(
        contract_version,config_digest,fingerprint_key_id,
        sealing_key_fingerprint,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_runtime_secret_runtime_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'Runtime-secret worker identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch<OLD.worker_epoch OR NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Runtime-secret readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.fingerprint_key_id<>OLD.fingerprint_key_id OR
        NEW.sealing_key_fingerprint<>OLD.sealing_key_fingerprint OR
        NEW.started_at<>OLD.started_at OR
        NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Runtime-secret readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER runtime_secret_runtime_readiness_epoch
    BEFORE UPDATE ON runtime_secret_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_runtime_secret_runtime_readiness_epoch();

-- Protected Argo desired state is written only to the operator-owned platform
-- Git binding. These rows are immutable commands, not a second desired-state
-- authority: the exact bytes become authoritative only after the hardened Git
-- writer has pushed and provider-verified the protected ref.
ALTER TABLE git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_id_cluster_unique UNIQUE (id,cluster_id);

CREATE TABLE argo_desired_state_commands (
    id uuid PRIMARY KEY,
    generation bigint NOT NULL CHECK (generation>0),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL,
    environment_target_ref text NOT NULL,
    environment_revision text NOT NULL CHECK (
        environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    path text NOT NULL CHECK (
        path='clusters/'||cluster_id::text||'/argocd/environments/'||
             environment_id::text||'.yaml'
    ),
    argo_namespace text NOT NULL CHECK (
        argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    destination_namespace text NOT NULL CHECK (
        destination_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    argo_project text NOT NULL CHECK (
        argo_project ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    base_revision text NOT NULL CHECK (
        base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR
        write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    precondition text NOT NULL CHECK (
        precondition IN ('match-etag','create-if-absent')
    ),
    expected_etag text NOT NULL DEFAULT '' CHECK (
        (precondition='match-etag' AND
         expected_etag ~ '^"sha256:[0-9a-f]{64}"$') OR
        (precondition='create-if-absent' AND expected_etag='')
    ),
    catalog_digest text NOT NULL CHECK (
        catalog_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    chart_repository text NOT NULL CHECK (
        length(chart_repository) BETWEEN 7 AND 512 AND
        chart_repository ~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$'
    ),
    chart_name text NOT NULL CHECK (chart_name='kuberploy-runtime'),
    chart_version text NOT NULL CHECK (
        length(chart_version) BETWEEN 5 AND 64 AND
        chart_version ~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'
    ),
    chart_digest text NOT NULL CHECK (
        chart_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    renderer_image text NOT NULL CHECK (
        length(renderer_image) BETWEEN 10 AND 512 AND
        renderer_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
    ),
    chart_digest_enforcement text NOT NULL CHECK (
        chart_digest_enforcement IN ('unavailable','native-oci-digest-v1')
    ),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 262144),
    content_sha256 text NOT NULL CHECK (
        content_sha256 ~ '^sha256:[0-9a-f]{64}$'
    ),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND message !~ E'[\\x00\\r]'
    ),
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending','claimed','git-committed','verified',
                  'blocked-prerequisite','failed','superseded')
    ),
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR
        committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    verified_at timestamptz,
    next_attempt_at timestamptz NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (
        consecutive_failures BETWEEN 0 AND 30
    ),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    worker_contract text CHECK (
        worker_contract IS NULL OR
        (length(worker_contract) BETWEEN 8 AND 64 AND
         worker_contract ~ '^[a-z][a-z0-9.-]{7,63}$')
    ),
    worker_config_digest text CHECK (
        worker_config_digest IS NULL OR
        worker_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (environment_id,generation),
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,platform_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,environment_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id)
        ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND next_attempt_at>=created_at),
    CHECK (
        (write_base_revision='' AND write_base_observed_at IS NULL) OR
        (write_base_revision<>'' AND write_base_observed_at IS NOT NULL AND
         write_base_observed_at>=created_at AND
         write_base_observed_at<=updated_at)
    ),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND
         worker_contract IS NULL AND worker_config_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract IS NOT NULL AND worker_config_digest IS NOT NULL AND
         lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='pending' AND committed_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND completed_at IS NULL AND lease_owner IS NULL) OR
        (state='claimed' AND committed_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND completed_at IS NULL AND lease_owner IS NOT NULL) OR
        (state='git-committed' AND write_base_revision<>'' AND
         committed_revision<>'' AND
         committed_at IS NOT NULL AND committed_at>=created_at AND
         verified_at IS NULL AND completed_at IS NULL) OR
        (state='verified' AND write_base_revision<>'' AND
         committed_revision<>'' AND
         committed_at IS NOT NULL AND verified_at IS NOT NULL AND
         verified_at>=committed_at AND completed_at=verified_at AND
         lease_owner IS NULL) OR
        (state IN ('blocked-prerequisite','superseded') AND
         write_base_revision='' AND committed_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         completed_at IS NOT NULL AND completed_at>=created_at AND
         lease_owner IS NULL) OR
        (state='failed' AND
         committed_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND completed_at IS NOT NULL AND
         completed_at>=created_at AND lease_owner IS NULL)
    ),
    CHECK (
        (chart_digest_enforcement='unavailable' AND
         state='blocked-prerequisite') OR
        (chart_digest_enforcement='native-oci-digest-v1' AND
         state<>'blocked-prerequisite')
    )
);

CREATE UNIQUE INDEX argo_desired_state_commands_environment_live_idx
    ON argo_desired_state_commands(environment_id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE UNIQUE INDEX argo_desired_state_commands_binding_claim_idx
    ON argo_desired_state_commands(platform_binding_id)
    WHERE lease_owner IS NOT NULL;
CREATE INDEX argo_desired_state_commands_due_idx
    ON argo_desired_state_commands(next_attempt_at,created_at,id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE INDEX argo_desired_state_commands_status_idx
    ON argo_desired_state_commands(environment_id,generation DESC);

CREATE OR REPLACE FUNCTION validate_argo_desired_state_command()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    platform_kind text;
    platform_mode text;
    platform_ref text;
    platform_cluster uuid;
    platform_state text;
    platform_head text;
    environment_kind text;
    environment_ref text;
    environment_project uuid;
    bound_environment uuid;
    environment_state text;
    environment_head text;
    environment_indexed text;
    environment_projection_generation bigint;
BEGIN
    SELECT kind,credential_mode,target_ref,cluster_id,state,target_head_revision
      INTO platform_kind,platform_mode,platform_ref,platform_cluster,
           platform_state,platform_head
      FROM git_repository_bindings
     WHERE id=NEW.platform_binding_id;
    IF platform_kind IS DISTINCT FROM 'platform' OR
       platform_mode IS DISTINCT FROM 'github-app' OR
       platform_ref IS DISTINCT FROM NEW.platform_target_ref OR
       platform_cluster IS DISTINCT FROM NEW.cluster_id THEN
        RAISE EXCEPTION 'Argo desired state requires the exact protected GitHub App platform binding'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       platform_state NOT IN ('ready','indexing') OR
       platform_head IS DISTINCT FROM NEW.base_revision
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires the exact provider-verified planned platform head'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       NEW.write_base_revision<>'' OR NEW.write_base_observed_at IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'Argo desired-state write base may only be set by a fenced claim'
            USING ERRCODE='23514';
    END IF;

    SELECT kind,target_ref,project_id,environment_id,state,target_head_revision,
           indexed_revision,projection_generation
      INTO environment_kind,environment_ref,environment_project,bound_environment,
           environment_state,environment_head,environment_indexed,
           environment_projection_generation
      FROM git_repository_bindings
     WHERE id=NEW.environment_binding_id;
    IF environment_kind IS DISTINCT FROM 'environment' OR
       environment_ref IS DISTINCT FROM NEW.environment_target_ref OR
       environment_project IS DISTINCT FROM NEW.project_id OR
       bound_environment IS DISTINCT FROM NEW.environment_id THEN
        RAISE EXCEPTION 'Argo desired state environment binding identity does not match'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       environment_state IS DISTINCT FROM 'ready' OR
       environment_head IS DISTINCT FROM environment_indexed OR
       environment_indexed IS DISTINCT FROM NEW.environment_revision OR
       environment_projection_generation IS DISTINCT FROM NEW.environment_generation
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires the exact active indexed environment generation'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND NOT EXISTS (
        SELECT 1 FROM git_projection_generations generation
         WHERE generation.binding_id=NEW.environment_binding_id
           AND generation.generation=NEW.environment_generation
           AND generation.head_revision=NEW.environment_revision
           AND generation.state='active'
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires an activated projection receipt'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND EXISTS (
        SELECT 1 FROM git_projected_documents document
         WHERE document.binding_id=NEW.environment_binding_id
           AND document.generation=NEW.environment_generation
           AND NOT document.valid
    ) THEN
        RAISE EXCEPTION 'Argo desired state refuses an invalid projected document'
            USING ERRCODE='23514';
    END IF;

    IF NEW.destination_namespace IS DISTINCT FROM (
        SELECT namespace FROM environments WHERE id=NEW.environment_id
    ) OR NEW.argo_project IS DISTINCT FROM (
        SELECT argo_project FROM environments WHERE id=NEW.environment_id
    ) THEN
        RAISE EXCEPTION 'Argo destination identity must be server-derived'
            USING ERRCODE='23514';
    END IF;

    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.id,NEW.generation,NEW.project_id,NEW.environment_id,
               NEW.platform_binding_id,NEW.environment_binding_id,NEW.cluster_id,
               NEW.platform_target_ref,NEW.environment_target_ref,
               NEW.environment_revision,NEW.environment_generation,NEW.path,
               NEW.argo_namespace,NEW.destination_namespace,NEW.argo_project,
               NEW.base_revision,NEW.precondition,NEW.expected_etag,
               NEW.catalog_digest,NEW.chart_repository,NEW.chart_name,
               NEW.chart_version,NEW.chart_digest,NEW.renderer_image,
               NEW.chart_digest_enforcement,NEW.content,NEW.content_sha256,
               NEW.message,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.generation,OLD.project_id,OLD.environment_id,
               OLD.platform_binding_id,OLD.environment_binding_id,OLD.cluster_id,
               OLD.platform_target_ref,OLD.environment_target_ref,
               OLD.environment_revision,OLD.environment_generation,OLD.path,
               OLD.argo_namespace,OLD.destination_namespace,OLD.argo_project,
               OLD.base_revision,OLD.precondition,OLD.expected_etag,
               OLD.catalog_digest,OLD.chart_repository,OLD.chart_name,
               OLD.chart_version,OLD.chart_digest,OLD.renderer_image,
               OLD.chart_digest_enforcement,OLD.content,OLD.content_sha256,
               OLD.message,OLD.created_at) THEN
            RAISE EXCEPTION 'Argo desired-state command identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Argo desired-state command epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Argo desired-state lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.write_base_revision<>'' AND
           ROW(NEW.write_base_revision,NEW.write_base_observed_at)
           IS DISTINCT FROM
           ROW(OLD.write_base_revision,OLD.write_base_observed_at) THEN
            RAISE EXCEPTION 'Argo desired-state write-base receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.write_base_revision='' AND NEW.write_base_revision<>'' AND (
           OLD.state<>'claimed' OR NEW.state<>'claimed' OR
           OLD.lease_owner IS NULL OR NEW.lease_owner IS NULL OR
           NEW.lease_epoch<>OLD.lease_epoch OR
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest)
        ) THEN
            RAISE EXCEPTION 'Argo desired-state write-base receipt requires the exact fenced claim'
                USING ERRCODE='23514';
        END IF;
        IF OLD.state IN ('verified','blocked-prerequisite','failed','superseded') AND
           NEW.state<>OLD.state THEN
            RAISE EXCEPTION 'Argo desired-state terminal state is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.state IN ('verified','blocked-prerequisite','failed','superseded') AND
           ROW(NEW.state,NEW.committed_revision,NEW.committed_at,NEW.verified_at,
               NEW.write_base_revision,NEW.write_base_observed_at,
               NEW.next_attempt_at,NEW.consecutive_failures,NEW.last_failure_code,
               NEW.lease_owner,NEW.lease_epoch,NEW.lease_until,
               NEW.worker_contract,NEW.worker_config_digest,NEW.updated_at,
               NEW.completed_at)
           IS DISTINCT FROM
           ROW(OLD.state,OLD.committed_revision,OLD.committed_at,OLD.verified_at,
               OLD.write_base_revision,OLD.write_base_observed_at,
               OLD.next_attempt_at,OLD.consecutive_failures,OLD.last_failure_code,
               OLD.lease_owner,OLD.lease_epoch,OLD.lease_until,
               OLD.worker_contract,OLD.worker_config_digest,OLD.updated_at,
               OLD.completed_at) THEN
            RAISE EXCEPTION 'Argo desired-state terminal result is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND
           ROW(NEW.committed_revision,NEW.committed_at)
           IS DISTINCT FROM ROW(OLD.committed_revision,OLD.committed_at) THEN
            RAISE EXCEPTION 'Argo desired-state Git receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.completed_at IS NOT NULL AND NEW.completed_at<>OLD.completed_at) THEN
            RAISE EXCEPTION 'Argo desired-state command time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_commands_validate
    BEFORE INSERT OR UPDATE ON argo_desired_state_commands
    FOR EACH ROW EXECUTE FUNCTION validate_argo_desired_state_command();

-- A public Argo capability requires a fresh worker with the exact protected
-- binding, non-secret runtime config, pinned chart identity, and a real digest
-- enforcement mechanism. Annotation-only Helm chart/version sources are
-- deliberately not an accepted enforcement mode here: Argo's generic OCI
-- source must use the exact digest as targetRevision.
CREATE TABLE argo_desired_state_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (
        config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    github_app_id bigint NOT NULL CHECK (github_app_id>0),
    platform_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    argo_namespace text NOT NULL CHECK (
        argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    root_application_name text NOT NULL CHECK (
        root_application_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'
    ),
    repository_secret_name text NOT NULL CHECK (
        repository_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$'
    ),
    chart_repository text NOT NULL CHECK (
        length(chart_repository) BETWEEN 7 AND 512 AND
        chart_repository ~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$'
    ),
    chart_name text NOT NULL CHECK (chart_name='kuberploy-runtime'),
    chart_version text NOT NULL CHECK (
        length(chart_version) BETWEEN 5 AND 64 AND
        chart_version ~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'
    ),
    chart_digest text NOT NULL CHECK (
        chart_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    renderer_image text NOT NULL CHECK (
        length(renderer_image) BETWEEN 10 AND 512 AND
        renderer_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'
    ),
    chart_digest_enforcement text NOT NULL CHECK (
        chart_digest_enforcement='native-oci-digest-v1'
    ),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX argo_desired_state_runtime_readiness_match_idx
    ON argo_desired_state_runtime_readiness(
        contract_version,config_digest,github_app_id,platform_binding_id,
        cluster_id,chart_digest,renderer_image,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_argo_desired_state_runtime_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_kind text;
    binding_mode text;
BEGIN
    SELECT kind,credential_mode INTO binding_kind,binding_mode
      FROM git_repository_bindings WHERE id=NEW.platform_binding_id;
    IF binding_kind IS DISTINCT FROM 'platform' OR
       binding_mode IS DISTINCT FROM 'github-app' THEN
        RAISE EXCEPTION 'Argo readiness requires a protected GitHub App platform binding'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
           NEW.worker_epoch>OLD.worker_epoch+1 THEN
            RAISE EXCEPTION 'Argo desired-state readiness epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.worker_epoch=OLD.worker_epoch AND (
            NEW.contract_version<>OLD.contract_version OR
            NEW.config_digest<>OLD.config_digest OR
            NEW.github_app_id<>OLD.github_app_id OR
            NEW.platform_binding_id<>OLD.platform_binding_id OR
            NEW.cluster_id<>OLD.cluster_id OR
            NEW.argo_namespace<>OLD.argo_namespace OR
            NEW.root_application_name<>OLD.root_application_name OR
            NEW.repository_secret_name<>OLD.repository_secret_name OR
            NEW.chart_repository<>OLD.chart_repository OR
            NEW.chart_name<>OLD.chart_name OR
            NEW.chart_version<>OLD.chart_version OR
            NEW.chart_digest<>OLD.chart_digest OR
            NEW.renderer_image<>OLD.renderer_image OR
            NEW.chart_digest_enforcement<>OLD.chart_digest_enforcement OR
            NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
        ) THEN
            RAISE EXCEPTION 'Argo desired-state readiness identity or time regressed'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER argo_desired_state_runtime_readiness_validate
    BEFORE INSERT OR UPDATE ON argo_desired_state_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_argo_desired_state_runtime_readiness();

-- Private runtime image pulls use operator-projected source credentials to
-- create revisioned, namespace-local dockerconfig Secrets. Credential bytes,
-- hashes of credential bytes, and Kubernetes Secret data are never persisted
-- in PostgreSQL. The exact registry target, opaque credential reference and
-- operator profile revision are immutable for each artifact.
CREATE TABLE runtime_registry_pull_artifacts (
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    namespace text NOT NULL CHECK (
        length(namespace) BETWEEN 1 AND 63 AND
        namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    pull_credential_ref text NOT NULL CHECK (
        length(pull_credential_ref) BETWEEN 1 AND 256 AND
        pull_credential_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
    ),
    profile_name text NOT NULL CHECK (
        length(profile_name) BETWEEN 1 AND 63 AND
        profile_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    profile_revision bigint NOT NULL CHECK (profile_revision > 0),
    secret_name text NOT NULL CHECK (
        length(secret_name) BETWEEN 1 AND 63 AND
        secret_name ~ '^kuberploy-pull-[a-f0-9]{24}$'
    ),
    active boolean NOT NULL DEFAULT true,
    runtime_state text NOT NULL DEFAULT 'awaiting'
        CHECK (runtime_state IN ('awaiting','ready','failed')),
    next_observation_at timestamptz NOT NULL,
    last_observed_at timestamptz,
    consecutive_failures integer NOT NULL DEFAULT 0
        CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    observed_uid text NOT NULL DEFAULT '' CHECK (
        observed_uid='' OR
        observed_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
    ),
    observed_resource_version text NOT NULL DEFAULT '' CHECK (
        length(observed_resource_version) <= 128 AND
        observed_resource_version !~ E'[\\x00\\r\\n]'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_until timestamptz,
    worker_contract text CHECK (
        worker_contract IS NULL OR
        (length(worker_contract) BETWEEN 8 AND 64 AND
         worker_contract ~ '^[a-z][a-z0-9.-]{7,63}$')
    ),
    worker_config_digest text CHECK (
        worker_config_digest IS NULL OR
        worker_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (environment_id,registry_target_id,profile_revision),
    UNIQUE (namespace,secret_name),
    CHECK (updated_at >= created_at AND next_observation_at >= created_at),
    CHECK ((last_observed_at IS NULL) = (observed_uid='')),
    CHECK ((observed_uid='') = (observed_resource_version='')),
    CHECK ((last_failure_code='') = (consecutive_failures=0)),
    CHECK (
        (runtime_state='awaiting' AND last_observed_at IS NULL) OR
        (runtime_state='ready' AND last_observed_at IS NOT NULL) OR
        (runtime_state='failed')
    ),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND
         worker_contract IS NULL AND worker_config_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract IS NOT NULL AND worker_config_digest IS NOT NULL AND
         lease_epoch > 0 AND lease_until > updated_at)
    )
);

CREATE INDEX runtime_registry_pull_artifacts_due_idx
    ON runtime_registry_pull_artifacts(next_observation_at,environment_id,registry_target_id)
    WHERE active AND runtime_state<>'failed';

CREATE UNIQUE INDEX runtime_registry_pull_artifacts_one_active_idx
    ON runtime_registry_pull_artifacts(environment_id,registry_target_id)
    WHERE active;

CREATE OR REPLACE FUNCTION validate_runtime_registry_pull_artifact()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_namespace text;
    durable_pull_ref text;
BEGIN
    SELECT namespace INTO durable_namespace
      FROM environments WHERE id=NEW.environment_id FOR KEY SHARE;
    SELECT pull_credential_ref INTO durable_pull_ref
      FROM registry_targets WHERE id=NEW.registry_target_id FOR KEY SHARE;
    IF durable_namespace IS DISTINCT FROM NEW.namespace OR
       durable_pull_ref IS NULL OR durable_pull_ref='' OR
       durable_pull_ref IS DISTINCT FROM NEW.pull_credential_ref THEN
        RAISE EXCEPTION 'Runtime registry pull artifact scope does not match durable metadata'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.environment_id,NEW.namespace,NEW.registry_target_id,
               NEW.pull_credential_ref,NEW.profile_name,NEW.profile_revision,
               NEW.secret_name,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.environment_id,OLD.namespace,OLD.registry_target_id,
               OLD.pull_credential_ref,OLD.profile_name,OLD.profile_revision,
               OLD.secret_name,OLD.created_at) THEN
            RAISE EXCEPTION 'Runtime registry pull artifact identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Runtime registry pull artifact epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) AND
           NEW.lease_owner IS NOT NULL THEN
            RAISE EXCEPTION 'Runtime registry pull artifact lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.runtime_state='failed' AND NEW.runtime_state<>'failed' THEN
            RAISE EXCEPTION 'Failed runtime registry pull artifacts are terminal'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.last_observed_at IS NOT NULL AND NEW.last_observed_at IS NOT NULL AND
            NEW.last_observed_at<OLD.last_observed_at) THEN
            RAISE EXCEPTION 'Runtime registry pull artifact time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER runtime_registry_pull_artifacts_validate
    BEFORE INSERT OR UPDATE ON runtime_registry_pull_artifacts
    FOR EACH ROW EXECUTE FUNCTION validate_runtime_registry_pull_artifact();

-- The API advertises private runtime pulls only while a worker with the exact
-- operator profile set and controller contract holds a fresh readiness lease.
CREATE TABLE runtime_registry_pull_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch > 0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_count integer NOT NULL CHECK (profile_count BETWEEN 1 AND 32),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at >= started_at AND lease_until > observed_at)
);

CREATE INDEX runtime_registry_pull_readiness_match_idx
    ON runtime_registry_pull_readiness(
        contract_version,config_digest,profile_count,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_runtime_registry_pull_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Runtime registry pull readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.profile_count<>OLD.profile_count OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Runtime registry pull readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER runtime_registry_pull_readiness_epoch
    BEFORE UPDATE ON runtime_registry_pull_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_runtime_registry_pull_readiness_epoch();

-- Edge-runtime readiness is an observation-only contract. It persists no
-- Kubernetes manifests, Secret data, provider credentials, provider endpoints,
-- or arbitrary API paths. The worker may only attest exact operator-approved
-- Traefik, cert-manager, and external-dns profiles.
CREATE TABLE edge_runtime_targets (
    target_key text NOT NULL CHECK (
        length(target_key) BETWEEN 7 AND 64 AND
        target_key ~ '^(traefik|cert-manager|external-dns/[0-9a-f-]{36})$'
    ),
    profile_revision bigint NOT NULL CHECK (profile_revision > 0),
    kind text NOT NULL CHECK (kind IN ('traefik','cert-manager','external-dns')),
    integration_id uuid REFERENCES external_dns_integrations(id) ON DELETE RESTRICT,
    management_mode text NOT NULL CHECK (management_mode IN ('managed','adopted')),
    namespace text NOT NULL CHECK (
        length(namespace) BETWEEN 1 AND 63 AND
        namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    profile_config_map text NOT NULL CHECK (
        length(profile_config_map) BETWEEN 1 AND 253 AND
        profile_config_map ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'
    ),
    external_txt_owner_id text NOT NULL DEFAULT '',
    external_policy text NOT NULL DEFAULT '',
    external_domains text NOT NULL DEFAULT '',
    desired_digest text NOT NULL CHECK (desired_digest ~ '^sha256:[0-9a-f]{64}$'),
    runtime_config_digest text NOT NULL CHECK (runtime_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    active boolean NOT NULL DEFAULT true,
    runtime_state text NOT NULL DEFAULT 'awaiting'
        CHECK (runtime_state IN ('awaiting','ready','failed')),
    next_observation_at timestamptz NOT NULL,
    last_observed_at timestamptz,
    observed_identity_digest text NOT NULL DEFAULT '',
    observed_resource_versions text NOT NULL DEFAULT '',
    consecutive_failures integer NOT NULL DEFAULT 0
        CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_until timestamptz,
    worker_contract text CHECK (
        worker_contract IS NULL OR worker_contract='edge-observer.v1'
    ),
    worker_config_digest text CHECK (
        worker_config_digest IS NULL OR
        worker_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (target_key,profile_revision),
    CHECK (updated_at >= created_at AND next_observation_at >= created_at),
    CHECK (
        (kind='traefik' AND target_key='traefik' AND integration_id IS NULL AND
         external_txt_owner_id='' AND external_policy='' AND external_domains='') OR
        (kind='cert-manager' AND target_key='cert-manager' AND integration_id IS NULL AND
         external_txt_owner_id='' AND external_policy='' AND external_domains='') OR
        (kind='external-dns' AND integration_id IS NOT NULL AND
         target_key='external-dns/' || integration_id::text AND
         external_txt_owner_id ~ '^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$' AND
         external_policy IN ('upsert-only','sync') AND
         length(external_domains) BETWEEN 1 AND 16384 AND
         external_domains !~ '[[:space:][:cntrl:]]')
    ),
    CHECK (
        (last_observed_at IS NULL AND observed_identity_digest='' AND
         observed_resource_versions='') OR
        (last_observed_at IS NOT NULL AND
         observed_identity_digest ~ '^sha256:[0-9a-f]{64}$' AND
         observed_resource_versions ~ '^sha256:[0-9a-f]{64}$')
    ),
    CHECK (runtime_state<>'ready' OR last_observed_at IS NOT NULL),
    CHECK ((last_failure_code='') = (consecutive_failures=0)),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND worker_contract IS NULL AND
         worker_config_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract='edge-observer.v1' AND
         worker_config_digest=runtime_config_digest AND lease_epoch>0 AND
         lease_until>updated_at)
    )
);

CREATE UNIQUE INDEX edge_runtime_targets_active_key_idx
    ON edge_runtime_targets(target_key) WHERE active;
CREATE INDEX edge_runtime_targets_due_idx
    ON edge_runtime_targets(next_observation_at,target_key,profile_revision)
    WHERE active AND runtime_state<>'failed';
CREATE INDEX edge_runtime_targets_readiness_idx
    ON edge_runtime_targets(runtime_config_digest,runtime_state,last_observed_at)
    WHERE active;

CREATE OR REPLACE FUNCTION validate_edge_runtime_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_mode text;
    durable_txt_owner text;
    durable_policy text;
    durable_profile text;
    durable_domains text;
BEGIN
    IF NEW.kind='external-dns' AND NEW.active THEN
        SELECT i.mode,i.txt_owner_id,i.sync_policy,
               COALESCE(i.operator_profile_ref,''),
               COALESCE((
                   SELECT string_agg(suffix.value,',' ORDER BY suffix.value)
                     FROM jsonb_array_elements_text(i.allowed_domain_suffixes) AS suffix(value)
               ),'')
          INTO durable_mode,durable_txt_owner,durable_policy,durable_profile,durable_domains
          FROM external_dns_integrations i
         WHERE i.id=NEW.integration_id;
        IF NOT FOUND OR
           ROW(NEW.management_mode,NEW.external_txt_owner_id,NEW.external_policy,NEW.external_domains)
           IS DISTINCT FROM
           ROW(durable_mode,durable_txt_owner,durable_policy,durable_domains) OR
           (NEW.management_mode='adopted' AND NEW.profile_config_map<>durable_profile) OR
           (NEW.management_mode='managed' AND durable_profile<>'') THEN
            RAISE EXCEPTION 'External DNS edge target does not match its safe integration metadata'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.kind,NEW.integration_id,
               NEW.management_mode,NEW.namespace,NEW.profile_config_map,
               NEW.external_txt_owner_id,NEW.external_policy,NEW.external_domains,
               NEW.desired_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.kind,OLD.integration_id,
               OLD.management_mode,OLD.namespace,OLD.profile_config_map,
               OLD.external_txt_owner_id,OLD.external_policy,OLD.external_domains,
               OLD.desired_digest,OLD.created_at) THEN
            RAISE EXCEPTION 'Edge runtime target identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Edge runtime target lease epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Edge runtime target lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.observed_identity_digest<>'' AND
           NEW.observed_identity_digest<>OLD.observed_identity_digest THEN
            RAISE EXCEPTION 'Edge runtime observed Kubernetes identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.last_observed_at IS NOT NULL AND
            (NEW.last_observed_at IS NULL OR NEW.last_observed_at<OLD.last_observed_at)) THEN
            RAISE EXCEPTION 'Edge runtime target time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER edge_runtime_target_validate
    BEFORE INSERT OR UPDATE ON edge_runtime_targets
    FOR EACH ROW EXECUTE FUNCTION validate_edge_runtime_target();

-- A capability may only consume a fresh exact worker observation. A restarted
-- worker must advance its durable epoch by exactly one; stale processes cannot
-- overwrite the new worker's readiness lease.
CREATE TABLE edge_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch > 0),
    contract_version text NOT NULL CHECK (contract_version='edge-observer.v1'),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    target_count integer NOT NULL CHECK (target_count BETWEEN 1 AND 66),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at >= started_at AND lease_until > observed_at)
);

CREATE INDEX edge_runtime_readiness_match_idx
    ON edge_runtime_readiness(
        contract_version,config_digest,target_count,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_edge_runtime_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'Edge runtime worker identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch<OLD.worker_epoch OR NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Edge runtime readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.target_count<>OLD.target_count OR
        NEW.started_at<>OLD.started_at OR
        NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Edge runtime readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER edge_runtime_readiness_epoch
    BEFORE UPDATE ON edge_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_edge_runtime_readiness_epoch();

-- Approved external Helm applications are an immutable OCI-chart projection.
-- PostgreSQL stores no OCI username, token, credential reference, kubeconfig,
-- Helm flag bag, post-renderer, or dependency URL. The renderer consumes one
-- server-derived descriptor and one bounded values.yaml document.
CREATE TABLE helm_chart_approvals (
    approval_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision>0),
    oci_repository text NOT NULL CHECK (
        length(oci_repository) BETWEEN 12 AND 512 AND
        oci_repository=lower(oci_repository) AND
        oci_repository ~ '^oci://[a-z0-9][a-z0-9.-]*(?::[1-9][0-9]{0,4})?/[a-z0-9][a-z0-9._/-]*[a-z0-9]$' AND
        oci_repository !~ '[@?#[:space:]]' AND
        oci_repository !~ '(^|/)(\.|\.\.)($|/)'
    ),
    chart_version text NOT NULL CHECK (
        length(chart_version) BETWEEN 5 AND 128 AND
        chart_version ~ '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'
    ),
    manifest_digest text NOT NULL CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    package_digest text NOT NULL CHECK (package_digest ~ '^sha256:[0-9a-f]{64}$'),
    values_schema_digest text NOT NULL CHECK (values_schema_digest ~ '^sha256:[0-9a-f]{64}$'),
    renderer_image text NOT NULL CHECK (
        renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    renderer_version text NOT NULL CHECK (renderer_version='4.2.3'),
    policy_version text NOT NULL CHECK (policy_version='external-helm-p0.v1'),
    identity_digest text NOT NULL CHECK (identity_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (
        length(idempotency_key) BETWEEN 16 AND 128 AND
        idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (approval_id,revision),
    UNIQUE (created_by,idempotency_key),
    UNIQUE (oci_repository,chart_version,manifest_digest,package_digest,
            values_schema_digest,renderer_image,policy_version)
);

CREATE OR REPLACE FUNCTION protect_helm_chart_approval()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Approved Helm chart revisions are immutable'
        USING ERRCODE='23514';
END;
$$;

CREATE TRIGGER helm_chart_approvals_immutable
    BEFORE UPDATE ON helm_chart_approvals
    FOR EACH ROW EXECUTE FUNCTION protect_helm_chart_approval();

CREATE TABLE helm_render_commands (
    id uuid PRIMARY KEY,
    idempotency_scope uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (
        length(idempotency_key) BETWEEN 16 AND 128 AND
        idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    approval_id uuid NOT NULL,
    approval_revision bigint NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    namespace text NOT NULL CHECK (
        length(namespace) BETWEEN 1 AND 63 AND
        namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    release_name text NOT NULL CHECK (
        length(release_name) BETWEEN 1 AND 63 AND
        release_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    descriptor_yaml bytea NOT NULL CHECK (octet_length(descriptor_yaml) BETWEEN 1 AND 32768),
    values_yaml bytea NOT NULL CHECK (octet_length(values_yaml) BETWEEN 1 AND 262144),
    descriptor_digest text NOT NULL CHECK (descriptor_digest ~ '^sha256:[0-9a-f]{64}$'),
    values_digest text NOT NULL CHECK (values_digest ~ '^sha256:[0-9a-f]{64}$'),
    input_digest text NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','processing','succeeded','failed')),
    available_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 10),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 10),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    worker_contract text CHECK (worker_contract IS NULL OR worker_contract='external-helm-renderer.v1'),
    worker_renderer_image text CHECK (
        worker_renderer_image IS NULL OR
        worker_renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    worker_renderer_version text CHECK (worker_renderer_version IS NULL OR worker_renderer_version='4.2.3'),
    worker_policy_version text CHECK (worker_policy_version IS NULL OR worker_policy_version='external-helm-p0.v1'),
    worker_limits_digest text CHECK (
        worker_limits_digest IS NULL OR worker_limits_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (idempotency_scope,idempotency_key),
    FOREIGN KEY (approval_id,approval_revision)
        REFERENCES helm_chart_approvals(approval_id,revision) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (application_id,project_id)
        REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND available_at>=created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND worker_contract IS NULL AND
         worker_renderer_image IS NULL AND worker_renderer_version IS NULL AND
         worker_policy_version IS NULL AND worker_limits_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract='external-helm-renderer.v1' AND
         worker_renderer_image='docker.io/alpine/helm:4.2.3' AND
         worker_renderer_version='4.2.3' AND worker_policy_version='external-helm-p0.v1' AND
         worker_limits_digest IS NOT NULL AND lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='queued' AND lease_owner IS NULL AND completed_at IS NULL) OR
        (state='processing' AND lease_owner IS NOT NULL AND completed_at IS NULL AND attempts>0) OR
        (state IN ('succeeded','failed') AND lease_owner IS NULL AND
         completed_at IS NOT NULL AND completed_at>=created_at)
    )
);

CREATE INDEX helm_render_commands_due_idx
    ON helm_render_commands(available_at,created_at,id)
    WHERE state IN ('queued','processing');
CREATE INDEX helm_render_commands_approval_idx
    ON helm_render_commands(approval_id,approval_revision,created_at DESC);
CREATE INDEX helm_render_commands_application_idx
    ON helm_render_commands(environment_id,application_id,created_at DESC);

CREATE OR REPLACE FUNCTION validate_helm_render_command()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_namespace text;
    durable_release_name text;
BEGIN
    SELECT namespace INTO durable_namespace
      FROM environments WHERE id=NEW.environment_id FOR KEY SHARE;
    IF durable_namespace IS DISTINCT FROM NEW.namespace THEN
        RAISE EXCEPTION 'Helm render destination does not match its durable environment'
            USING ERRCODE='23514';
    END IF;
    SELECT slug INTO durable_release_name
      FROM applications
      WHERE id=NEW.application_id AND project_id=NEW.project_id
      FOR KEY SHARE;
    IF durable_release_name IS DISTINCT FROM NEW.release_name THEN
        RAISE EXCEPTION 'Helm render release does not match its durable application'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
        NEW.state<>'queued' OR NEW.attempts<>0 OR
        NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
        NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
        NEW.lease_until IS NOT NULL OR NEW.worker_contract IS NOT NULL OR
        NEW.worker_renderer_image IS NOT NULL OR
        NEW.worker_renderer_version IS NOT NULL OR
        NEW.worker_policy_version IS NOT NULL OR
        NEW.worker_limits_digest IS NOT NULL OR NEW.completed_at IS NOT NULL OR
        NEW.available_at<>NEW.created_at OR NEW.updated_at<>NEW.created_at
    ) THEN
        RAISE EXCEPTION 'Helm render commands must be inserted as pristine queued work'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('succeeded','failed') THEN
            RAISE EXCEPTION 'Terminal Helm render commands are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(NEW.id,NEW.idempotency_scope,NEW.idempotency_key,
               NEW.approval_id,NEW.approval_revision,NEW.project_id,
               NEW.environment_id,NEW.application_id,NEW.namespace,
               NEW.release_name,NEW.descriptor_yaml,NEW.values_yaml,
               NEW.descriptor_digest,NEW.values_digest,NEW.input_digest,
               NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.idempotency_scope,OLD.idempotency_key,
               OLD.approval_id,OLD.approval_revision,OLD.project_id,
               OLD.environment_id,OLD.application_id,OLD.namespace,
               OLD.release_name,OLD.descriptor_yaml,OLD.values_yaml,
               OLD.descriptor_digest,OLD.values_digest,OLD.input_digest,
               OLD.created_at) THEN
            RAISE EXCEPTION 'Helm render command identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm render command fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_renderer_image,
               NEW.worker_renderer_version,NEW.worker_policy_version,
               NEW.worker_limits_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_renderer_image,
               OLD.worker_renderer_version,OLD.worker_policy_version,
               OLD.worker_limits_digest) THEN
            RAISE EXCEPTION 'Helm render lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_render_commands_validate
    BEFORE INSERT OR UPDATE ON helm_render_commands
    FOR EACH ROW EXECUTE FUNCTION validate_helm_render_command();

CREATE TABLE helm_render_results (
    command_id uuid PRIMARY KEY REFERENCES helm_render_commands(id) ON DELETE RESTRICT,
    input_digest text NOT NULL CHECK (input_digest ~ '^sha256:[0-9a-f]{64}$'),
    manifest_digest text NOT NULL CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    inventory_digest text NOT NULL CHECK (inventory_digest ~ '^sha256:[0-9a-f]{64}$'),
    rendered_manifests bytea NOT NULL CHECK (octet_length(rendered_manifests) BETWEEN 1 AND 2097152),
    resource_count integer NOT NULL CHECK (resource_count BETWEEN 1 AND 128),
    output_bytes integer NOT NULL CHECK (
        output_bytes BETWEEN 1 AND 2097152 AND
        output_bytes=octet_length(rendered_manifests)
    ),
    renderer_image text NOT NULL CHECK (
        renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    renderer_version text NOT NULL CHECK (renderer_version='4.2.3'),
    policy_version text NOT NULL CHECK (policy_version='external-helm-p0.v1'),
    limits_digest text NOT NULL CHECK (limits_digest ~ '^sha256:[0-9a-f]{64}$'),
    completed_at timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION validate_helm_render_result()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_input text;
    durable_state text;
BEGIN
    SELECT input_digest,state INTO durable_input,durable_state
      FROM helm_render_commands WHERE id=NEW.command_id FOR KEY SHARE;
    IF durable_state IS DISTINCT FROM 'processing' OR
       durable_input IS DISTINCT FROM NEW.input_digest THEN
        RAISE EXCEPTION 'Helm render result does not match one processing command'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Helm render results are immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_render_results_validate
    BEFORE INSERT OR UPDATE ON helm_render_results
    FOR EACH ROW EXECUTE FUNCTION validate_helm_render_result();

-- No capability is wired by this migration. A later API seam may consume only
-- a fresh exact row matching every pinned renderer and policy identity field.
CREATE TABLE helm_renderer_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (contract_version='external-helm-renderer.v1'),
    renderer_image text NOT NULL CHECK (
        renderer_image='docker.io/alpine/helm:4.2.3'
    ),
    renderer_version text NOT NULL CHECK (renderer_version='4.2.3'),
    policy_version text NOT NULL CHECK (policy_version='external-helm-p0.v1'),
    limits_digest text NOT NULL CHECK (limits_digest ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX helm_renderer_readiness_match_idx
    ON helm_renderer_readiness(contract_version,renderer_image,renderer_version,
                               policy_version,limits_digest,observed_at DESC);

CREATE OR REPLACE FUNCTION protect_helm_renderer_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Helm renderer readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.renderer_image<>OLD.renderer_image OR
        NEW.renderer_version<>OLD.renderer_version OR
        NEW.policy_version<>OLD.policy_version OR
        NEW.limits_digest<>OLD.limits_digest OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Helm renderer readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_renderer_readiness_epoch
    BEFORE UPDATE ON helm_renderer_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_helm_renderer_readiness_epoch();

-- Durable desired-release and protected-Git intent state for approved external
-- Helm applications. Migration 027 remains the immutable chart/render core.
--
-- Publication is deliberately two phase. First, rendered bytes (or a disabled
-- receipt) are committed at a revision-unique path outside Argo's recursive
-- root and provider-verified. Only then may a second intent publish the stable
-- Argo Application, pinned to the first phase's exact commit. A mutable branch
-- can therefore never replace the protected payload Argo consumes.

CREATE TABLE helm_chart_approval_documents (
    approval_id uuid NOT NULL,
    approval_revision bigint NOT NULL,
    values_schema_json bytea NOT NULL CHECK (
        octet_length(values_schema_json) BETWEEN 2 AND 524288
    ),
    default_values_yaml bytea NOT NULL CHECK (
        octet_length(default_values_yaml) BETWEEN 1 AND 262144
    ),
    values_schema_digest text NOT NULL CHECK (
        values_schema_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    documents_digest text NOT NULL CHECK (
        documents_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (approval_id,approval_revision),
    FOREIGN KEY (approval_id,approval_revision)
        REFERENCES helm_chart_approvals(approval_id,revision) ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION protect_helm_chart_approval_documents()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Approved Helm UI documents are immutable'
        USING ERRCODE='23514';
END;
$$;

CREATE TRIGGER helm_chart_approval_documents_immutable
    BEFORE UPDATE OR DELETE ON helm_chart_approval_documents
    FOR EACH ROW EXECUTE FUNCTION protect_helm_chart_approval_documents();

CREATE OR REPLACE FUNCTION validate_helm_chart_approval_document_insert()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE durable_schema_digest text;
BEGIN
    SELECT values_schema_digest INTO durable_schema_digest
      FROM helm_chart_approvals
     WHERE approval_id=NEW.approval_id
       AND revision=NEW.approval_revision
     FOR KEY SHARE;
    IF durable_schema_digest IS DISTINCT FROM NEW.values_schema_digest THEN
        RAISE EXCEPTION 'Approved Helm schema document digest does not match approval'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_chart_approval_documents_validate_insert
    BEFORE INSERT ON helm_chart_approval_documents
    FOR EACH ROW EXECUTE FUNCTION validate_helm_chart_approval_document_insert();

CREATE TABLE helm_release_revisions (
    id uuid PRIMARY KEY,
    generation bigint NOT NULL CHECK (generation>0),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    release_name text NOT NULL CHECK (
        length(release_name) BETWEEN 1 AND 63 AND
        release_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    action text NOT NULL CHECK (
        action IN ('initial','update','retry','disable','rollback')
    ),
    desired_enabled boolean NOT NULL,
    parent_revision_id uuid REFERENCES helm_release_revisions(id) ON DELETE RESTRICT,
    rollback_source_revision_id uuid REFERENCES helm_release_revisions(id) ON DELETE RESTRICT,
    -- The latest verified, live stable Application before this desired change.
    -- It is NULL only while that stable Application is known absent.
    base_intent_id uuid,
    approval_id uuid NOT NULL,
    approval_revision bigint NOT NULL,
    render_command_id uuid UNIQUE REFERENCES helm_render_commands(id) ON DELETE RESTRICT,
    values_yaml bytea NOT NULL CHECK (
        octet_length(values_yaml) BETWEEN 1 AND 262144
    ),
    values_digest text NOT NULL CHECK (
        values_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    intent_digest text NOT NULL CHECK (
        intent_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (
        length(idempotency_key) BETWEEN 16 AND 128 AND
        idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    request_id text NOT NULL CHECK (
        length(request_id) BETWEEN 1 AND 128 AND
        request_id !~ '[[:cntrl:]]'
    ),
    created_at timestamptz NOT NULL,
    UNIQUE (environment_id,application_id,generation),
    UNIQUE (actor_id,idempotency_key),
    UNIQUE (id,project_id,environment_id,application_id,generation),
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (application_id,project_id)
        REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (approval_id,approval_revision)
        REFERENCES helm_chart_approvals(approval_id,revision) ON DELETE RESTRICT,
    CHECK (desired_enabled=(action<>'disable')),
    CHECK ((render_command_id IS NOT NULL)=desired_enabled),
    CHECK (
        (action='initial' AND generation=1 AND parent_revision_id IS NULL AND
         rollback_source_revision_id IS NULL AND base_intent_id IS NULL) OR
        (action IN ('update','retry') AND generation>1 AND
         parent_revision_id IS NOT NULL AND rollback_source_revision_id IS NULL) OR
        (action='disable' AND generation>1 AND parent_revision_id IS NOT NULL AND
         rollback_source_revision_id IS NULL AND base_intent_id IS NOT NULL) OR
        (action='rollback' AND generation>1 AND parent_revision_id IS NOT NULL AND
         rollback_source_revision_id IS NOT NULL)
    ),
    CHECK (
        rollback_source_revision_id IS NULL OR
        rollback_source_revision_id<>parent_revision_id
    )
);

CREATE TABLE helm_release_heads (
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    revision_id uuid NOT NULL,
    generation bigint NOT NULL CHECK (generation>0),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (environment_id,application_id),
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (application_id,project_id)
        REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (revision_id,project_id,environment_id,application_id,generation)
        REFERENCES helm_release_revisions(
            id,project_id,environment_id,application_id,generation
        ) ON DELETE RESTRICT
);

-- Phase one: write one immutable payload path. The stable Argo Application
-- cannot reference this payload until this row reaches provider-verified.
CREATE TABLE helm_protected_payload_intents (
    id uuid PRIMARY KEY,
    release_revision_id uuid NOT NULL UNIQUE,
    release_generation bigint NOT NULL CHECK (release_generation>0),
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('publish','disable-receipt')),
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL CHECK (
        length(platform_target_ref) BETWEEN 1 AND 255 AND
        platform_target_ref !~ '[[:cntrl:]]'
    ),
    environment_target_ref text NOT NULL CHECK (
        length(environment_target_ref) BETWEEN 1 AND 255 AND
        environment_target_ref !~ '[[:cntrl:]]'
    ),
    environment_revision text NOT NULL CHECK (
        environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    catalog_digest text NOT NULL CHECK (
        catalog_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    planned_base_revision text NOT NULL CHECK (
        planned_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    path text NOT NULL CHECK (
        length(path) BETWEEN 1 AND 1024 AND
        path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    precondition text NOT NULL CHECK (precondition='create-if-absent'),
    expected_etag text NOT NULL CHECK (expected_etag=''),
    content bytea NOT NULL CHECK (
        octet_length(content) BETWEEN 1 AND 2097152
    ),
    content_digest text NOT NULL CHECK (
        content_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    manifest_inventory_digest text CHECK (
        manifest_inventory_digest IS NULL OR
        manifest_inventory_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    manifest_resource_count integer CHECK (
        manifest_resource_count IS NULL OR
        manifest_resource_count BETWEEN 1 AND 128
    ),
    intent_digest text NOT NULL CHECK (
        intent_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    commit_trailer text NOT NULL CHECK (
        commit_trailer='Kuberploy-Helm-Payload-Intent: '||id::text
    ),
    publisher_contract text NOT NULL CHECK (
        publisher_contract='helm-protected-publisher.v1'
    ),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND
        message !~ '[[:cntrl:]]'
    ),
    state text NOT NULL CHECK (
        state IN ('pending','claimed','git-committed','verified','failed','superseded')
    ),
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (
        consecutive_failures BETWEEN 0 AND 30
    ),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR
        write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR
        committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_parent_revision text NOT NULL DEFAULT '' CHECK (
        committed_parent_revision='' OR
        committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    verified_at timestamptz,
    verified_path_digest text NOT NULL DEFAULT '' CHECK (
        verified_path_digest='' OR
        verified_path_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    provider_request text NOT NULL DEFAULT '' CHECK (
        length(provider_request)<=256 AND
        provider_request !~ '[[:cntrl:]]'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (environment_id,application_id,release_generation),
    FOREIGN KEY (
        release_revision_id,project_id,environment_id,application_id,release_generation
    ) REFERENCES helm_release_revisions(
        id,project_id,environment_id,application_id,generation
    ) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,platform_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,environment_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id)
        ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND next_attempt_at>=created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (write_base_revision='' AND write_base_observed_at IS NULL) OR
        (write_base_revision<>'' AND write_base_observed_at IS NOT NULL AND
         write_base_observed_at>=created_at AND
         write_base_observed_at<=updated_at)
    ),
    CHECK (
        (action='publish' AND manifest_inventory_digest IS NOT NULL AND
         manifest_resource_count IS NOT NULL) OR
        (action='disable-receipt' AND manifest_inventory_digest IS NULL AND
         manifest_resource_count IS NULL)
    ),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='pending' AND lease_owner IS NULL AND committed_revision='' AND
         committed_parent_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND verified_path_digest='' AND
         provider_request='' AND completed_at IS NULL) OR
        (state='claimed' AND lease_owner IS NOT NULL AND attempts>0 AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='git-committed' AND lease_owner IS NOT NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='verified' AND lease_owner IS NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NOT NULL AND
         verified_at>=committed_at AND verified_path_digest=content_digest AND
         provider_request<>'' AND completed_at=verified_at) OR
        (state='failed' AND lease_owner IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL) OR
        (state='superseded' AND lease_owner IS NULL AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at)
    )
);

-- Phase two: after the payload commit is provider-verified, create/update/delete
-- the one stable Argo Application. Published content must be constructed by the
-- typed Go adapter and pins source_revision to the exact phase-one commit.
CREATE TABLE helm_protected_application_intents (
    id uuid PRIMARY KEY,
    release_revision_id uuid NOT NULL UNIQUE,
    payload_intent_id uuid NOT NULL UNIQUE
        REFERENCES helm_protected_payload_intents(id) ON DELETE RESTRICT,
    release_generation bigint NOT NULL CHECK (release_generation>0),
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('publish','delete')),
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL CHECK (
        length(platform_target_ref) BETWEEN 1 AND 255 AND
        platform_target_ref !~ '[[:cntrl:]]'
    ),
    environment_target_ref text NOT NULL CHECK (
        length(environment_target_ref) BETWEEN 1 AND 255 AND
        environment_target_ref !~ '[[:cntrl:]]'
    ),
    environment_revision text NOT NULL CHECK (
        environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    catalog_digest text NOT NULL CHECK (
        catalog_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    planned_base_revision text NOT NULL CHECK (
        planned_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    payload_revision text NOT NULL CHECK (
        payload_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    payload_path text NOT NULL CHECK (
        length(payload_path) BETWEEN 1 AND 1024 AND
        payload_path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    source_directory text NOT NULL CHECK (
        length(source_directory)<=1024 AND
        source_directory !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    application_path text NOT NULL CHECK (
        length(application_path) BETWEEN 1 AND 1024 AND
        application_path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    operation text NOT NULL CHECK (operation IN ('create','update','delete')),
    precondition text NOT NULL CHECK (
        precondition IN ('create-if-absent','match-etag')
    ),
    expected_etag text NOT NULL CHECK (
        (precondition='create-if-absent' AND expected_etag='') OR
        (precondition='match-etag' AND
         expected_etag ~ '^"sha256:[0-9a-f]{64}"$')
    ),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 0 AND 32768),
    content_digest text NOT NULL CHECK (
        content_digest='' OR content_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    intent_digest text NOT NULL CHECK (
        intent_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    commit_trailer text NOT NULL CHECK (
        commit_trailer='Kuberploy-Helm-Application-Intent: '||id::text
    ),
    publisher_contract text NOT NULL CHECK (
        publisher_contract='helm-protected-publisher.v1'
    ),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    message text NOT NULL CHECK (
        length(message) BETWEEN 1 AND 512 AND
        message !~ '[[:cntrl:]]'
    ),
    state text NOT NULL CHECK (
        state IN ('pending','claimed','git-committed','verified','failed','superseded')
    ),
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (
        consecutive_failures BETWEEN 0 AND 30
    ),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR
        write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR
        committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_parent_revision text NOT NULL DEFAULT '' CHECK (
        committed_parent_revision='' OR
        committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_at timestamptz,
    verified_at timestamptz,
    verified_path_digest text NOT NULL DEFAULT '' CHECK (
        verified_path_digest='' OR
        verified_path_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    provider_request text NOT NULL DEFAULT '' CHECK (
        length(provider_request)<=256 AND
        provider_request !~ '[[:cntrl:]]'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    UNIQUE (environment_id,application_id,release_generation),
    FOREIGN KEY (
        release_revision_id,project_id,environment_id,application_id,release_generation
    ) REFERENCES helm_release_revisions(
        id,project_id,environment_id,application_id,generation
    ) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,platform_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,environment_target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id,project_id,environment_id)
        REFERENCES git_repository_bindings(id,project_id,environment_id)
        ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND next_attempt_at>=created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK (
        (write_base_revision='' AND write_base_observed_at IS NULL) OR
        (write_base_revision<>'' AND write_base_observed_at IS NOT NULL AND
         write_base_observed_at>=created_at AND
         write_base_observed_at<=updated_at)
    ),
    CHECK (
        (action='publish' AND operation IN ('create','update') AND
         source_directory<>'' AND octet_length(content)>0 AND
         content_digest<>'') OR
        (action='delete' AND operation='delete' AND source_directory='' AND
         octet_length(content)=0 AND content_digest='')
    ),
    CHECK (
        (operation='create' AND precondition='create-if-absent') OR
        (operation IN ('update','delete') AND precondition='match-etag')
    ),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         lease_epoch>0 AND lease_until>updated_at)
    ),
    CHECK (
        (state='pending' AND lease_owner IS NULL AND committed_revision='' AND
         committed_parent_revision='' AND committed_at IS NULL AND
         verified_at IS NULL AND verified_path_digest='' AND
         provider_request='' AND completed_at IS NULL) OR
        (state='claimed' AND lease_owner IS NOT NULL AND attempts>0 AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='git-committed' AND lease_owner IS NOT NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NULL) OR
        (state='verified' AND lease_owner IS NULL AND
         write_base_revision<>'' AND committed_revision<>'' AND
         committed_parent_revision=write_base_revision AND
         committed_at IS NOT NULL AND verified_at IS NOT NULL AND
         verified_at>=committed_at AND verified_path_digest=content_digest AND
         provider_request<>'' AND completed_at=verified_at) OR
        (state='failed' AND lease_owner IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL) OR
        (state='superseded' AND lease_owner IS NULL AND
         committed_revision='' AND committed_parent_revision='' AND
         committed_at IS NULL AND verified_at IS NULL AND
         verified_path_digest='' AND provider_request='' AND
         completed_at IS NOT NULL AND completed_at>=created_at)
    )
);

ALTER TABLE helm_release_revisions
    ADD CONSTRAINT helm_release_revisions_base_intent_fk
    FOREIGN KEY (base_intent_id)
    REFERENCES helm_protected_application_intents(id) ON DELETE RESTRICT;

CREATE INDEX helm_release_revisions_history_idx
    ON helm_release_revisions(environment_id,application_id,generation DESC);
CREATE INDEX helm_protected_payload_intents_due_idx
    ON helm_protected_payload_intents(next_attempt_at,created_at,id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE INDEX helm_protected_payload_intents_binding_idx
    ON helm_protected_payload_intents(platform_binding_id,state,created_at);
CREATE INDEX helm_protected_application_intents_due_idx
    ON helm_protected_application_intents(next_attempt_at,created_at,id)
    WHERE state IN ('pending','claimed','git-committed');
CREATE INDEX helm_protected_application_intents_binding_idx
    ON helm_protected_application_intents(platform_binding_id,state,created_at);

CREATE OR REPLACE FUNCTION validate_helm_release_revision()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_namespace text;
    durable_release_name text;
    head_revision uuid;
    head_generation bigint;
    parent_row helm_release_revisions%ROWTYPE;
    rollback_row helm_release_revisions%ROWTYPE;
    base_row helm_protected_application_intents%ROWTYPE;
    latest_application_id uuid;
    latest_application_action text;
    command_approval_id uuid;
    command_approval_revision bigint;
    command_project_id uuid;
    command_environment_id uuid;
    command_application_id uuid;
    command_release_name text;
    command_values bytea;
    command_values_digest text;
    command_state text;
    command_attempts integer;
BEGIN
    IF TG_OP IN ('UPDATE','DELETE') THEN
        RAISE EXCEPTION 'Helm desired-release revisions are immutable'
            USING ERRCODE='23514';
    END IF;

    SELECT namespace INTO durable_namespace
      FROM environments
     WHERE id=NEW.environment_id AND project_id=NEW.project_id
     FOR KEY SHARE;
    SELECT slug INTO durable_release_name
      FROM applications
     WHERE id=NEW.application_id AND project_id=NEW.project_id
     FOR KEY SHARE;
    IF durable_namespace IS NULL OR durable_release_name IS NULL OR
       durable_release_name IS DISTINCT FROM NEW.release_name THEN
        RAISE EXCEPTION 'Helm release destination is not the durable application environment'
            USING ERRCODE='23514';
    END IF;

    SELECT revision_id,generation INTO head_revision,head_generation
      FROM helm_release_heads
     WHERE environment_id=NEW.environment_id
       AND application_id=NEW.application_id
     FOR UPDATE;
    IF NEW.generation=1 THEN
        IF head_revision IS NOT NULL OR NEW.action<>'initial' OR
           NEW.parent_revision_id IS NOT NULL THEN
            RAISE EXCEPTION 'Initial Helm release revision conflicts with durable head'
                USING ERRCODE='23514';
        END IF;
    ELSIF head_revision IS NULL OR
          NEW.parent_revision_id IS DISTINCT FROM head_revision OR
          NEW.generation<>head_generation+1 THEN
        RAISE EXCEPTION 'Helm release revision does not advance the exact durable head'
            USING ERRCODE='23514';
    END IF;
    IF NEW.generation>1 AND EXISTS (
        SELECT 1 FROM helm_protected_application_intents
         WHERE release_revision_id=head_revision
           AND state IN ('pending','claimed','git-committed')
    ) THEN
        RAISE EXCEPTION 'Helm release cannot advance while stable Application publication is unresolved'
            USING ERRCODE='23514';
    END IF;

    IF NEW.parent_revision_id IS NOT NULL THEN
        SELECT * INTO parent_row
          FROM helm_release_revisions
         WHERE id=NEW.parent_revision_id
         FOR KEY SHARE;
        IF parent_row.id IS NULL OR parent_row.project_id<>NEW.project_id OR
           parent_row.environment_id<>NEW.environment_id OR
           parent_row.application_id<>NEW.application_id THEN
            RAISE EXCEPTION 'Helm release parent escaped its application environment'
                USING ERRCODE='23514';
        END IF;
    END IF;

    IF NEW.rollback_source_revision_id IS NOT NULL THEN
        SELECT * INTO rollback_row
          FROM helm_release_revisions
         WHERE id=NEW.rollback_source_revision_id
         FOR KEY SHARE;
        IF rollback_row.id IS NULL OR rollback_row.project_id<>NEW.project_id OR
           rollback_row.environment_id<>NEW.environment_id OR
           rollback_row.application_id<>NEW.application_id OR
           rollback_row.generation>=NEW.generation OR
           NOT rollback_row.desired_enabled THEN
            RAISE EXCEPTION 'Helm rollback source escaped its enabled release history'
                USING ERRCODE='23514';
        END IF;
    END IF;

    SELECT id,action INTO latest_application_id,latest_application_action
      FROM helm_protected_application_intents
     WHERE environment_id=NEW.environment_id
       AND application_id=NEW.application_id
       AND state='verified'
     ORDER BY release_generation DESC
     LIMIT 1
     FOR KEY SHARE;
    IF latest_application_action='delete' THEN
        latest_application_id := NULL;
    END IF;
    IF NEW.base_intent_id IS DISTINCT FROM latest_application_id THEN
        RAISE EXCEPTION 'Helm release base is not the latest verified live Application'
            USING ERRCODE='23514';
    END IF;
    IF NEW.base_intent_id IS NOT NULL THEN
        SELECT * INTO base_row
          FROM helm_protected_application_intents
         WHERE id=NEW.base_intent_id
         FOR KEY SHARE;
        IF base_row.id IS NULL OR base_row.state<>'verified' OR
           base_row.action<>'publish' OR base_row.project_id<>NEW.project_id OR
           base_row.environment_id<>NEW.environment_id OR
           base_row.application_id<>NEW.application_id OR
           base_row.release_generation>=NEW.generation THEN
            RAISE EXCEPTION 'Helm release base is not one prior verified publication'
                USING ERRCODE='23514';
        END IF;
    END IF;

    IF NEW.desired_enabled THEN
        SELECT approval_id,approval_revision,project_id,environment_id,
               application_id,release_name,values_yaml,values_digest,state,attempts
          INTO command_approval_id,command_approval_revision,command_project_id,
               command_environment_id,command_application_id,command_release_name,
               command_values,command_values_digest,command_state,command_attempts
          FROM helm_render_commands
         WHERE id=NEW.render_command_id
         FOR KEY SHARE;
        IF command_approval_id IS NULL OR
           command_approval_id<>NEW.approval_id OR
           command_approval_revision<>NEW.approval_revision OR
           command_project_id<>NEW.project_id OR
           command_environment_id<>NEW.environment_id OR
           command_application_id<>NEW.application_id OR
           command_release_name<>NEW.release_name OR
           command_values IS DISTINCT FROM NEW.values_yaml OR
           command_values_digest<>NEW.values_digest OR
           command_state<>'queued' OR command_attempts<>0 THEN
            RAISE EXCEPTION 'Helm release revision does not match one pristine render command'
                USING ERRCODE='23514';
        END IF;
    END IF;

    IF NEW.action IN ('retry','disable') AND (
        parent_row.approval_id<>NEW.approval_id OR
        parent_row.approval_revision<>NEW.approval_revision OR
        parent_row.values_yaml IS DISTINCT FROM NEW.values_yaml OR
        parent_row.values_digest<>NEW.values_digest
    ) THEN
        RAISE EXCEPTION 'Helm retry/disable must copy the exact parent desired input'
            USING ERRCODE='23514';
    END IF;
    IF NEW.action='rollback' AND (
        rollback_row.approval_id<>NEW.approval_id OR
        rollback_row.approval_revision<>NEW.approval_revision OR
        rollback_row.values_yaml IS DISTINCT FROM NEW.values_yaml OR
        rollback_row.values_digest<>NEW.values_digest
    ) THEN
        RAISE EXCEPTION 'Helm rollback must copy the exact selected historical input'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_release_revisions_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_release_revisions
    FOR EACH ROW EXECUTE FUNCTION validate_helm_release_revision();

CREATE OR REPLACE FUNCTION validate_helm_release_head()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE revision_row helm_release_revisions%ROWTYPE;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'Helm release heads cannot be deleted'
            USING ERRCODE='23514';
    END IF;
    SELECT * INTO revision_row
      FROM helm_release_revisions
     WHERE id=NEW.revision_id
     FOR KEY SHARE;
    IF revision_row.id IS NULL OR revision_row.project_id<>NEW.project_id OR
       revision_row.environment_id<>NEW.environment_id OR
       revision_row.application_id<>NEW.application_id OR
       revision_row.generation<>NEW.generation THEN
        RAISE EXCEPTION 'Helm release head does not identify its exact revision'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND NEW.generation<>1 THEN
        RAISE EXCEPTION 'Helm release head must begin at generation one'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND (
        NEW.project_id<>OLD.project_id OR
        NEW.environment_id<>OLD.environment_id OR
        NEW.application_id<>OLD.application_id OR
        NEW.generation<>OLD.generation+1 OR
        revision_row.parent_revision_id IS DISTINCT FROM OLD.revision_id OR
        NEW.updated_at<=OLD.updated_at
    ) THEN
        RAISE EXCEPTION 'Helm release head update skipped or changed identity'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_release_heads_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_release_heads
    FOR EACH ROW EXECUTE FUNCTION validate_helm_release_head();

CREATE OR REPLACE FUNCTION validate_helm_protected_payload_intent()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    release_row helm_release_revisions%ROWTYPE;
    platform_row git_repository_bindings%ROWTYPE;
    environment_row git_repository_bindings%ROWTYPE;
    result_manifest bytea;
    result_digest text;
    result_inventory text;
    result_count integer;
    command_state text;
    expected_path text;
    disabled_receipt jsonb;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'Helm protected payload intents cannot be deleted'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('verified','failed','superseded') THEN
            RAISE EXCEPTION 'Terminal Helm protected payload intents are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(
            NEW.id,NEW.release_revision_id,NEW.release_generation,NEW.project_id,
            NEW.environment_id,NEW.application_id,NEW.action,
            NEW.platform_binding_id,NEW.environment_binding_id,NEW.cluster_id,
            NEW.platform_target_ref,NEW.environment_target_ref,
            NEW.environment_revision,NEW.environment_generation,NEW.catalog_digest,
            NEW.planned_base_revision,NEW.path,NEW.precondition,NEW.expected_etag,
            NEW.content,NEW.content_digest,NEW.manifest_inventory_digest,
            NEW.manifest_resource_count,NEW.intent_digest,NEW.commit_trailer,
            NEW.publisher_contract,NEW.publisher_config_digest,NEW.message,
            NEW.created_at
        ) IS DISTINCT FROM ROW(
            OLD.id,OLD.release_revision_id,OLD.release_generation,OLD.project_id,
            OLD.environment_id,OLD.application_id,OLD.action,
            OLD.platform_binding_id,OLD.environment_binding_id,OLD.cluster_id,
            OLD.platform_target_ref,OLD.environment_target_ref,
            OLD.environment_revision,OLD.environment_generation,OLD.catalog_digest,
            OLD.planned_base_revision,OLD.path,OLD.precondition,OLD.expected_etag,
            OLD.content,OLD.content_digest,OLD.manifest_inventory_digest,
            OLD.manifest_resource_count,OLD.intent_digest,OLD.commit_trailer,
            OLD.publisher_contract,OLD.publisher_config_digest,OLD.message,
            OLD.created_at
        ) THEN
            RAISE EXCEPTION 'Helm protected payload intent identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm protected payload fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND
           OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NOT NULL AND
           NEW.lease_owner<>OLD.lease_owner THEN
            RAISE EXCEPTION 'Helm protected payload lease owner changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND ROW(
            NEW.committed_revision,NEW.committed_parent_revision,NEW.committed_at
        ) IS DISTINCT FROM ROW(
            OLD.committed_revision,OLD.committed_parent_revision,OLD.committed_at
        ) THEN
            RAISE EXCEPTION 'Helm protected payload commit receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.verified_at IS NOT NULL AND ROW(
            NEW.verified_at,NEW.verified_path_digest,NEW.provider_request
        ) IS DISTINCT FROM ROW(
            OLD.verified_at,OLD.verified_path_digest,OLD.provider_request
        ) THEN
            RAISE EXCEPTION 'Helm protected payload verification receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NOT (
            (OLD.state='pending' AND NEW.state IN ('pending','claimed','failed','superseded')) OR
            (OLD.state='claimed' AND NEW.state IN ('pending','claimed','git-committed','failed','superseded')) OR
            (OLD.state='git-committed' AND NEW.state IN ('git-committed','verified','failed'))
        ) THEN
            RAISE EXCEPTION 'Helm protected payload state transition is invalid'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.state<>'pending' OR NEW.next_attempt_at<>NEW.created_at OR
       NEW.updated_at<>NEW.created_at OR NEW.attempts<>0 OR
       NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
       NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
       NEW.lease_until IS NOT NULL OR NEW.write_base_revision<>'' OR
       NEW.write_base_observed_at IS NOT NULL OR NEW.committed_revision<>'' OR
       NEW.committed_parent_revision<>'' OR NEW.committed_at IS NOT NULL OR
       NEW.verified_at IS NOT NULL OR NEW.verified_path_digest<>'' OR
       NEW.provider_request<>'' OR NEW.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'Helm protected payload intents must be pristine pending work'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO release_row
      FROM helm_release_revisions
     WHERE id=NEW.release_revision_id
     FOR KEY SHARE;
    IF release_row.id IS NULL OR release_row.generation<>NEW.release_generation OR
       release_row.project_id<>NEW.project_id OR
       release_row.environment_id<>NEW.environment_id OR
       release_row.application_id<>NEW.application_id OR
       (release_row.desired_enabled AND NEW.action<>'publish') OR
       (NOT release_row.desired_enabled AND NEW.action<>'disable-receipt') THEN
        RAISE EXCEPTION 'Helm protected payload does not match its desired release revision'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM helm_release_heads
         WHERE environment_id=NEW.environment_id
           AND application_id=NEW.application_id
           AND revision_id=NEW.release_revision_id
           AND generation=NEW.release_generation
         FOR UPDATE
    ) THEN
        RAISE EXCEPTION 'Helm protected payload revision is no longer the durable head'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO platform_row
      FROM git_repository_bindings
     WHERE id=NEW.platform_binding_id
     FOR KEY SHARE;
    SELECT * INTO environment_row
      FROM git_repository_bindings
     WHERE id=NEW.environment_binding_id
     FOR KEY SHARE;
    IF platform_row.id IS NULL OR platform_row.kind<>'platform' OR
       platform_row.credential_mode<>'github-app' OR
       platform_row.cluster_id<>NEW.cluster_id OR
       platform_row.target_ref<>NEW.platform_target_ref OR
       platform_row.path_prefix<>'clusters/'||NEW.cluster_id::text OR
       platform_row.state NOT IN ('ready','indexing') OR
       platform_row.target_head_revision<>NEW.planned_base_revision OR
       environment_row.id IS NULL OR environment_row.kind<>'environment' OR
       environment_row.project_id<>NEW.project_id OR
       environment_row.environment_id<>NEW.environment_id OR
       environment_row.target_ref<>NEW.environment_target_ref OR
       environment_row.target_head_revision<>NEW.environment_revision OR
       environment_row.indexed_revision<>NEW.environment_revision OR
       environment_row.projection_generation<>NEW.environment_generation OR
       environment_row.state<>'ready' THEN
        RAISE EXCEPTION 'Helm protected payload bindings are stale or mismatched'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM git_projection_generations generation
         WHERE generation.binding_id=NEW.environment_binding_id
           AND generation.generation=NEW.environment_generation
           AND generation.head_revision=NEW.environment_revision
           AND generation.state='active'
    ) OR EXISTS (
        SELECT 1 FROM git_projected_documents document
         WHERE document.binding_id=NEW.environment_binding_id
           AND document.generation=NEW.environment_generation
           AND NOT document.valid
    ) THEN
        RAISE EXCEPTION 'Helm protected payload requires one valid active projection generation'
            USING ERRCODE='23514';
    END IF;

    IF NEW.action='publish' THEN
        expected_path := 'clusters/'||NEW.cluster_id::text||
            '/helm-manifests/environments/'||NEW.environment_id::text||
            '/applications/'||NEW.application_id::text||'/revisions/'||
            NEW.release_revision_id::text||'/release.yaml';
        SELECT result.rendered_manifests,result.manifest_digest,
               result.inventory_digest,result.resource_count,command.state
          INTO result_manifest,result_digest,result_inventory,result_count,command_state
          FROM helm_render_results result
          JOIN helm_render_commands command ON command.id=result.command_id
         WHERE result.command_id=release_row.render_command_id
         FOR KEY SHARE OF result,command;
        IF command_state IS DISTINCT FROM 'succeeded' OR
           result_manifest IS NULL OR result_manifest IS DISTINCT FROM NEW.content OR
           result_digest<>NEW.content_digest OR
           result_inventory<>NEW.manifest_inventory_digest OR
           result_count<>NEW.manifest_resource_count THEN
            RAISE EXCEPTION 'Helm protected payload does not match the durable render result'
                USING ERRCODE='23514';
        END IF;
    ELSE
        expected_path := 'clusters/'||NEW.cluster_id::text||
            '/helm-manifests/environments/'||NEW.environment_id::text||
            '/applications/'||NEW.application_id::text||'/revisions/'||
            NEW.release_revision_id::text||'/disabled.json';
        BEGIN
            disabled_receipt := convert_from(NEW.content,'UTF8')::jsonb;
        EXCEPTION WHEN OTHERS THEN
            RAISE EXCEPTION 'Helm disabled receipt is not valid UTF-8 JSON'
                USING ERRCODE='23514';
        END;
        IF disabled_receipt IS DISTINCT FROM jsonb_build_object(
            'apiVersion','kuberploy.io/v1alpha1',
            'kind','HelmReleaseDisabledReceipt',
            'releaseRevisionId',NEW.release_revision_id::text,
            'generation',NEW.release_generation,
            'projectId',NEW.project_id::text,
            'environmentId',NEW.environment_id::text,
            'applicationId',NEW.application_id::text
        ) THEN
            RAISE EXCEPTION 'Helm disabled receipt identity was not server-derived'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF NEW.path<>expected_path THEN
        RAISE EXCEPTION 'Helm protected payload path was not server-derived'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_payload_intents_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_protected_payload_intents
    FOR EACH ROW EXECUTE FUNCTION validate_helm_protected_payload_intent();

CREATE OR REPLACE FUNCTION validate_helm_protected_application_intent()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    release_row helm_release_revisions%ROWTYPE;
    payload_row helm_protected_payload_intents%ROWTYPE;
    base_row helm_protected_application_intents%ROWTYPE;
    platform_row git_repository_bindings%ROWTYPE;
    expected_application_path text;
    expected_source_directory text;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'Helm protected Application intents cannot be deleted'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('verified','failed','superseded') THEN
            RAISE EXCEPTION 'Terminal Helm protected Application intents are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(
            NEW.id,NEW.release_revision_id,NEW.payload_intent_id,
            NEW.release_generation,NEW.project_id,NEW.environment_id,
            NEW.application_id,NEW.action,NEW.platform_binding_id,
            NEW.environment_binding_id,NEW.cluster_id,NEW.platform_target_ref,
            NEW.environment_target_ref,NEW.environment_revision,
            NEW.environment_generation,NEW.catalog_digest,NEW.planned_base_revision,
            NEW.payload_revision,NEW.payload_path,NEW.source_directory,
            NEW.application_path,NEW.operation,NEW.precondition,NEW.expected_etag,
            NEW.content,NEW.content_digest,NEW.intent_digest,NEW.commit_trailer,
            NEW.publisher_contract,NEW.publisher_config_digest,NEW.message,
            NEW.created_at
        ) IS DISTINCT FROM ROW(
            OLD.id,OLD.release_revision_id,OLD.payload_intent_id,
            OLD.release_generation,OLD.project_id,OLD.environment_id,
            OLD.application_id,OLD.action,OLD.platform_binding_id,
            OLD.environment_binding_id,OLD.cluster_id,OLD.platform_target_ref,
            OLD.environment_target_ref,OLD.environment_revision,
            OLD.environment_generation,OLD.catalog_digest,OLD.planned_base_revision,
            OLD.payload_revision,OLD.payload_path,OLD.source_directory,
            OLD.application_path,OLD.operation,OLD.precondition,OLD.expected_etag,
            OLD.content,OLD.content_digest,OLD.intent_digest,OLD.commit_trailer,
            OLD.publisher_contract,OLD.publisher_config_digest,OLD.message,
            OLD.created_at
        ) THEN
            RAISE EXCEPTION 'Helm protected Application intent identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm protected Application fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND
           OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NOT NULL AND
           NEW.lease_owner<>OLD.lease_owner THEN
            RAISE EXCEPTION 'Helm protected Application lease owner changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND ROW(
            NEW.committed_revision,NEW.committed_parent_revision,NEW.committed_at
        ) IS DISTINCT FROM ROW(
            OLD.committed_revision,OLD.committed_parent_revision,OLD.committed_at
        ) THEN
            RAISE EXCEPTION 'Helm protected Application commit receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.verified_at IS NOT NULL AND ROW(
            NEW.verified_at,NEW.verified_path_digest,NEW.provider_request
        ) IS DISTINCT FROM ROW(
            OLD.verified_at,OLD.verified_path_digest,OLD.provider_request
        ) THEN
            RAISE EXCEPTION 'Helm protected Application verification receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NOT (
            (OLD.state='pending' AND NEW.state IN ('pending','claimed','failed','superseded')) OR
            (OLD.state='claimed' AND NEW.state IN ('pending','claimed','git-committed','failed','superseded')) OR
            (OLD.state='git-committed' AND NEW.state IN ('git-committed','verified'))
        ) THEN
            RAISE EXCEPTION 'Helm protected Application state transition is invalid'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.state<>'pending' OR NEW.next_attempt_at<>NEW.created_at OR
       NEW.updated_at<>NEW.created_at OR NEW.attempts<>0 OR
       NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
       NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
       NEW.lease_until IS NOT NULL OR NEW.write_base_revision<>'' OR
       NEW.write_base_observed_at IS NOT NULL OR NEW.committed_revision<>'' OR
       NEW.committed_parent_revision<>'' OR NEW.committed_at IS NOT NULL OR
       NEW.verified_at IS NOT NULL OR NEW.verified_path_digest<>'' OR
       NEW.provider_request<>'' OR NEW.completed_at IS NOT NULL THEN
        RAISE EXCEPTION 'Helm protected Application intents must be pristine pending work'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO release_row
      FROM helm_release_revisions
     WHERE id=NEW.release_revision_id
     FOR KEY SHARE;
    SELECT * INTO payload_row
      FROM helm_protected_payload_intents
     WHERE id=NEW.payload_intent_id
     FOR KEY SHARE;
    IF release_row.id IS NULL OR payload_row.id IS NULL OR
       payload_row.state<>'verified' OR
       payload_row.release_revision_id<>NEW.release_revision_id OR
       payload_row.release_generation<>NEW.release_generation OR
       payload_row.project_id<>NEW.project_id OR
       payload_row.environment_id<>NEW.environment_id OR
       payload_row.application_id<>NEW.application_id OR
       payload_row.platform_binding_id<>NEW.platform_binding_id OR
       payload_row.environment_binding_id<>NEW.environment_binding_id OR
       payload_row.cluster_id<>NEW.cluster_id OR
       payload_row.platform_target_ref<>NEW.platform_target_ref OR
       payload_row.environment_target_ref<>NEW.environment_target_ref OR
       payload_row.environment_revision<>NEW.environment_revision OR
       payload_row.environment_generation<>NEW.environment_generation OR
       payload_row.catalog_digest<>NEW.catalog_digest OR
       payload_row.committed_revision<>NEW.payload_revision OR
       payload_row.path<>NEW.payload_path OR
       (release_row.desired_enabled AND
        (NEW.action<>'publish' OR payload_row.action<>'publish')) OR
       (NOT release_row.desired_enabled AND
        (NEW.action<>'delete' OR payload_row.action<>'disable-receipt')) THEN
        RAISE EXCEPTION 'Helm protected Application lacks its exact verified payload'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM helm_release_heads
         WHERE environment_id=NEW.environment_id
           AND application_id=NEW.application_id
           AND revision_id=NEW.release_revision_id
           AND generation=NEW.release_generation
         FOR UPDATE
    ) THEN
        RAISE EXCEPTION 'Helm protected Application revision is no longer the durable head'
            USING ERRCODE='23514';
    END IF;

    SELECT * INTO platform_row
      FROM git_repository_bindings
     WHERE id=NEW.platform_binding_id
     FOR KEY SHARE;
    IF platform_row.id IS NULL OR platform_row.kind<>'platform' OR
       platform_row.credential_mode<>'github-app' OR
       platform_row.cluster_id<>NEW.cluster_id OR
       platform_row.target_ref<>NEW.platform_target_ref OR
       platform_row.path_prefix<>'clusters/'||NEW.cluster_id::text OR
       platform_row.state NOT IN ('ready','indexing') OR
       platform_row.target_head_revision<>NEW.planned_base_revision THEN
        RAISE EXCEPTION 'Helm protected Application binding is stale or mismatched'
            USING ERRCODE='23514';
    END IF;

    expected_application_path := 'clusters/'||NEW.cluster_id::text||
        '/argocd/helm-applications/'||NEW.environment_id::text||'/'||
        NEW.application_id::text||'.yaml';
    expected_source_directory := 'clusters/'||NEW.cluster_id::text||
        '/helm-manifests/environments/'||NEW.environment_id::text||
        '/applications/'||NEW.application_id::text||'/revisions/'||
        NEW.release_revision_id::text;
    IF NEW.application_path<>expected_application_path OR
       (NEW.action='publish' AND NEW.source_directory<>expected_source_directory) OR
       (NEW.action='delete' AND NEW.source_directory<>'') THEN
        RAISE EXCEPTION 'Helm protected Application paths were not server-derived'
            USING ERRCODE='23514';
    END IF;

    IF release_row.base_intent_id IS NULL THEN
        IF NEW.operation<>'create' OR NEW.precondition<>'create-if-absent' OR
           NEW.expected_etag<>'' THEN
            RAISE EXCEPTION 'Initial Helm Application publication must prove absence'
                USING ERRCODE='23514';
        END IF;
    ELSE
        SELECT * INTO base_row
          FROM helm_protected_application_intents
         WHERE id=release_row.base_intent_id
         FOR KEY SHARE;
        IF base_row.id IS NULL OR base_row.state<>'verified' OR
           base_row.action<>'publish' OR
           base_row.application_path<>NEW.application_path OR
           base_row.content_digest='' OR
           NEW.expected_etag<>'"'||base_row.content_digest||'"' OR
           NEW.precondition<>'match-etag' OR
           (NEW.action='publish' AND NEW.operation<>'update') OR
           (NEW.action='delete' AND NEW.operation<>'delete') THEN
            RAISE EXCEPTION 'Helm protected Application before-image is not exact'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_application_intents_validate
    BEFORE INSERT OR UPDATE OR DELETE ON helm_protected_application_intents
    FOR EACH ROW EXECUTE FUNCTION validate_helm_protected_application_intent();

-- A capability may consume this observation only when it is fresh and matches
-- the configured writer exactly. It contains no repository credential.
CREATE TABLE helm_protected_publisher_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        contract_version='helm-protected-publisher.v1'
    ),
    policy_version text NOT NULL CHECK (
        policy_version='helm-protected-git.v1'
    ),
    config_digest text NOT NULL CHECK (
        config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);

CREATE INDEX helm_protected_publisher_readiness_match_idx
    ON helm_protected_publisher_readiness(
        contract_version,policy_version,config_digest,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_helm_protected_publisher_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Helm protected publisher readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.policy_version<>OLD.policy_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Helm protected publisher readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_publisher_readiness_epoch
    BEFORE UPDATE ON helm_protected_publisher_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_helm_protected_publisher_readiness_epoch();

-- No user-visible Helm or rollback capability is implied by these tables.
-- Serving requires exact fresh readiness from the Helm renderer, this protected
-- publisher, and Argo desired-state/credential/root observations.

-- Custom ingress certificates reuse the write-only strict SealedSecret
-- lifecycle, but are isolated from general workload secrets by an immutable
-- binding purpose and Kubernetes Secret type. Only public X.509 metadata and
-- the platform-keyed content identity are retained here.

ALTER TABLE secret_bindings
    ADD COLUMN IF NOT EXISTS purpose text NOT NULL DEFAULT 'runtime-secret';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname='secret_bindings_purpose_check'
          AND conrelid='secret_bindings'::regclass
    ) THEN
        ALTER TABLE secret_bindings
            ADD CONSTRAINT secret_bindings_purpose_check CHECK (
                purpose='runtime-secret' OR
                (purpose='tls-certificate' AND provider='sealed-secrets')
            );
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION protect_secret_binding_purpose()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.purpose IS DISTINCT FROM OLD.purpose THEN
        RAISE EXCEPTION 'secret binding purpose is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS secret_bindings_purpose_protect ON secret_bindings;
CREATE TRIGGER secret_bindings_purpose_protect
    BEFORE UPDATE ON secret_bindings
    FOR EACH ROW EXECUTE FUNCTION protect_secret_binding_purpose();

ALTER TABLE secret_binding_versions
    ADD COLUMN IF NOT EXISTS target_secret_type text NOT NULL DEFAULT 'Opaque';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname='secret_binding_versions_target_type_check'
          AND conrelid='secret_binding_versions'::regclass
    ) THEN
        ALTER TABLE secret_binding_versions
            ADD CONSTRAINT secret_binding_versions_target_type_check CHECK (
                target_secret_type IN ('Opaque','kubernetes.io/tls')
            );
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_secret_binding_version_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_purpose text;
    binding_provider text;
BEGIN
    IF TG_OP='UPDATE' AND NEW.target_secret_type IS DISTINCT FROM OLD.target_secret_type THEN
        RAISE EXCEPTION 'secret binding target type is immutable' USING ERRCODE='23514';
    END IF;
    SELECT purpose,provider
      INTO binding_purpose,binding_provider
      FROM secret_bindings
      WHERE id=NEW.binding_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'secret binding does not exist' USING ERRCODE='23503';
    END IF;
    IF NOT (
        (binding_purpose='runtime-secret' AND NEW.target_secret_type='Opaque') OR
        (binding_purpose='tls-certificate' AND binding_provider='sealed-secrets'
         AND NEW.provider='sealed-secrets' AND NEW.target_secret_type='kubernetes.io/tls')
    ) THEN
        RAISE EXCEPTION 'secret binding purpose and target type mismatch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS secret_binding_versions_target_protect ON secret_binding_versions;
CREATE TRIGGER secret_binding_versions_target_protect
    BEFORE INSERT OR UPDATE ON secret_binding_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_secret_binding_version_target();

CREATE TABLE IF NOT EXISTS tls_certificate_versions (
    version_id uuid PRIMARY KEY,
    binding_id uuid NOT NULL,
    version_number bigint NOT NULL CHECK (version_number>0),
    secret_content_fingerprint bytea NOT NULL CHECK (
        octet_length(secret_content_fingerprint)=32
    ),
    leaf_fingerprint text NOT NULL CHECK (
        leaf_fingerprint ~ '^sha256:[0-9a-f]{64}$'
    ),
    public_key_fingerprint text NOT NULL CHECK (
        public_key_fingerprint ~ '^sha256:[0-9a-f]{64}$'
    ),
    dns_names jsonb NOT NULL CHECK (
        jsonb_typeof(dns_names)='array' AND
        jsonb_array_length(dns_names) BETWEEN 1 AND 128
    ),
    ip_addresses jsonb NOT NULL CHECK (
        jsonb_typeof(ip_addresses)='array' AND
        jsonb_array_length(ip_addresses) BETWEEN 0 AND 128 AND
        jsonb_array_length(dns_names)+jsonb_array_length(ip_addresses)<=128
    ),
    not_before timestamptz NOT NULL,
    not_after timestamptz NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    UNIQUE (binding_id,version_number),
    UNIQUE (version_id,binding_id),
    FOREIGN KEY (version_id,binding_id)
        REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT,
    CHECK (not_after>not_before)
);

CREATE INDEX IF NOT EXISTS tls_certificate_versions_binding_idx
    ON tls_certificate_versions(binding_id,version_number);

CREATE OR REPLACE FUNCTION validate_tls_certificate_version()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    stored_binding_id uuid;
    stored_number bigint;
    stored_binding_purpose text;
    stored_binding_provider text;
    stored_version_provider text;
    stored_target_type text;
    stored_state text;
    stored_fingerprint bytea;
    stored_artifact boolean;
    staging_actor uuid;
    item jsonb;
    value text;
    previous text := '';
BEGIN
    SELECT v.binding_id,v.version_number,b.purpose,b.provider,v.provider,
           v.target_secret_type,v.state,v.content_fingerprint,
           (v.provider_object_name IS NOT NULL)
      INTO stored_binding_id,stored_number,stored_binding_purpose,
           stored_binding_provider,stored_version_provider,stored_target_type,
           stored_state,stored_fingerprint,stored_artifact
      FROM secret_binding_versions v
      JOIN secret_bindings b ON b.id=v.binding_id
      WHERE v.id=NEW.version_id;
    IF NOT FOUND OR stored_binding_id<>NEW.binding_id OR
       stored_number<>NEW.version_number OR
       stored_binding_purpose<>'tls-certificate' OR
       stored_binding_provider<>'sealed-secrets' OR
       stored_version_provider<>'sealed-secrets' OR
       stored_target_type<>'kubernetes.io/tls' OR
       stored_state NOT IN ('awaiting-readiness','active','retained') OR
       NOT stored_artifact OR
       stored_fingerprint IS DISTINCT FROM NEW.secret_content_fingerprint THEN
        RAISE EXCEPTION 'certificate attestation does not match its sealed TLS version'
            USING ERRCODE='23514';
    END IF;
    SELECT actor_id INTO staging_actor
      FROM secret_binding_events
      WHERE binding_id=NEW.binding_id AND version_id=NEW.version_id
        AND kind='version-staging';
    IF NOT FOUND OR staging_actor IS DISTINCT FROM NEW.created_by THEN
        RAISE EXCEPTION 'certificate actor does not match the staging event'
            USING ERRCODE='23514';
    END IF;
    IF NEW.created_at IS DISTINCT FROM (
        SELECT created_at FROM secret_binding_versions WHERE id=NEW.version_id
    ) THEN
        RAISE EXCEPTION 'certificate creation time does not match its secret version'
            USING ERRCODE='23514';
    END IF;

    FOR item IN
        SELECT entry.value
        FROM jsonb_array_elements(NEW.dns_names) AS entry(value)
    LOOP
        IF jsonb_typeof(item)<>'string' THEN
            RAISE EXCEPTION 'certificate DNS names must be strings' USING ERRCODE='23514';
        END IF;
        value := item #>> '{}';
        IF value<>lower(value) OR value<>btrim(value) OR length(value)>253 OR
           NOT (
               value ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*$' OR
               value ~ '^\*\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*$'
           ) OR (previous<>'' AND value COLLATE "C"<=previous COLLATE "C") THEN
            RAISE EXCEPTION 'certificate DNS names are not canonical'
                USING ERRCODE='23514';
        END IF;
        previous := value;
    END LOOP;

    previous := '';
    FOR item IN
        SELECT entry.value
        FROM jsonb_array_elements(NEW.ip_addresses) AS entry(value)
    LOOP
        IF jsonb_typeof(item)<>'string' THEN
            RAISE EXCEPTION 'certificate IP addresses must be strings' USING ERRCODE='23514';
        END IF;
        value := item #>> '{}';
        BEGIN
            IF value<>btrim(value) OR value LIKE '::ffff:%' OR
               host(value::inet)<>value OR
               (previous<>'' AND value COLLATE "C"<=previous COLLATE "C") THEN
                RAISE EXCEPTION 'certificate IP addresses are not canonical'
                    USING ERRCODE='23514';
            END IF;
        EXCEPTION WHEN invalid_text_representation THEN
            RAISE EXCEPTION 'certificate IP address is invalid' USING ERRCODE='23514';
        END;
        previous := value;
    END LOOP;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_versions_validate ON tls_certificate_versions;
CREATE TRIGGER tls_certificate_versions_validate
    BEFORE INSERT ON tls_certificate_versions
    FOR EACH ROW EXECUTE FUNCTION validate_tls_certificate_version();

CREATE OR REPLACE FUNCTION protect_tls_certificate_version()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'certificate attestations are append-only' USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_versions_protect ON tls_certificate_versions;
CREATE TRIGGER tls_certificate_versions_protect
    BEFORE UPDATE OR DELETE ON tls_certificate_versions
    FOR EACH ROW EXECUTE FUNCTION protect_tls_certificate_version();

-- A one-time successful SealedSecret activation is not current serving
-- readiness. These rows continuously fence the exact active secret artifact,
-- public X.509 attestation, observer configuration and lease owner. Provider
-- material is never persisted here.
CREATE TABLE IF NOT EXISTS tls_certificate_observations (
    version_id uuid PRIMARY KEY,
    binding_id uuid NOT NULL,
    target_digest text NOT NULL CHECK (target_digest ~ '^sha256:[0-9a-f]{64}$'),
    observation_contract_version text NOT NULL DEFAULT '' CHECK (
        observation_contract_version='' OR
        observation_contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    observation_config_digest text NOT NULL DEFAULT '' CHECK (
        observation_config_digest='' OR
        observation_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    state text NOT NULL CHECK (state IN ('awaiting','ready','degraded','requeue')),
    next_observation_at timestamptz NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 30),
    failure_code text NOT NULL DEFAULT '' CHECK (
        failure_code='' OR failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    last_observed_at timestamptz,
    last_ready_at timestamptz,
    lease_owner text CHECK (
        lease_owner IS NULL OR lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_claimed_at timestamptz,
    lease_until timestamptz,
    lease_contract_version text CHECK (
        lease_contract_version IS NULL OR
        lease_contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    lease_config_digest text CHECK (
        lease_config_digest IS NULL OR
        lease_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    lease_target_digest text CHECK (
        lease_target_digest IS NULL OR
        lease_target_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (version_id,binding_id)
        REFERENCES tls_certificate_versions(version_id,binding_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at),
    CHECK ((observation_contract_version='')=(observation_config_digest='')),
    CHECK (
        (state='awaiting' AND failure_code='' AND last_observed_at IS NULL AND last_ready_at IS NULL) OR
        (state='ready' AND failure_code='' AND last_observed_at IS NOT NULL AND last_ready_at=last_observed_at AND
         observation_contract_version<>'') OR
        (state='degraded' AND failure_code<>'' AND last_observed_at IS NOT NULL AND observation_contract_version<>'') OR
        (state='requeue' AND failure_code<>'' AND observation_contract_version<>'')
    ),
    CHECK (last_ready_at IS NULL OR last_observed_at IS NOT NULL AND last_ready_at<=last_observed_at),
    CHECK (
        (lease_owner IS NULL AND lease_claimed_at IS NULL AND lease_until IS NULL AND
         lease_contract_version IS NULL AND lease_config_digest IS NULL AND lease_target_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_claimed_at IS NOT NULL AND lease_until>lease_claimed_at AND
         lease_contract_version IS NOT NULL AND lease_config_digest IS NOT NULL AND
         lease_target_digest=target_digest)
    )
);
CREATE INDEX IF NOT EXISTS tls_certificate_observations_claim_idx
    ON tls_certificate_observations(next_observation_at,version_id)
    WHERE lease_owner IS NULL;
CREATE INDEX IF NOT EXISTS tls_certificate_observations_binding_idx
    ON tls_certificate_observations(binding_id,version_id,state,last_ready_at);

CREATE OR REPLACE FUNCTION protect_tls_certificate_observation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    old_payload jsonb;
    new_payload jsonb;
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'awaiting' OR NEW.observation_contract_version<>'' OR
           NEW.observation_config_digest<>'' OR NEW.consecutive_failures<>0 OR
           NEW.failure_code<>'' OR NEW.last_observed_at IS NOT NULL OR
           NEW.last_ready_at IS NOT NULL OR NEW.lease_epoch<>0 OR
           NEW.lease_owner IS NOT NULL OR NEW.created_at<>NEW.updated_at THEN
            RAISE EXCEPTION 'certificate observation must start pristine'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'certificate observations are retained'
            USING ERRCODE='23514';
    END IF;
    IF ROW(NEW.version_id,NEW.binding_id,NEW.target_digest,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.version_id,OLD.binding_id,OLD.target_digest,OLD.created_at) OR
       NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'certificate observation identity is immutable'
            USING ERRCODE='23514';
    END IF;
    old_payload := to_jsonb(OLD) - ARRAY['lease_owner','lease_epoch','lease_claimed_at','lease_until',
        'lease_contract_version','lease_config_digest','lease_target_digest','updated_at'];
    new_payload := to_jsonb(NEW) - ARRAY['lease_owner','lease_epoch','lease_claimed_at','lease_until',
        'lease_contract_version','lease_config_digest','lease_target_digest','updated_at'];

    -- Claim or reclaim. The observation payload remains byte-identical and a
    -- previous owner may be replaced only after its exact lease expired.
    IF NEW.lease_epoch=OLD.lease_epoch+1 AND NEW.lease_owner IS NOT NULL THEN
        IF old_payload IS DISTINCT FROM new_payload OR
           NOT (OLD.lease_owner IS NULL OR OLD.lease_until<=NEW.lease_claimed_at) OR
           NEW.lease_target_digest<>OLD.target_digest OR
           NEW.updated_at<>NEW.lease_claimed_at THEN
            RAISE EXCEPTION 'invalid certificate observation claim'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;

    -- Heartbeat. Every lease identity field remains exact and only the expiry
    -- and monotonic updated time may advance.
    IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
       NEW.lease_owner=OLD.lease_owner AND NEW.lease_claimed_at=OLD.lease_claimed_at AND
       NEW.lease_contract_version=OLD.lease_contract_version AND
       NEW.lease_config_digest=OLD.lease_config_digest AND
       NEW.lease_target_digest=OLD.lease_target_digest AND
       NEW.lease_until>OLD.lease_until AND old_payload IS NOT DISTINCT FROM new_payload THEN
        RETURN NEW;
    END IF;

    -- Completion/requeue. Only the exact live lease can clear itself, and the
    -- published readiness identity must be the one that held that lease.
    IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
       NEW.lease_owner IS NULL AND NEW.lease_claimed_at IS NULL AND NEW.lease_until IS NULL AND
       NEW.lease_contract_version IS NULL AND NEW.lease_config_digest IS NULL AND
       NEW.lease_target_digest IS NULL AND NEW.updated_at<OLD.lease_until AND
       NEW.observation_contract_version=OLD.lease_contract_version AND
       NEW.observation_config_digest=OLD.lease_config_digest THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invalid certificate observation transition'
        USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_observations_protect ON tls_certificate_observations;
CREATE TRIGGER tls_certificate_observations_protect
    BEFORE INSERT OR UPDATE OR DELETE ON tls_certificate_observations
    FOR EACH ROW EXECUTE FUNCTION protect_tls_certificate_observation();

CREATE TABLE IF NOT EXISTS tls_certificate_observation_workers (
    worker_id text PRIMARY KEY CHECK (
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL CHECK (observed_at>=started_at),
    lease_until timestamptz NOT NULL CHECK (lease_until>observed_at),
    updated_at timestamptz NOT NULL CHECK (updated_at=observed_at)
);

CREATE OR REPLACE FUNCTION protect_tls_certificate_observation_worker()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'certificate observation worker receipts are retained'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        RETURN NEW;
    END IF;
    IF NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'certificate observation worker identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch+1 AND NEW.started_at>=OLD.observed_at THEN
        RETURN NEW;
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND NEW.contract_version=OLD.contract_version AND
       NEW.config_digest=OLD.config_digest AND NEW.started_at=OLD.started_at AND
       NEW.observed_at>=OLD.observed_at AND NEW.observed_at<OLD.lease_until AND
       NEW.lease_until>OLD.lease_until THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invalid certificate observation worker transition'
        USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_observation_workers_protect ON tls_certificate_observation_workers;
CREATE TRIGGER tls_certificate_observation_workers_protect
    BEFORE UPDATE OR DELETE ON tls_certificate_observation_workers
    FOR EACH ROW EXECUTE FUNCTION protect_tls_certificate_observation_worker();

-- sslip.io route names are derived only from a freshly observed, public
-- Traefik LoadBalancer IPv4 address. AppConfig and API callers never select an
-- IP address or arbitrary sslip.io hostname. Hostname-based cloud load
-- balancers are eligible only when their live DNS answers contain an exact
-- operator-approved static IPv4.
CREATE TABLE edge_sslip_ingress_observations (
    target_key text NOT NULL,
    profile_revision bigint NOT NULL CHECK (profile_revision > 0),
    desired_digest text NOT NULL CHECK (desired_digest ~ '^sha256:[0-9a-f]{64}$'),
    runtime_config_digest text NOT NULL CHECK (runtime_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    public_ipv4 inet NOT NULL CHECK (
        family(public_ipv4)=4 AND masklen(public_ipv4)=32 AND
        NOT (public_ipv4 <<= inet '0.0.0.0/8') AND
        NOT (public_ipv4 <<= inet '10.0.0.0/8') AND
        NOT (public_ipv4 <<= inet '100.64.0.0/10') AND
        NOT (public_ipv4 <<= inet '127.0.0.0/8') AND
        NOT (public_ipv4 <<= inet '169.254.0.0/16') AND
        NOT (public_ipv4 <<= inet '172.16.0.0/12') AND
        NOT (public_ipv4 <<= inet '192.0.0.0/24') AND
        NOT (public_ipv4 <<= inet '192.0.2.0/24') AND
        NOT (public_ipv4 <<= inet '192.88.99.0/24') AND
        NOT (public_ipv4 <<= inet '192.168.0.0/16') AND
        NOT (public_ipv4 <<= inet '198.18.0.0/15') AND
        NOT (public_ipv4 <<= inet '198.51.100.0/24') AND
        NOT (public_ipv4 <<= inet '203.0.113.0/24') AND
        NOT (public_ipv4 <<= inet '224.0.0.0/4') AND
        NOT (public_ipv4 <<= inet '240.0.0.0/4')
    ),
    source_kind text NOT NULL CHECK (source_kind IN ('service-ip','verified-static-ip')),
    service_uid uuid NOT NULL,
    service_resource_version text NOT NULL CHECK (
        length(service_resource_version) BETWEEN 1 AND 128 AND
        service_resource_version ~ '^[A-Za-z0-9._:/+\-]+$'
    ),
    worker_id text NOT NULL CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    lease_epoch bigint NOT NULL CHECK (lease_epoch > 0),
    observed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (target_key,profile_revision),
    FOREIGN KEY (target_key,profile_revision)
        REFERENCES edge_runtime_targets(target_key,profile_revision) ON DELETE RESTRICT,
    CHECK (target_key='traefik' AND updated_at=observed_at AND observed_at>=created_at)
);

CREATE INDEX edge_sslip_ingress_fresh_idx
    ON edge_sslip_ingress_observations(runtime_config_digest,observed_at DESC);

CREATE OR REPLACE FUNCTION protect_edge_sslip_ingress_observation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target edge_runtime_targets%ROWTYPE;
BEGIN
    IF TG_OP='DELETE' THEN
        SELECT * INTO target
          FROM edge_runtime_targets
         WHERE target_key=OLD.target_key AND profile_revision=OLD.profile_revision
         FOR SHARE;
        IF FOUND AND target.active THEN
            RAISE EXCEPTION 'an active sslip ingress observation cannot be deleted'
                USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;

    SELECT * INTO target
      FROM edge_runtime_targets
     WHERE target_key=NEW.target_key AND profile_revision=NEW.profile_revision
     FOR SHARE;
    IF NOT FOUND OR NOT target.active OR target.kind<>'traefik' OR
       target.desired_digest<>NEW.desired_digest OR
       target.runtime_config_digest<>NEW.runtime_config_digest OR
       target.lease_owner<>NEW.worker_id OR target.lease_epoch<>NEW.lease_epoch OR
       target.worker_contract<>'edge-observer.v1' OR
       target.worker_config_digest<>NEW.runtime_config_digest OR
       target.lease_until IS NULL OR target.lease_until<=NEW.observed_at THEN
        RAISE EXCEPTION 'sslip observation is not fenced by the exact live Traefik lease'
            USING ERRCODE='23514';
    END IF;

    IF TG_OP='INSERT' THEN
        IF NEW.created_at<>NEW.observed_at OR NEW.updated_at<>NEW.observed_at THEN
            RAISE EXCEPTION 'sslip observation creation receipt is not pristine'
                USING ERRCODE='23514';
        END IF;
    ELSE
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.desired_digest,
               NEW.public_ipv4,NEW.source_kind,NEW.service_uid,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.desired_digest,
               OLD.public_ipv4,OLD.source_kind,OLD.service_uid,OLD.created_at) OR
           NEW.lease_epoch<=OLD.lease_epoch OR NEW.observed_at<OLD.observed_at THEN
            RAISE EXCEPTION 'sslip endpoint identity is immutable or observation time regressed'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER edge_sslip_ingress_observation_protect
    BEFORE INSERT OR UPDATE OR DELETE ON edge_sslip_ingress_observations
    FOR EACH ROW EXECUTE FUNCTION protect_edge_sslip_ingress_observation();

-- Immutable protected-Git intents for cluster-level environment namespace
-- foundations. Identity is joined from environments and the sole platform Git
-- authority by the store; this table has no generic Kubernetes/YAML input API.
CREATE TABLE environment_foundation_intents (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL,
    project_id uuid NOT NULL,
    namespace text NOT NULL CHECK (
        namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    argo_project text NOT NULL CHECK (
        argo_project ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    platform_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    target_ref text NOT NULL CHECK (
        target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$' AND
        target_ref !~ '(\.\.|//)'
    ),
    planned_head_revision text NOT NULL CHECK (
        planned_head_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    binding_generation bigint NOT NULL CHECK (binding_generation > 0),
    profile_digest text NOT NULL CHECK (profile_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    publisher_contract text NOT NULL CHECK (
        publisher_contract='environment-foundation-protected-git.v1'
    ),
    publisher_policy text NOT NULL CHECK (
        publisher_policy='platform-protected-git.v1'
    ),
    manifest_path text NOT NULL CHECK (
		manifest_path = 'clusters/'||cluster_id::text||'/argocd/foundations/'||environment_id::text||'.yaml'
    ),
    manifest bytea NOT NULL CHECK (octet_length(manifest) BETWEEN 1 AND 262144),
    manifest_digest text NOT NULL CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    intent_digest text NOT NULL UNIQUE CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    commit_trailer text NOT NULL CHECK (
        commit_trailer = 'Kuberploy-Environment-Foundation-Intent: '||id::text
    ),
    state text NOT NULL CHECK (state IN ('pending','claimed','ready','failed','superseded')),
    active boolean NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_until timestamptz,
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_parent_revision text NOT NULL DEFAULT '' CHECK (
        committed_parent_revision='' OR committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    provider_request text NOT NULL DEFAULT '' CHECK (
        length(provider_request)<=256 AND provider_request !~ '[[:cntrl:]]'
    ),
    published_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    CHECK (updated_at >= created_at AND next_attempt_at >= created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
	CHECK ((write_base_revision='')=(write_base_observed_at IS NULL)),
	CHECK (write_base_observed_at IS NULL OR
	       (write_base_observed_at>=created_at AND write_base_observed_at<=updated_at)),
    CHECK ((state IN ('pending','claimed','ready'))=active),
    CHECK (
        (state='claimed' AND lease_owner IS NOT NULL AND lease_epoch>0 AND lease_until>updated_at AND attempts>0) OR
        (state<>'claimed' AND lease_owner IS NULL AND lease_until IS NULL)
    ),
    CHECK (
        (committed_revision='' AND committed_parent_revision='' AND
         provider_request='' AND published_at IS NULL) OR
		(committed_revision<>'' AND committed_parent_revision=write_base_revision AND
         provider_request<>'' AND published_at IS NOT NULL)
    ),
    CHECK (
        (state='ready' AND committed_revision<>'' AND completed_at=published_at) OR
        (state IN ('pending','claimed') AND committed_revision='' AND completed_at IS NULL) OR
        (state='failed' AND committed_revision='' AND completed_at IS NOT NULL AND consecutive_failures>0) OR
        (state='superseded' AND completed_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX environment_foundation_active_environment_idx
    ON environment_foundation_intents(environment_id) WHERE active;
CREATE INDEX environment_foundation_due_idx
    ON environment_foundation_intents(next_attempt_at,id)
    WHERE active AND state IN ('pending','claimed');
CREATE INDEX environment_foundation_exact_ready_idx
    ON environment_foundation_intents(profile_digest,publisher_config_digest,state)
    WHERE active;

CREATE OR REPLACE FUNCTION protect_environment_foundation_intent()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.environment_id,NEW.project_id,NEW.namespace,NEW.argo_project,
           NEW.platform_binding_id,NEW.cluster_id,NEW.target_ref,NEW.planned_head_revision,
           NEW.binding_generation,NEW.profile_digest,NEW.publisher_config_digest,
           NEW.publisher_contract,NEW.publisher_policy,
           NEW.manifest_path,NEW.manifest,NEW.manifest_digest,NEW.intent_digest,
           NEW.commit_trailer,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.environment_id,OLD.project_id,OLD.namespace,OLD.argo_project,
           OLD.platform_binding_id,OLD.cluster_id,OLD.target_ref,OLD.planned_head_revision,
           OLD.binding_generation,OLD.profile_digest,OLD.publisher_config_digest,
           OLD.publisher_contract,OLD.publisher_policy,
           OLD.manifest_path,OLD.manifest,OLD.manifest_digest,OLD.intent_digest,
           OLD.commit_trailer,OLD.created_at) THEN
        RAISE EXCEPTION 'Environment foundation intent identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Environment foundation lease epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='pending' AND NEW.state IN ('claimed','superseded')) OR
        (OLD.state='claimed' AND NEW.state IN ('claimed','pending','ready','failed','superseded')) OR
        (OLD.state='ready' AND NEW.state IN ('ready','superseded')) OR
        (OLD.state IN ('failed','superseded') AND NEW.state=OLD.state)
    ) THEN
        RAISE EXCEPTION 'Environment foundation state transition is invalid'
            USING ERRCODE='23514';
    END IF;
    IF OLD.committed_revision<>'' AND
       ROW(NEW.committed_revision,NEW.committed_parent_revision,
           NEW.provider_request,NEW.published_at)
       IS DISTINCT FROM
       ROW(OLD.committed_revision,OLD.committed_parent_revision,
           OLD.provider_request,OLD.published_at) THEN
        RAISE EXCEPTION 'Environment foundation publication receipt is immutable'
            USING ERRCODE='23514';
    END IF;
	IF OLD.write_base_revision<>'' AND
	   ROW(NEW.write_base_revision,NEW.write_base_observed_at)
	   IS DISTINCT FROM ROW(OLD.write_base_revision,OLD.write_base_observed_at) THEN
		RAISE EXCEPTION 'Environment foundation write base is immutable'
			USING ERRCODE='23514';
	END IF;
    IF NEW.updated_at<OLD.updated_at OR
       (OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at) OR
       (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) THEN
        RAISE EXCEPTION 'Environment foundation durable time or receipt regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER environment_foundation_intent_protect
    BEFORE UPDATE ON environment_foundation_intents
    FOR EACH ROW EXECUTE FUNCTION protect_environment_foundation_intent();

CREATE TABLE environment_foundation_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (contract_version='environment-foundation.v1'),
    profile_digest text NOT NULL CHECK (profile_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    active_intent_count integer NOT NULL CHECK (active_intent_count BETWEEN 0 AND 10000),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);
CREATE INDEX environment_foundation_readiness_exact_idx
    ON environment_foundation_readiness(
        contract_version,profile_digest,publisher_config_digest,
        active_intent_count,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_environment_foundation_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_epoch<OLD.worker_epoch OR NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Environment foundation readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND
       (ROW(NEW.contract_version,NEW.profile_digest,NEW.publisher_config_digest,
            NEW.started_at)
        IS DISTINCT FROM
        ROW(OLD.contract_version,OLD.profile_digest,OLD.publisher_config_digest,
            OLD.started_at) OR
        NEW.observed_at<OLD.observed_at) THEN
        RAISE EXCEPTION 'Environment foundation readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER environment_foundation_readiness_protect
    BEFORE UPDATE ON environment_foundation_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_environment_foundation_readiness();

-- Fence every external Helm render across rolling operator configuration
-- changes. Historical work cannot be attributed to the complete operator
-- policy, so it is stamped with a non-runnable sentinel and any non-terminal
-- command is failed closed during the upgrade.

ALTER TABLE helm_render_commands
    ADD COLUMN operator_config_digest text NOT NULL
        DEFAULT 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
        CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN worker_operator_config_digest text
        CHECK (worker_operator_config_digest IS NULL OR
               worker_operator_config_digest ~ '^sha256:[0-9a-f]{64}$');
ALTER TABLE helm_render_commands ALTER COLUMN operator_config_digest DROP DEFAULT;

ALTER TABLE helm_render_results
    ADD COLUMN operator_config_digest text NOT NULL
        DEFAULT 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
        CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$');
ALTER TABLE helm_render_results ALTER COLUMN operator_config_digest DROP DEFAULT;

-- Old readiness cannot prove the complete operator policy that produced it.
DELETE FROM helm_renderer_readiness;
ALTER TABLE helm_renderer_readiness
    ADD COLUMN operator_config_digest text NOT NULL
        CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$');

-- Existing queued or leased commands were created without an operator digest.
-- Do not let a new worker adopt them, including after a crashed old worker.
UPDATE helm_render_commands SET
    state='failed',
    consecutive_failures=LEAST(consecutive_failures+1,10),
    last_failure_code='operator-config-upgrade',
    lease_owner=NULL,
    lease_until=NULL,
    worker_contract=NULL,
    worker_renderer_image=NULL,
    worker_renderer_version=NULL,
    worker_policy_version=NULL,
    worker_limits_digest=NULL,
    worker_operator_config_digest=NULL,
    completed_at=GREATEST(updated_at,clock_timestamp()),
    updated_at=GREATEST(updated_at,clock_timestamp())
WHERE state IN ('queued','processing');

ALTER TABLE helm_render_commands ADD CONSTRAINT helm_render_commands_operator_lease_check CHECK (
    (lease_owner IS NULL AND worker_operator_config_digest IS NULL) OR
    (lease_owner IS NOT NULL AND
     worker_operator_config_digest=operator_config_digest)
);

CREATE OR REPLACE FUNCTION validate_helm_render_command()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_namespace text;
    durable_release_name text;
BEGIN
    SELECT namespace INTO durable_namespace
      FROM environments WHERE id=NEW.environment_id FOR KEY SHARE;
    IF durable_namespace IS DISTINCT FROM NEW.namespace THEN
        RAISE EXCEPTION 'Helm render destination does not match its durable environment'
            USING ERRCODE='23514';
    END IF;
    SELECT slug INTO durable_release_name
      FROM applications
      WHERE id=NEW.application_id AND project_id=NEW.project_id
      FOR KEY SHARE;
    IF durable_release_name IS DISTINCT FROM NEW.release_name THEN
        RAISE EXCEPTION 'Helm render release does not match its durable application'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
        NEW.state<>'queued' OR NEW.attempts<>0 OR
        NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
        NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
        NEW.lease_until IS NOT NULL OR NEW.worker_contract IS NOT NULL OR
        NEW.worker_renderer_image IS NOT NULL OR
        NEW.worker_renderer_version IS NOT NULL OR
        NEW.worker_policy_version IS NOT NULL OR
        NEW.worker_limits_digest IS NOT NULL OR
        NEW.worker_operator_config_digest IS NOT NULL OR
        NEW.completed_at IS NOT NULL OR
        NEW.available_at<>NEW.created_at OR NEW.updated_at<>NEW.created_at
    ) THEN
        RAISE EXCEPTION 'Helm render commands must be inserted as pristine queued work'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('succeeded','failed') THEN
            RAISE EXCEPTION 'Terminal Helm render commands are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(NEW.id,NEW.idempotency_scope,NEW.idempotency_key,
               NEW.approval_id,NEW.approval_revision,NEW.project_id,
               NEW.environment_id,NEW.application_id,NEW.namespace,
               NEW.release_name,NEW.descriptor_yaml,NEW.values_yaml,
               NEW.descriptor_digest,NEW.values_digest,NEW.input_digest,
               NEW.operator_config_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.idempotency_scope,OLD.idempotency_key,
               OLD.approval_id,OLD.approval_revision,OLD.project_id,
               OLD.environment_id,OLD.application_id,OLD.namespace,
               OLD.release_name,OLD.descriptor_yaml,OLD.values_yaml,
               OLD.descriptor_digest,OLD.values_digest,OLD.input_digest,
               OLD.operator_config_digest,OLD.created_at) THEN
            RAISE EXCEPTION 'Helm render command identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm render command fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_renderer_image,
               NEW.worker_renderer_version,NEW.worker_policy_version,
               NEW.worker_limits_digest,NEW.worker_operator_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_renderer_image,
               OLD.worker_renderer_version,OLD.worker_policy_version,
               OLD.worker_limits_digest,OLD.worker_operator_config_digest) THEN
            RAISE EXCEPTION 'Helm render lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_helm_render_result()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_input text;
    durable_state text;
    durable_operator_config_digest text;
    durable_worker_operator_config_digest text;
BEGIN
    SELECT input_digest,state,operator_config_digest,worker_operator_config_digest
      INTO durable_input,durable_state,durable_operator_config_digest,
           durable_worker_operator_config_digest
      FROM helm_render_commands WHERE id=NEW.command_id FOR KEY SHARE;
    IF durable_state IS DISTINCT FROM 'processing' OR
       durable_input IS DISTINCT FROM NEW.input_digest OR
       durable_operator_config_digest IS DISTINCT FROM NEW.operator_config_digest OR
       durable_worker_operator_config_digest IS DISTINCT FROM NEW.operator_config_digest THEN
        RAISE EXCEPTION 'Helm render result does not match one processing command and operator lease'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Helm render results are immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP INDEX helm_renderer_readiness_match_idx;
CREATE INDEX helm_renderer_readiness_match_idx
    ON helm_renderer_readiness(contract_version,renderer_image,renderer_version,
                               policy_version,limits_digest,operator_config_digest,
                               observed_at DESC);

CREATE OR REPLACE FUNCTION protect_helm_renderer_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Helm renderer readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.renderer_image<>OLD.renderer_image OR
        NEW.renderer_version<>OLD.renderer_version OR
        NEW.policy_version<>OLD.policy_version OR
        NEW.limits_digest<>OLD.limits_digest OR
        NEW.operator_config_digest<>OLD.operator_config_digest OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Helm renderer readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Environment protection is immutable policy. Deployment/config callers never
-- choose direct versus pull-request publication; every accepted Git command
-- snapshots the mode derived from this server-owned environment field.
ALTER TABLE environments
    ADD COLUMN protection_policy text NOT NULL DEFAULT 'protected'
        CHECK (protection_policy IN ('development','protected'));

CREATE OR REPLACE FUNCTION protect_environment_protection_policy()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.protection_policy IS DISTINCT FROM OLD.protection_policy THEN
        RAISE EXCEPTION 'environment protection policy is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER environments_protection_policy_immutable
    BEFORE UPDATE ON environments
    FOR EACH ROW EXECUTE FUNCTION protect_environment_protection_policy();

-- Existing commands retain the direct publication contract under which they
-- were accepted. New writers must explicitly persist the environment-derived
-- mode; dropping the default prevents an accidental implicit direct command.
ALTER TABLE git_deployment_write_commands
    ADD COLUMN publication_mode text NOT NULL DEFAULT 'direct'
        CHECK (publication_mode IN ('direct','pull-request'));
ALTER TABLE git_deployment_write_commands
    ALTER COLUMN publication_mode DROP DEFAULT;

CREATE OR REPLACE FUNCTION protect_git_deployment_write_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.operation_id,NEW.deployment_id,NEW.actor_id,NEW.binding_id,
           NEW.project_id,NEW.environment_id,NEW.application_id,NEW.target_ref,
           NEW.path,NEW.base_revision,NEW.precondition,NEW.expected_etag,
           NEW.chart_digest,NEW.policy_version,NEW.content,NEW.content_sha256,
           NEW.message,NEW.publication_mode,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.deployment_id,OLD.actor_id,OLD.binding_id,
           OLD.project_id,OLD.environment_id,OLD.application_id,OLD.target_ref,
           OLD.path,OLD.base_revision,OLD.precondition,OLD.expected_etag,
           OLD.chart_digest,OLD.policy_version,OLD.content,OLD.content_sha256,
           OLD.message,OLD.publication_mode,OLD.created_at) THEN
        RAISE EXCEPTION 'Git deployment write command identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF (OLD.state='git-committed' AND NEW.state='pending') OR
       (OLD.state='indexed' AND NEW.state<>'indexed') OR
       NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'Git deployment write command state cannot regress'
            USING ERRCODE='23514';
    END IF;
    IF OLD.committed_revision<>'' AND
       (NEW.committed_revision<>OLD.committed_revision OR NEW.committed_at<>OLD.committed_at) THEN
        RAISE EXCEPTION 'Git deployment write result is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- A protected candidate and its provider pull request are durable workflow
-- state only. target_revision is absent until the exact merged revision is
-- verified on the authoritative target ref; target indexing remains the sole
-- desired-state advancement path.
CREATE TABLE git_pull_request_publications (
    operation_id uuid PRIMARY KEY
        REFERENCES git_deployment_write_commands(operation_id) ON DELETE CASCADE,
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE RESTRICT,
    provider text NOT NULL CHECK (provider='github'),
    installation_id bigint NOT NULL CHECK (installation_id>0),
    repository_id bigint NOT NULL CHECK (repository_id>0),
    repository_owner text NOT NULL CHECK (
        repository_owner ~ '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$'
    ),
    repository_name text NOT NULL CHECK (
        repository_name ~ '^[A-Za-z0-9_.-]{1,100}$' AND
        repository_name NOT IN ('.','..') AND
        lower(repository_name)<>'.git' AND lower(repository_name) !~ '\.git$'
    ),
    target_ref text NOT NULL CHECK (
        target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$' AND
        target_ref !~ '\.\.' AND target_ref !~ '//' AND target_ref !~ '/$'
    ),
    base_revision text NOT NULL CHECK (
        base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    candidate_ref text NOT NULL CHECK (
        candidate_ref='refs/heads/kuberploy/operations/'||operation_id::text
    ),
    candidate_revision text NOT NULL DEFAULT '' CHECK (
        candidate_revision='' OR candidate_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    pull_request_number bigint NOT NULL DEFAULT 0 CHECK (pull_request_number>=0),
    pull_request_url text NOT NULL DEFAULT '',
    pull_request_state text NOT NULL DEFAULT '' CHECK (pull_request_state IN ('','open','closed')),
    merge_revision text NOT NULL DEFAULT '' CHECK (
        merge_revision='' OR merge_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    target_revision text NOT NULL DEFAULT '' CHECK (
        target_revision='' OR target_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    state text NOT NULL CHECK (state IN (
        'pending-candidate','write-base-ready','candidate-ready','pull-request-open',
        'pull-request-closed','merge-pending','merge-verified'
    )),
    provider_observed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    version bigint NOT NULL DEFAULT 1 CHECK (version>0),
    UNIQUE (repository_id,candidate_ref),
    CHECK (candidate_ref<>target_ref),
    CHECK (updated_at>=created_at),
    CHECK (provider_observed_at IS NULL OR
           (provider_observed_at>=created_at AND provider_observed_at<=updated_at)),
    CHECK (
        (state='pending-candidate' AND write_base_revision='' AND candidate_revision='' AND
            pull_request_number=0 AND pull_request_url='' AND pull_request_state='' AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NULL) OR
        (state='write-base-ready' AND write_base_revision<>'' AND candidate_revision='' AND
            pull_request_number=0 AND pull_request_url='' AND pull_request_state='' AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NULL) OR
        (state='candidate-ready' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number=0 AND pull_request_url='' AND pull_request_state='' AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NULL) OR
        (state='pull-request-open' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='open' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NOT NULL) OR
        (state='pull-request-closed' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='closed' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision='' AND target_revision='' AND provider_observed_at IS NOT NULL) OR
        (state='merge-pending' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='closed' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision<>'' AND target_revision='' AND provider_observed_at IS NOT NULL) OR
        (state='merge-verified' AND write_base_revision<>'' AND candidate_revision<>'' AND
            pull_request_number>0 AND pull_request_state='closed' AND
            pull_request_url='https://github.com/'||repository_owner||'/'||repository_name||'/pull/'||pull_request_number::text AND
            merge_revision<>'' AND target_revision<>'' AND provider_observed_at IS NOT NULL)
    )
);
CREATE UNIQUE INDEX git_pull_request_publications_provider_pr_idx
    ON git_pull_request_publications(repository_id,pull_request_number)
    WHERE pull_request_number>0;
CREATE INDEX git_pull_request_publications_reconcile_idx
    ON git_pull_request_publications(state,updated_at,operation_id)
    WHERE state IN ('candidate-ready','pull-request-open','pull-request-closed','merge-pending');

CREATE OR REPLACE FUNCTION protect_git_pull_request_publication()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NOT EXISTS (
            SELECT 1
            FROM git_deployment_write_commands c
            JOIN git_repository_bindings b ON b.id=c.binding_id
            WHERE c.operation_id=NEW.operation_id
              AND c.publication_mode='pull-request'
              AND c.binding_id=NEW.binding_id
              AND c.target_ref=NEW.target_ref
              AND c.base_revision=NEW.base_revision
              AND b.provider=NEW.provider
              AND b.installation_id=NEW.installation_id
              AND b.repository_id=NEW.repository_id
              AND b.repository_owner=NEW.repository_owner
              AND b.repository_name=NEW.repository_name
        ) THEN
            RAISE EXCEPTION 'pull request publication identity does not match protected command'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.operation_id,NEW.binding_id,NEW.provider,NEW.installation_id,NEW.repository_id,
           NEW.repository_owner,NEW.repository_name,NEW.target_ref,
           NEW.base_revision,NEW.candidate_ref,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.binding_id,OLD.provider,OLD.installation_id,OLD.repository_id,
           OLD.repository_owner,OLD.repository_name,OLD.target_ref,
           OLD.base_revision,OLD.candidate_ref,OLD.created_at) THEN
        RAISE EXCEPTION 'pull request publication identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'pull request publication update is not fenced'
            USING ERRCODE='23514';
    END IF;
    IF OLD.write_base_revision<>'' AND NEW.write_base_revision<>OLD.write_base_revision THEN
        RAISE EXCEPTION 'pull request write base is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.candidate_revision<>'' AND NEW.candidate_revision<>OLD.candidate_revision THEN
        RAISE EXCEPTION 'pull request candidate revision is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.pull_request_number>0 AND
       (NEW.pull_request_number<>OLD.pull_request_number OR
        NEW.pull_request_url<>OLD.pull_request_url) THEN
        RAISE EXCEPTION 'pull request identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.merge_revision<>'' AND NEW.merge_revision<>OLD.merge_revision THEN
        RAISE EXCEPTION 'pull request merge revision is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.target_revision<>'' AND NEW.target_revision<>OLD.target_revision THEN
        RAISE EXCEPTION 'verified target revision is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='pending-candidate' AND NEW.state='write-base-ready') OR
        (OLD.state='write-base-ready' AND NEW.state='candidate-ready') OR
        (OLD.state='candidate-ready' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-open' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-closed' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='merge-pending' AND NEW.state IN ('merge-pending','merge-verified')) OR
        (OLD.state='merge-verified' AND NEW.state='merge-verified')
    ) THEN
        RAISE EXCEPTION 'invalid pull request publication transition'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER git_pull_request_publications_protect
    BEFORE INSERT OR UPDATE ON git_pull_request_publications
    FOR EACH ROW EXECUTE FUNCTION protect_git_pull_request_publication();

-- Scheduling profiles are platform-admin-owned, immutable Pod scheduling
-- policy. Workload callers select an exact revision; they never submit node,
-- taint, NodePool, or NodeClass mutations.
CREATE TABLE scheduling_profiles (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','deactivated')),
    current_revision bigint NOT NULL CHECK (current_revision>0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    deactivated_at timestamptz,
    CHECK ((lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
           (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL))
);

CREATE TABLE scheduling_profile_revisions (
    profile_id uuid NOT NULL REFERENCES scheduling_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision>0),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec)='object' AND octet_length(spec::text)<=65536),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    assignments_digest text NOT NULL CHECK (assignments_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (profile_id,revision)
);
ALTER TABLE scheduling_profiles ADD CONSTRAINT scheduling_profiles_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES scheduling_profile_revisions(profile_id,revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE scheduling_profile_assignments (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal>=0),
    scope_type text NOT NULL CHECK (scope_type IN ('team','project','environment')),
    scope_id uuid NOT NULL,
    PRIMARY KEY (profile_id,revision,ordinal),
    UNIQUE (profile_id,revision,scope_type,scope_id),
    FOREIGN KEY (profile_id,revision) REFERENCES scheduling_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION validate_scheduling_profile_assignment()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.scope_type='team' AND NOT EXISTS (SELECT 1 FROM teams WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) THEN
        RAISE EXCEPTION 'scheduling profile assignment scope does not exist' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER scheduling_profile_assignment_validate BEFORE INSERT ON scheduling_profile_assignments
    FOR EACH ROW EXECUTE FUNCTION validate_scheduling_profile_assignment();

CREATE OR REPLACE FUNCTION protect_scheduling_assigned_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE kind text := TG_ARGV[0];
BEGIN
    IF EXISTS (SELECT 1 FROM scheduling_profile_assignments WHERE scope_type=kind AND scope_id=OLD.id) THEN
        RAISE EXCEPTION 'assigned scheduling scope cannot be deleted' USING ERRCODE='23503';
    END IF;
    RETURN OLD;
END;
$$;
CREATE TRIGGER teams_scheduling_assignment_restrict BEFORE DELETE ON teams
    FOR EACH ROW EXECUTE FUNCTION protect_scheduling_assigned_scope('team');
CREATE TRIGGER projects_scheduling_assignment_restrict BEFORE DELETE ON projects
    FOR EACH ROW EXECUTE FUNCTION protect_scheduling_assigned_scope('project');
CREATE TRIGGER environments_scheduling_assignment_restrict BEFORE DELETE ON environments
    FOR EACH ROW EXECUTE FUNCTION protect_scheduling_assigned_scope('environment');

CREATE TABLE scheduling_profile_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise','deactivate')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_id uuid NOT NULL REFERENCES scheduling_profiles(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key)
);
CREATE OR REPLACE FUNCTION reject_scheduling_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'scheduling profile immutable record cannot change' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER scheduling_profile_revisions_immutable BEFORE UPDATE OR DELETE ON scheduling_profile_revisions FOR EACH ROW EXECUTE FUNCTION reject_scheduling_immutable_change();
CREATE TRIGGER scheduling_profile_assignments_immutable BEFORE UPDATE OR DELETE ON scheduling_profile_assignments FOR EACH ROW EXECUTE FUNCTION reject_scheduling_immutable_change();
CREATE TRIGGER scheduling_profile_commands_immutable BEFORE UPDATE OR DELETE ON scheduling_profile_commands FOR EACH ROW EXECUTE FUNCTION reject_scheduling_immutable_change();

CREATE OR REPLACE FUNCTION protect_scheduling_profile()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.name<>OLD.name OR NEW.created_by<>OLD.created_by OR NEW.created_at<>OLD.created_at OR
       NEW.current_revision<OLD.current_revision OR NEW.current_revision>OLD.current_revision+1 OR
       OLD.lifecycle='deactivated' OR
       (NEW.lifecycle='active' AND (NEW.deactivated_by IS NOT NULL OR NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid scheduling profile transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER scheduling_profiles_protect BEFORE UPDATE ON scheduling_profiles FOR EACH ROW EXECUTE FUNCTION protect_scheduling_profile();

-- Reusable Traefik HTTP middleware profiles are immutable revisions assigned
-- to exact project/environment/application scopes. Kubernetes object names and
-- secret values never enter this catalog.
CREATE TABLE middleware_profiles (
    id uuid PRIMARY KEY,
    name text NOT NULL CHECK (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','deactivated')),
    current_revision bigint NOT NULL CHECK (current_revision > 0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    deactivated_at timestamptz,
    CHECK ((lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
           (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL))
);
CREATE TABLE middleware_profile_revisions (
    profile_id uuid NOT NULL REFERENCES middleware_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec)='object' AND octet_length(spec::text)<=65536),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    assignments_digest text NOT NULL CHECK (assignments_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    cloned_from_profile_id uuid,
    cloned_from_revision bigint,
    PRIMARY KEY (profile_id,revision),
    FOREIGN KEY (cloned_from_profile_id,cloned_from_revision) REFERENCES middleware_profile_revisions(profile_id,revision) ON DELETE RESTRICT,
    CHECK ((cloned_from_profile_id IS NULL) = (cloned_from_revision IS NULL))
);
ALTER TABLE middleware_profiles ADD CONSTRAINT middleware_profiles_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES middleware_profile_revisions(profile_id,revision) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE middleware_profile_assignments (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal>=0),
    scope_type text NOT NULL CHECK (scope_type IN ('project','environment','application')),
    scope_id uuid NOT NULL,
    PRIMARY KEY (profile_id,revision,ordinal),
    UNIQUE (profile_id,revision,scope_type,scope_id),
    FOREIGN KEY (profile_id,revision) REFERENCES middleware_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);
CREATE OR REPLACE FUNCTION validate_middleware_profile_assignment()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) OR
       (NEW.scope_type='application' AND NOT EXISTS (SELECT 1 FROM applications WHERE id=NEW.scope_id)) THEN
        RAISE EXCEPTION 'middleware profile assignment scope does not exist' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profile_assignment_validate BEFORE INSERT ON middleware_profile_assignments FOR EACH ROW EXECUTE FUNCTION validate_middleware_profile_assignment();

CREATE TABLE middleware_profile_references (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    git_path text NOT NULL CHECK (git_path<>'' AND octet_length(git_path)<=1024),
    logical_name text NOT NULL CHECK (logical_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (git_path,profile_id,logical_name),
    FOREIGN KEY (profile_id,revision) REFERENCES middleware_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);
CREATE INDEX middleware_profile_references_profile_idx ON middleware_profile_references(profile_id,git_path);
CREATE OR REPLACE FUNCTION validate_middleware_profile_reference_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM applications a
        JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id
        JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment'
        WHERE a.id=NEW.application_id
          AND NEW.git_path = b.path_prefix || '/apps/' || NEW.application_id::text || '/app.yaml'
    ) THEN
        RAISE EXCEPTION 'middleware profile reference destination does not match application/environment/Git binding' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profile_reference_scope BEFORE INSERT ON middleware_profile_references FOR EACH ROW EXECUTE FUNCTION validate_middleware_profile_reference_scope();

CREATE TABLE middleware_profile_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise','clone','deactivate')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_id uuid NOT NULL REFERENCES middleware_profiles(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key)
);
CREATE OR REPLACE FUNCTION reject_middleware_profile_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'middleware profile immutable record cannot change' USING ERRCODE='23514'; END; $$;
CREATE TRIGGER middleware_profile_revisions_immutable BEFORE UPDATE OR DELETE ON middleware_profile_revisions FOR EACH ROW EXECUTE FUNCTION reject_middleware_profile_immutable_change();
CREATE TRIGGER middleware_profile_assignments_immutable BEFORE UPDATE OR DELETE ON middleware_profile_assignments FOR EACH ROW EXECUTE FUNCTION reject_middleware_profile_immutable_change();
CREATE TRIGGER middleware_profile_commands_immutable BEFORE UPDATE OR DELETE ON middleware_profile_commands FOR EACH ROW EXECUTE FUNCTION reject_middleware_profile_immutable_change();

CREATE OR REPLACE FUNCTION protect_middleware_profile()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.name<>OLD.name OR NEW.created_by<>OLD.created_by OR NEW.created_at<>OLD.created_at OR OLD.lifecycle='deactivated' OR
       NOT ((NEW.lifecycle='active' AND NEW.current_revision IN (OLD.current_revision,OLD.current_revision+1) AND NEW.deactivated_by IS NULL AND NEW.deactivated_at IS NULL) OR
            (OLD.lifecycle='active' AND NEW.lifecycle='deactivated' AND NEW.current_revision=OLD.current_revision AND NEW.deactivated_by IS NOT NULL AND NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid middleware profile transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profiles_protect BEFORE UPDATE ON middleware_profiles FOR EACH ROW EXECUTE FUNCTION protect_middleware_profile();

CREATE OR REPLACE FUNCTION reject_referenced_middleware_profile_deactivation()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.lifecycle='deactivated' AND OLD.lifecycle='active' AND EXISTS (SELECT 1 FROM middleware_profile_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced middleware profile cannot be deactivated' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER middleware_profiles_referenced BEFORE UPDATE ON middleware_profiles FOR EACH ROW EXECUTE FUNCTION reject_referenced_middleware_profile_deactivation();

-- Immutable image-only auto-deploy policies and replay-safe Git projection
-- webhook wakes. Build workers never receive deployment or Git credentials;
-- the control-plane service account is freshly authorized for every run.

CREATE TABLE auto_deploy_policies (
    id uuid PRIMARY KEY,
    build_definition_id uuid NOT NULL REFERENCES build_definitions(id) ON DELETE RESTRICT,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    application_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    current_revision bigint NOT NULL CHECK (current_revision>0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    UNIQUE (build_definition_id,environment_id),
    FOREIGN KEY (application_id,project_id) REFERENCES applications(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE RESTRICT
);

CREATE TABLE auto_deploy_policy_revisions (
    policy_id uuid NOT NULL REFERENCES auto_deploy_policies(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision>0),
    enabled boolean NOT NULL,
    source_deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    source_deployment_generation bigint NOT NULL CHECK (source_deployment_generation>0),
    source_config_etag text NOT NULL CHECK (source_config_etag ~ '^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$'),
    config_intent bytea NOT NULL CHECK (octet_length(config_intent) BETWEEN 2 AND 262144),
    template_digest text NOT NULL CHECK (template_digest ~ '^sha256:[0-9a-f]{64}$'),
    service_actor_id uuid NOT NULL REFERENCES service_accounts(id) ON DELETE RESTRICT,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (policy_id,revision),
    CHECK (jsonb_typeof(convert_from(config_intent,'UTF8')::jsonb)='object')
);
ALTER TABLE auto_deploy_policies ADD CONSTRAINT auto_deploy_policy_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES auto_deploy_policy_revisions(policy_id,revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE auto_deploy_policy_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    policy_id uuid NOT NULL REFERENCES auto_deploy_policies(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key),
    FOREIGN KEY (policy_id,result_revision) REFERENCES auto_deploy_policy_revisions(policy_id,revision) ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION validate_auto_deploy_policy_revision()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE policy_row auto_deploy_policies%ROWTYPE;
BEGIN
    SELECT * INTO STRICT policy_row FROM auto_deploy_policies WHERE id=NEW.policy_id;
    IF NOT EXISTS (
        SELECT 1 FROM build_definitions b
        JOIN applications a ON a.id=b.service_id AND a.project_id=b.project_id
        JOIN environments e ON e.id=policy_row.environment_id AND e.project_id=b.project_id
        JOIN deployments d ON d.id=NEW.source_deployment_id
             AND d.application_id=a.id AND d.environment_id=e.id AND d.generation=NEW.source_deployment_generation
        JOIN service_accounts sa ON sa.id=NEW.service_actor_id AND sa.project_id=b.project_id AND sa.disabled_at IS NULL
        WHERE b.id=policy_row.build_definition_id AND b.project_id=policy_row.project_id
          AND b.service_id=policy_row.application_id
    ) THEN
        RAISE EXCEPTION 'auto-deploy policy resource binding mismatch' USING ERRCODE='23503';
    END IF;
    IF NEW.created_at<policy_row.created_at THEN
        RAISE EXCEPTION 'auto-deploy revision predates policy' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE CONSTRAINT TRIGGER auto_deploy_policy_revision_validate
    AFTER INSERT ON auto_deploy_policy_revisions DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION validate_auto_deploy_policy_revision();

CREATE OR REPLACE FUNCTION protect_auto_deploy_policy()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.build_definition_id<>OLD.build_definition_id OR NEW.project_id<>OLD.project_id OR
       NEW.application_id<>OLD.application_id OR NEW.environment_id<>OLD.environment_id OR NEW.created_by<>OLD.created_by OR
       NEW.created_at<>OLD.created_at OR NEW.current_revision<>OLD.current_revision+1 THEN
        RAISE EXCEPTION 'invalid auto-deploy policy transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER auto_deploy_policies_protect BEFORE UPDATE ON auto_deploy_policies
    FOR EACH ROW EXECUTE FUNCTION protect_auto_deploy_policy();

CREATE OR REPLACE FUNCTION reject_auto_deploy_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'auto-deploy immutable record cannot change' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER auto_deploy_revisions_immutable BEFORE UPDATE OR DELETE ON auto_deploy_policy_revisions FOR EACH ROW EXECUTE FUNCTION reject_auto_deploy_immutable_change();
CREATE TRIGGER auto_deploy_commands_immutable BEFORE UPDATE OR DELETE ON auto_deploy_policy_commands FOR EACH ROW EXECUTE FUNCTION reject_auto_deploy_immutable_change();

CREATE TABLE auto_deploy_runs (
    attempt_id uuid NOT NULL REFERENCES build_attempts(id) ON DELETE RESTRICT,
    policy_id uuid NOT NULL REFERENCES auto_deploy_policies(id) ON DELETE RESTRICT,
    policy_revision bigint NOT NULL,
    definition_id uuid NOT NULL REFERENCES build_definitions(id) ON DELETE RESTRICT,
    definition_digest text NOT NULL CHECK (definition_digest ~ '^sha256:[0-9a-f]{64}$'),
    release_id uuid NOT NULL REFERENCES registry_releases(id) ON DELETE RESTRICT,
    template_digest text NOT NULL CHECK (template_digest ~ '^sha256:[0-9a-f]{64}$'),
    source_deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    source_deployment_generation bigint NOT NULL CHECK (source_deployment_generation>0),
    source_config_etag text NOT NULL CHECK (source_config_etag ~ '^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$'),
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^auto-deploy/[0-9a-f-]{36}/[1-9][0-9]*/[0-9a-f-]{36}$'),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','processing','submitted','failed')),
    attempts bigint NOT NULL DEFAULT 0 CHECK (attempts>=0),
    available_at timestamptz NOT NULL,
    lease_owner text,
    lease_until timestamptz,
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    operation_id uuid REFERENCES operations(id) ON DELETE RESTRICT,
    deployment_id uuid REFERENCES deployments(id) ON DELETE RESTRICT,
    failure_code text NOT NULL DEFAULT '' CHECK (failure_code='' OR failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    PRIMARY KEY (attempt_id,policy_id),
    UNIQUE (idempotency_key),
    FOREIGN KEY (policy_id,policy_revision) REFERENCES auto_deploy_policy_revisions(policy_id,revision) ON DELETE RESTRICT,
    CHECK ((lease_owner IS NULL)=(lease_until IS NULL)),
    CHECK ((state='processing')=(lease_owner IS NOT NULL)),
    CHECK ((state IN ('submitted','failed'))=(completed_at IS NOT NULL)),
    CHECK ((state='submitted' AND operation_id IS NOT NULL AND deployment_id IS NOT NULL AND failure_code='') OR
           (state='failed' AND operation_id IS NULL AND deployment_id IS NULL AND failure_code<>'') OR
           (state IN ('pending','processing') AND operation_id IS NULL AND deployment_id IS NULL)),
    CHECK (completed_at IS NULL OR completed_at>=created_at),
    CHECK (updated_at>=created_at),
    CHECK (lease_until IS NULL OR lease_until>updated_at)
);
CREATE INDEX auto_deploy_runs_work_idx ON auto_deploy_runs(available_at,created_at,attempt_id,policy_id)
    WHERE state IN ('pending','processing');

CREATE OR REPLACE FUNCTION protect_auto_deploy_run()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending' OR NEW.attempts<>0 OR NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
           NEW.operation_id IS NOT NULL OR NEW.deployment_id IS NOT NULL OR NEW.failure_code<>'' OR NEW.completed_at IS NOT NULL OR
           NEW.updated_at<>NEW.created_at OR NEW.available_at<NEW.created_at THEN
            RAISE EXCEPTION 'auto-deploy run must be inserted pristine and pending' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.attempt_id<>OLD.attempt_id OR NEW.policy_id<>OLD.policy_id OR NEW.policy_revision<>OLD.policy_revision OR
       NEW.definition_id<>OLD.definition_id OR NEW.definition_digest<>OLD.definition_digest OR NEW.release_id<>OLD.release_id OR
       NEW.template_digest<>OLD.template_digest OR NEW.source_deployment_id<>OLD.source_deployment_id OR
       NEW.source_deployment_generation<>OLD.source_deployment_generation OR NEW.source_config_etag<>OLD.source_config_etag OR
       NEW.idempotency_key<>OLD.idempotency_key OR NEW.created_at<>OLD.created_at OR NEW.updated_at<OLD.updated_at OR
       NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'auto-deploy run immutable identity changed' USING ERRCODE='23514';
    END IF;
    IF OLD.state='pending' AND NEW.state='processing' THEN
        IF NEW.attempts<>OLD.attempts+1 OR NEW.lease_epoch<>OLD.lease_epoch+1 OR NEW.lease_owner IS NULL OR
           NEW.available_at<>OLD.available_at OR NEW.failure_code<>'' OR NEW.completed_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid auto-deploy run acquisition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state='processing' THEN
		IF OLD.lease_until>NEW.updated_at AND NEW.attempts=OLD.attempts AND NEW.lease_epoch=OLD.lease_epoch AND NEW.lease_owner=OLD.lease_owner AND NEW.lease_until>OLD.lease_until AND
		   NEW.available_at=OLD.available_at AND NEW.failure_code=OLD.failure_code AND NEW.completed_at IS NOT DISTINCT FROM OLD.completed_at AND
		   NEW.operation_id IS NOT DISTINCT FROM OLD.operation_id AND NEW.deployment_id IS NOT DISTINCT FROM OLD.deployment_id THEN
            NULL; -- heartbeat
		ELSIF OLD.lease_until<=NEW.updated_at AND NEW.attempts=OLD.attempts+1 AND NEW.lease_epoch=OLD.lease_epoch+1 AND NEW.lease_owner IS NOT NULL AND
		   NEW.available_at=OLD.available_at AND NEW.failure_code='' AND NEW.completed_at IS NULL AND NEW.operation_id IS NULL AND NEW.deployment_id IS NULL THEN
            NULL; -- expired-lease recovery
        ELSE RAISE EXCEPTION 'invalid auto-deploy processing transition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state='pending' THEN
		IF OLD.lease_until<=NEW.updated_at OR NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR NEW.lease_owner IS NOT NULL OR
           NEW.available_at<NEW.updated_at OR NEW.failure_code='' OR NEW.completed_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid auto-deploy retry transition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state IN ('submitted','failed') THEN
		IF OLD.lease_until<=NEW.updated_at OR NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR NEW.lease_owner IS NOT NULL OR NEW.completed_at<>NEW.updated_at THEN
            RAISE EXCEPTION 'invalid auto-deploy completion transition' USING ERRCODE='23514'; END IF;
    ELSE
        RAISE EXCEPTION 'invalid auto-deploy run state transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER auto_deploy_runs_protect BEFORE INSERT OR UPDATE OR DELETE ON auto_deploy_runs
    FOR EACH ROW EXECUTE FUNCTION protect_auto_deploy_run();

CREATE OR REPLACE FUNCTION enqueue_auto_deploy_runs()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state<>'succeeded' OR NEW.release_id IS NULL OR
       (TG_OP='UPDATE' AND OLD.state='succeeded') THEN RETURN NEW; END IF;
    INSERT INTO auto_deploy_runs(attempt_id,policy_id,policy_revision,definition_id,definition_digest,release_id,
        template_digest,source_deployment_id,source_deployment_generation,source_config_etag,idempotency_key,available_at,created_at,updated_at)
    SELECT a.id,p.id,p.current_revision,a.definition_id,a.definition_digest,NEW.release_id,r.template_digest,
        r.source_deployment_id,r.source_deployment_generation,r.source_config_etag,
        'auto-deploy/'||p.id::text||'/'||p.current_revision::text||'/'||a.id::text,
        NEW.completed_at,NEW.completed_at,NEW.completed_at
    FROM build_attempts a
    JOIN auto_deploy_policies p ON p.build_definition_id=a.definition_id
    JOIN auto_deploy_policy_revisions r ON r.policy_id=p.id AND r.revision=p.current_revision AND r.enabled
    WHERE a.id=NEW.attempt_id
    ON CONFLICT(attempt_id,policy_id) DO NOTHING;
    RETURN NEW;
END; $$;
CREATE TRIGGER build_release_enqueue_auto_deploy
    AFTER INSERT OR UPDATE OF state ON build_release_projections
    FOR EACH ROW EXECUTE FUNCTION enqueue_auto_deploy_runs();

-- A wake receipt is an HMAC-authenticated hint, never a verified head. Exact
-- binding targets record which monotonic wake generation each receipt caused.
ALTER TABLE git_safety_poll_cursors
    ADD COLUMN wake_generation bigint NOT NULL DEFAULT 0 CHECK (wake_generation>=0),
    ADD COLUMN reconciled_wake_generation bigint NOT NULL DEFAULT 0 CHECK (reconciled_wake_generation>=0),
    ADD CONSTRAINT git_safety_poll_wake_order CHECK (reconciled_wake_generation<=wake_generation);

CREATE TABLE git_projection_push_wakes (
    delivery_hash text PRIMARY KEY CHECK (delivery_hash ~ '^sha256:[0-9a-f]{64}$'),
    github_app_id bigint NOT NULL CHECK (github_app_id>0),
    installation_id bigint NOT NULL CHECK (installation_id>0),
    repository_id bigint NOT NULL CHECK (repository_id>0),
    target_ref text NOT NULL CHECK (target_ref ~ '^refs/heads/'),
    after_commit text NOT NULL CHECK (after_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    received_at timestamptz NOT NULL,
    UNIQUE (github_app_id,installation_id,repository_id,target_ref,after_commit)
);
CREATE TABLE git_projection_push_wake_targets (
    delivery_hash text NOT NULL REFERENCES git_projection_push_wakes(delivery_hash) ON DELETE RESTRICT,
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE RESTRICT,
    wake_generation bigint NOT NULL CHECK (wake_generation>0),
    PRIMARY KEY (delivery_hash,binding_id),
    UNIQUE (binding_id,wake_generation)
);
CREATE OR REPLACE FUNCTION reject_git_push_wake_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    RAISE EXCEPTION 'Git push wake receipts are immutable' USING ERRCODE='23514';
END; $$;
CREATE TRIGGER git_push_wakes_immutable BEFORE UPDATE OR DELETE ON git_projection_push_wakes FOR EACH ROW EXECUTE FUNCTION reject_git_push_wake_change();
CREATE TRIGGER git_push_wake_targets_immutable BEFORE UPDATE OR DELETE ON git_projection_push_wake_targets FOR EACH ROW EXECUTE FUNCTION reject_git_push_wake_change();

-- Exact, lease-fenced readiness for the control-plane auto-deploy controller.
-- Capability and policy mutation surfaces must match this identity; a chart
-- flag alone never proves that the canonical image-only pipeline is running.

CREATE TABLE auto_deploy_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,127}$'),
    contract_version text NOT NULL CHECK (contract_version='auto-deploy.v1'),
    operator_config_digest text NOT NULL CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_epoch bigint NOT NULL CHECK (lease_epoch>0),
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at),
    CHECK (lease_until>observed_at),
    CHECK (lease_until<=observed_at+interval '5 minutes')
);

CREATE OR REPLACE FUNCTION protect_auto_deploy_runtime_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.observed_at>clock_timestamp()+interval '30 seconds' OR NEW.started_at>NEW.observed_at THEN
        RAISE EXCEPTION 'auto-deploy readiness timestamp is invalid' USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.lease_epoch<>1 THEN
            RAISE EXCEPTION 'auto-deploy readiness must start at epoch one' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' OR NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'auto-deploy readiness identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.contract_version=OLD.contract_version AND NEW.operator_config_digest=OLD.operator_config_digest AND
       NEW.started_at=OLD.started_at THEN
        IF OLD.lease_until>NEW.observed_at AND NEW.lease_epoch=OLD.lease_epoch AND
           NEW.observed_at>OLD.observed_at AND NEW.lease_until>OLD.lease_until THEN
            NULL; -- active heartbeat
        ELSIF OLD.lease_until<=NEW.observed_at AND NEW.lease_epoch=OLD.lease_epoch+1 AND
              NEW.observed_at>OLD.observed_at AND NEW.lease_until>NEW.observed_at THEN
            NULL; -- expired lease reacquisition by the same process identity
        ELSE
            RAISE EXCEPTION 'invalid auto-deploy readiness heartbeat' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.started_at<=OLD.started_at OR NEW.observed_at<NEW.started_at OR NEW.lease_epoch<>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'invalid auto-deploy readiness identity replacement' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END; $$;

CREATE TRIGGER auto_deploy_runtime_readiness_protect
    BEFORE INSERT OR UPDATE OR DELETE ON auto_deploy_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_auto_deploy_runtime_readiness();

-- Human-managed project/environment VariableSets use the same fenced Git and
-- protected-publication machinery as AppConfig, but retain a closed command
-- shape so no generic arbitrary-path writer is introduced. Project scope is
-- deliberately tied to one concrete environment binding/repository in MVP.
CREATE TABLE git_variable_write_commands (
    operation_id uuid PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    scope text NOT NULL CHECK (scope IN ('project','environment')),
    target_ref text NOT NULL,
    path text NOT NULL CHECK (
        (scope='project' AND path='tenants/'||project_id::text||'/variables.yaml') OR
        (scope='environment' AND path='tenants/'||project_id::text||'/environments/'||environment_id::text||'/variables.yaml')
    ),
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    precondition text NOT NULL CHECK (precondition IN ('match-etag','create-if-absent')),
    expected_etag text NOT NULL CHECK (
        (precondition='match-etag' AND expected_etag ~ '^"sha256:[0-9a-f]{64}"$') OR
        (precondition='create-if-absent' AND expected_etag='')
    ),
    parser_version text NOT NULL CHECK (length(parser_version) BETWEEN 1 AND 128 AND parser_version !~ E'[\\x00\\r\\n]'),
    content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 131072),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    message text NOT NULL CHECK (length(message) BETWEEN 1 AND 512 AND message !~ E'[\\x00\\r]'),
    publication_mode text NOT NULL CHECK (publication_mode IN ('direct','pull-request')),
    state text NOT NULL CHECK (state IN ('pending','git-committed','indexed')),
    committed_revision text NOT NULL DEFAULT '' CHECK (committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    committed_at timestamptz,
    indexed_generation bigint NOT NULL DEFAULT 0 CHECK (indexed_generation>=0),
    indexed_at timestamptz,
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (binding_id,target_ref) REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (binding_id,project_id,environment_id) REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at),
    CHECK (
        (state='pending' AND committed_revision='' AND committed_at IS NULL AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='git-committed' AND committed_revision<>'' AND committed_at IS NOT NULL AND committed_at>=created_at AND indexed_generation=0 AND indexed_at IS NULL) OR
        (state='indexed' AND committed_revision<>'' AND committed_at IS NOT NULL AND committed_at>=created_at AND indexed_generation>0 AND indexed_at IS NOT NULL AND indexed_at>=committed_at)
    )
);
CREATE INDEX git_variable_write_commands_binding_state_idx ON git_variable_write_commands(binding_id,state,created_at,operation_id);
CREATE INDEX git_variable_write_commands_committed_idx ON git_variable_write_commands(binding_id,committed_revision) WHERE state IN ('git-committed','indexed');

CREATE OR REPLACE FUNCTION protect_git_variable_write_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP='DELETE' THEN
		RAISE EXCEPTION 'Git VariableSet write commands are immutable' USING ERRCODE='23514';
	END IF;
	IF TG_OP='INSERT' THEN
		IF NEW.state<>'pending' OR NEW.committed_revision<>'' OR NEW.committed_at IS NOT NULL OR NEW.indexed_generation<>0 OR NEW.indexed_at IS NOT NULL OR NEW.updated_at<>NEW.created_at THEN
			RAISE EXCEPTION 'Git VariableSet write command must start pristine pending' USING ERRCODE='23514';
		END IF;
		RETURN NEW;
	END IF;
    IF ROW(NEW.operation_id,NEW.actor_id,NEW.binding_id,NEW.project_id,NEW.environment_id,NEW.scope,NEW.target_ref,
           NEW.path,NEW.base_revision,NEW.precondition,NEW.expected_etag,NEW.parser_version,NEW.content,NEW.content_sha256,
           NEW.message,NEW.publication_mode,NEW.request_digest,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.actor_id,OLD.binding_id,OLD.project_id,OLD.environment_id,OLD.scope,OLD.target_ref,
           OLD.path,OLD.base_revision,OLD.precondition,OLD.expected_etag,OLD.parser_version,OLD.content,OLD.content_sha256,
           OLD.message,OLD.publication_mode,OLD.request_digest,OLD.created_at) THEN
        RAISE EXCEPTION 'Git VariableSet write command identity is immutable' USING ERRCODE='23514';
    END IF;
	IF NEW.updated_at<OLD.updated_at OR NOT (
		(OLD.publication_mode='direct' AND OLD.state='pending' AND NEW.state='git-committed') OR
		(OLD.publication_mode='direct' AND OLD.state='git-committed' AND NEW.state='indexed') OR
		(OLD.publication_mode='pull-request' AND OLD.state='pending' AND NEW.state='indexed' AND EXISTS (
			SELECT 1 FROM git_pull_request_publications p WHERE p.operation_id=OLD.operation_id AND p.state='merge-verified'
		))
	) THEN RAISE EXCEPTION 'invalid Git VariableSet write command transition' USING ERRCODE='23514'; END IF;
    IF OLD.committed_revision<>'' AND (NEW.committed_revision<>OLD.committed_revision OR NEW.committed_at<>OLD.committed_at) THEN
        RAISE EXCEPTION 'Git VariableSet write result is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER git_variable_write_commands_protect BEFORE INSERT OR UPDATE OR DELETE ON git_variable_write_commands
FOR EACH ROW EXECUTE FUNCTION protect_git_variable_write_command();

CREATE OR REPLACE FUNCTION validate_git_variable_write_operation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE expected_target uuid;
BEGIN
	expected_target := CASE WHEN NEW.scope='project' THEN NEW.project_id ELSE NEW.environment_id END;
	IF NOT EXISTS (SELECT 1 FROM operations o WHERE o.id=NEW.operation_id AND o.kind='variable-set.git-write'
		AND o.target_type=NEW.scope AND o.target_id=expected_target AND o.status='queued' AND o.generation=1
		AND o.lease_owner IS NULL AND o.lease_until IS NULL) THEN
		RAISE EXCEPTION 'Git VariableSet command operation identity mismatch' USING ERRCODE='23514';
	END IF;
	IF NOT EXISTS (SELECT 1 FROM idempotency_keys i WHERE i.operation_id=NEW.operation_id AND i.actor_id=NEW.actor_id
		AND i.fingerprint=NEW.request_digest) THEN
		RAISE EXCEPTION 'Git VariableSet command request authority mismatch' USING ERRCODE='23514';
	END IF;
	RETURN NEW;
END;
$$;
CREATE CONSTRAINT TRIGGER git_variable_write_commands_operation
AFTER INSERT ON git_variable_write_commands DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION validate_git_variable_write_operation();

CREATE TABLE variable_set_previews (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash)=32),
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL REFERENCES git_repository_bindings(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    scope text NOT NULL CHECK (scope IN ('project','environment')),
    path text NOT NULL,
    base_revision text NOT NULL CHECK (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    base_etag text NOT NULL CHECK (base_etag='' OR base_etag ~ '^"sha256:[0-9a-f]{64}"$'),
    parser_version text NOT NULL CHECK (length(parser_version) BETWEEN 1 AND 128 AND parser_version !~ E'[\\x00\\r\\n]'),
    candidate_hash bytea NOT NULL CHECK (octet_length(candidate_hash)=32),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL,
    FOREIGN KEY (binding_id,project_id,environment_id) REFERENCES git_repository_bindings(id,project_id,environment_id) ON DELETE CASCADE,
    CHECK ((scope='project' AND path='tenants/'||project_id::text||'/variables.yaml') OR
           (scope='environment' AND path='tenants/'||project_id::text||'/environments/'||environment_id::text||'/variables.yaml')),
    CHECK (expires_at>created_at AND (consumed_at IS NULL OR consumed_at>=created_at))
);

CREATE OR REPLACE FUNCTION protect_variable_set_preview()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP='DELETE' THEN RAISE EXCEPTION 'VariableSet previews are immutable' USING ERRCODE='23514'; END IF;
	IF TG_OP='INSERT' THEN
		IF NEW.consumed_at IS NOT NULL OR NEW.created_at>now()+interval '1 minute' OR NEW.expires_at>NEW.created_at+interval '15 minutes' THEN
			RAISE EXCEPTION 'VariableSet preview must start pristine and bounded' USING ERRCODE='23514';
		END IF;
		RETURN NEW;
	END IF;
	IF ROW(NEW.token_hash,NEW.actor_id,NEW.binding_id,NEW.project_id,NEW.environment_id,NEW.scope,NEW.path,
		NEW.base_revision,NEW.base_etag,NEW.parser_version,NEW.candidate_hash,NEW.expires_at,NEW.created_at)
		IS DISTINCT FROM ROW(OLD.token_hash,OLD.actor_id,OLD.binding_id,OLD.project_id,OLD.environment_id,OLD.scope,OLD.path,
		OLD.base_revision,OLD.base_etag,OLD.parser_version,OLD.candidate_hash,OLD.expires_at,OLD.created_at) OR
		OLD.consumed_at IS NOT NULL OR NEW.consumed_at IS NULL OR NEW.consumed_at<OLD.created_at OR NEW.consumed_at>now()+interval '1 minute' THEN
		RAISE EXCEPTION 'VariableSet preview update is not an exact consumption' USING ERRCODE='23514';
	END IF;
	RETURN NEW;
END;
$$;
CREATE TRIGGER variable_set_previews_protect BEFORE INSERT OR UPDATE OR DELETE ON variable_set_previews
FOR EACH ROW EXECUTE FUNCTION protect_variable_set_preview();

-- Protected publication receipts may belong to either of the two closed Git
-- command families. The constraint trigger rejects orphan/substituted rows.
ALTER TABLE git_pull_request_publications DROP CONSTRAINT git_pull_request_publications_operation_id_fkey;
ALTER TABLE git_pull_request_publications ADD CONSTRAINT git_pull_request_publications_operation_id_fkey
    FOREIGN KEY (operation_id) REFERENCES operations(id) ON DELETE CASCADE;
CREATE OR REPLACE FUNCTION require_closed_git_publication_command()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ((SELECT count(*) FROM git_deployment_write_commands WHERE operation_id=NEW.operation_id) +
        (SELECT count(*) FROM git_variable_write_commands WHERE operation_id=NEW.operation_id)) <> 1 THEN
        RAISE EXCEPTION 'Git publication must reference exactly one closed command' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE CONSTRAINT TRIGGER git_pull_request_publications_closed_command
AFTER INSERT OR UPDATE ON git_pull_request_publications DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION require_closed_git_publication_command();

CREATE OR REPLACE FUNCTION protect_git_pull_request_publication()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
		IF NEW.state<>'pending-candidate' OR NEW.write_base_revision<>'' OR NEW.candidate_revision<>'' OR
		   NEW.pull_request_number<>0 OR NEW.pull_request_url<>'' OR NEW.pull_request_state<>'' OR NEW.merge_revision<>'' OR
		   NEW.target_revision<>'' OR NEW.provider_observed_at IS NOT NULL OR NEW.version<>1 OR NEW.updated_at<>NEW.created_at THEN
			RAISE EXCEPTION 'pull request publication must start pristine pending-candidate' USING ERRCODE='23514';
		END IF;
        IF ((SELECT count(*) FROM git_deployment_write_commands c JOIN git_repository_bindings b ON b.id=c.binding_id
              WHERE c.operation_id=NEW.operation_id AND c.publication_mode='pull-request' AND c.binding_id=NEW.binding_id
                AND c.target_ref=NEW.target_ref AND c.base_revision=NEW.base_revision AND b.provider=NEW.provider
                AND b.installation_id=NEW.installation_id AND b.repository_id=NEW.repository_id
                AND b.repository_owner=NEW.repository_owner AND b.repository_name=NEW.repository_name) +
            (SELECT count(*) FROM git_variable_write_commands c JOIN git_repository_bindings b ON b.id=c.binding_id
              WHERE c.operation_id=NEW.operation_id AND c.publication_mode='pull-request' AND c.binding_id=NEW.binding_id
                AND c.target_ref=NEW.target_ref AND c.base_revision=NEW.base_revision AND b.provider=NEW.provider
                AND b.installation_id=NEW.installation_id AND b.repository_id=NEW.repository_id
                AND b.repository_owner=NEW.repository_owner AND b.repository_name=NEW.repository_name)) <> 1 THEN
            RAISE EXCEPTION 'pull request publication identity does not match exactly one protected command' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.operation_id,NEW.binding_id,NEW.provider,NEW.installation_id,NEW.repository_id,NEW.repository_owner,NEW.repository_name,NEW.target_ref,NEW.base_revision,NEW.candidate_ref,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.operation_id,OLD.binding_id,OLD.provider,OLD.installation_id,OLD.repository_id,OLD.repository_owner,OLD.repository_name,OLD.target_ref,OLD.base_revision,OLD.candidate_ref,OLD.created_at) THEN
        RAISE EXCEPTION 'pull request publication identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN RAISE EXCEPTION 'pull request publication update is not fenced' USING ERRCODE='23514'; END IF;
    IF OLD.write_base_revision<>'' AND NEW.write_base_revision<>OLD.write_base_revision THEN RAISE EXCEPTION 'pull request write base is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.candidate_revision<>'' AND NEW.candidate_revision<>OLD.candidate_revision THEN RAISE EXCEPTION 'pull request candidate revision is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.pull_request_number>0 AND (NEW.pull_request_number<>OLD.pull_request_number OR NEW.pull_request_url<>OLD.pull_request_url) THEN RAISE EXCEPTION 'pull request identity is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.merge_revision<>'' AND NEW.merge_revision<>OLD.merge_revision THEN RAISE EXCEPTION 'pull request merge revision is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.target_revision<>'' AND NEW.target_revision<>OLD.target_revision THEN RAISE EXCEPTION 'verified target revision is immutable' USING ERRCODE='23514'; END IF;
    IF NOT ((OLD.state='pending-candidate' AND NEW.state='write-base-ready') OR
        (OLD.state='write-base-ready' AND NEW.state='candidate-ready') OR
        (OLD.state='candidate-ready' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-open' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-closed' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='merge-pending' AND NEW.state IN ('merge-pending','merge-verified')) OR
        (OLD.state='merge-verified' AND NEW.state='merge-verified')) THEN
        RAISE EXCEPTION 'invalid pull request publication transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- Platform-admin cert-manager ClusterIssuer profiles. Desired specifications
-- are immutable revisions; tenant callers only see readiness-gated identities.
CREATE TABLE cert_manager_issuer_profiles (
    id uuid PRIMARY KEY,
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','deactivated')),
    current_revision bigint NOT NULL CHECK (current_revision > 0),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    deactivated_at timestamptz,
    CHECK ((lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
           (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL))
);

CREATE TABLE cert_manager_issuer_profile_revisions (
    profile_id uuid NOT NULL REFERENCES cert_manager_issuer_profiles(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    solver_type text NOT NULL CHECK (solver_type IN ('http01','dns01-cloudflare')),
    spec jsonb NOT NULL CHECK (jsonb_typeof(spec)='object' AND octet_length(spec::text)<=32768),
    spec_digest text NOT NULL CHECK (spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (profile_id,revision)
);
ALTER TABLE cert_manager_issuer_profiles ADD CONSTRAINT cert_manager_issuer_profiles_current_revision_fk
    FOREIGN KEY (id,current_revision) REFERENCES cert_manager_issuer_profile_revisions(profile_id,revision) DEFERRABLE INITIALLY DEFERRED;

-- One mutable observation per desired revision. It is never tenant authority:
-- the catalog requires an exact ready digest before returning an identity.
CREATE TABLE cert_manager_issuer_observations (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    state text NOT NULL CHECK (state IN ('pending','ready','degraded')),
    observed_spec_digest text CHECK (observed_spec_digest IS NULL OR observed_spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    observed_generation bigint CHECK (observed_generation IS NULL OR observed_generation>0),
    reason text NOT NULL DEFAULT '' CHECK (octet_length(reason)<=1024),
    observed_at timestamptz,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (profile_id,revision),
    FOREIGN KEY (profile_id,revision) REFERENCES cert_manager_issuer_profile_revisions(profile_id,revision) ON DELETE RESTRICT,
    CHECK ((state='pending' AND observed_spec_digest IS NULL AND observed_generation IS NULL AND observed_at IS NULL) OR
           (state IN ('ready','degraded') AND observed_spec_digest IS NOT NULL AND observed_generation IS NOT NULL AND observed_at IS NOT NULL))
);

CREATE TABLE cert_manager_issuer_references (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    application_id uuid NOT NULL REFERENCES applications(id) ON DELETE RESTRICT,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    git_path text NOT NULL CHECK (git_path<>'' AND octet_length(git_path)<=1024),
    hostname text NOT NULL CHECK (hostname<>'' AND octet_length(hostname)<=253),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (git_path,hostname),
    FOREIGN KEY (profile_id,revision) REFERENCES cert_manager_issuer_profile_revisions(profile_id,revision) ON DELETE RESTRICT
);
CREATE INDEX cert_manager_issuer_references_profile_idx ON cert_manager_issuer_references(profile_id,git_path);
CREATE OR REPLACE FUNCTION validate_cert_manager_issuer_reference_scope()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM applications a JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment' WHERE a.id=NEW.application_id AND NEW.git_path=b.path_prefix || '/apps/' || a.id::text || '/app.yaml') THEN
        RAISE EXCEPTION 'cert-manager issuer reference destination mismatch' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_reference_scope BEFORE INSERT ON cert_manager_issuer_references FOR EACH ROW EXECUTE FUNCTION validate_cert_manager_issuer_reference_scope();

CREATE TABLE cert_manager_issuer_commands (
    actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    action text NOT NULL CHECK (action IN ('create','revise','deactivate')),
    request_digest text NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    profile_id uuid NOT NULL REFERENCES cert_manager_issuer_profiles(id) ON DELETE RESTRICT,
    result_revision bigint NOT NULL CHECK (result_revision>0),
    request_id text NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id,idempotency_key)
);
CREATE OR REPLACE FUNCTION reject_cert_manager_issuer_immutable_change()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cert-manager issuer immutable record cannot change' USING ERRCODE='23514'; END; $$;
CREATE TRIGGER cert_manager_issuer_revisions_immutable BEFORE UPDATE OR DELETE ON cert_manager_issuer_profile_revisions FOR EACH ROW EXECUTE FUNCTION reject_cert_manager_issuer_immutable_change();
CREATE TRIGGER cert_manager_issuer_commands_immutable BEFORE UPDATE OR DELETE ON cert_manager_issuer_commands FOR EACH ROW EXECUTE FUNCTION reject_cert_manager_issuer_immutable_change();

CREATE OR REPLACE FUNCTION protect_cert_manager_issuer_profile()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.name<>OLD.name OR NEW.created_by<>OLD.created_by OR NEW.created_at<>OLD.created_at OR OLD.lifecycle='deactivated' OR
       NOT ((NEW.lifecycle='active' AND NEW.current_revision IN (OLD.current_revision,OLD.current_revision+1) AND NEW.deactivated_by IS NULL AND NEW.deactivated_at IS NULL) OR
            (OLD.lifecycle='active' AND NEW.lifecycle='deactivated' AND NEW.current_revision=OLD.current_revision AND NEW.deactivated_by IS NOT NULL AND NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid cert-manager issuer profile transition' USING ERRCODE='23514';
    END IF;
    IF (NEW.lifecycle='deactivated' OR NEW.current_revision<>OLD.current_revision) AND EXISTS (SELECT 1 FROM cert_manager_issuer_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced cert-manager issuer profile cannot be revised or deactivated' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_profiles_protect BEFORE UPDATE ON cert_manager_issuer_profiles FOR EACH ROW EXECUTE FUNCTION protect_cert_manager_issuer_profile();

-- The SQL boundary also enforces platform-admin authority even if a future
-- transport accidentally calls the store with a tenant actor.
CREATE OR REPLACE FUNCTION require_cert_manager_issuer_platform_admin()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin') THEN
        RAISE EXCEPTION 'cert-manager issuer mutation requires platform-admin' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_commands_admin BEFORE INSERT ON cert_manager_issuer_commands FOR EACH ROW EXECUTE FUNCTION require_cert_manager_issuer_platform_admin();

-- Durable proof that a production worker is observing the exact dynamic
-- ClusterIssuer catalog. API capability and admin mutations fail closed when
-- this lease is absent, stale, superseded, or bound to a different runtime
-- identity/target set.
CREATE TABLE cert_manager_issuer_observer_readiness (
    worker_id text PRIMARY KEY CHECK (worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    contract_version text NOT NULL CHECK (contract_version='cert-manager-cluster-issuer-observer.v1'),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    target_digest text NOT NULL CHECK (target_digest ~ '^sha256:[0-9a-f]{64}$'),
    target_count integer NOT NULL CHECK (target_count>=0 AND target_count<=128),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_epoch bigint NOT NULL CHECK (lease_epoch>0),
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (config_digest),
    CHECK (observed_at>=started_at AND updated_at>=observed_at AND lease_until>observed_at AND lease_until<=observed_at+interval '15 minutes')
);
CREATE INDEX cert_manager_issuer_observer_readiness_match_idx
    ON cert_manager_issuer_observer_readiness(contract_version,config_digest,target_digest,target_count,observed_at,lease_until);

CREATE OR REPLACE FUNCTION protect_cert_manager_issuer_observer_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'cert-manager issuer observer readiness cannot be deleted' USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.lease_epoch<>1 OR NEW.updated_at<>NEW.observed_at OR NEW.observed_at>clock_timestamp()+interval '30 seconds' THEN
            RAISE EXCEPTION 'invalid initial cert-manager issuer observer readiness lease' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.observed_at<OLD.observed_at OR NEW.updated_at<OLD.updated_at OR
       NEW.observed_at>clock_timestamp()+interval '30 seconds' THEN
        RAISE EXCEPTION 'invalid cert-manager issuer observer readiness mutation' USING ERRCODE='23514';
    END IF;
    IF NEW.worker_id=OLD.worker_id AND NEW.contract_version=OLD.contract_version AND NEW.config_digest=OLD.config_digest AND
       NEW.started_at=OLD.started_at AND NEW.lease_epoch=OLD.lease_epoch THEN
        IF OLD.lease_until<=NEW.observed_at OR NEW.updated_at<>NEW.observed_at OR NEW.lease_until<=OLD.lease_until THEN
            RAISE EXCEPTION 'invalid cert-manager issuer observer heartbeat' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.contract_version<>OLD.contract_version OR NEW.config_digest<>OLD.config_digest OR
       OLD.lease_until>NEW.observed_at OR NEW.lease_epoch<>OLD.lease_epoch+1 OR
       NEW.started_at<OLD.started_at OR NEW.updated_at<>NEW.observed_at THEN
        RAISE EXCEPTION 'invalid cert-manager issuer observer lease replacement' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_observer_readiness_protect
BEFORE INSERT OR UPDATE OR DELETE ON cert_manager_issuer_observer_readiness
FOR EACH ROW EXECUTE FUNCTION protect_cert_manager_issuer_observer_readiness();

-- ExternalDNS readiness must attest the provider and managed reference
-- identities recorded by the platform integration. No Secret values or
-- provider configuration payloads are persisted or read here.
ALTER TABLE edge_runtime_targets
    ADD COLUMN external_provider_kind text NOT NULL DEFAULT '',
    ADD COLUMN external_credential_secret_ref text NOT NULL DEFAULT '',
    ADD COLUMN external_provider_config_ref text NOT NULL DEFAULT '',
    ADD COLUMN external_egress_config_ref text NOT NULL DEFAULT '';

-- Observations made under the previous, weaker identity contract cannot stay
-- active. Preserve their audit receipts, but require a new profile revision
-- whose digest covers the added fields before readiness can recover.
UPDATE edge_runtime_targets target
   SET external_provider_kind=integration.provider_kind,
       external_credential_secret_ref=COALESCE(integration.credential_secret_ref,''),
       external_provider_config_ref=COALESCE(integration.provider_config_ref,''),
       external_egress_config_ref=COALESCE(integration.egress_config_ref,''),
       active=false,runtime_state='awaiting',lease_owner=NULL,lease_until=NULL,
       worker_contract=NULL,worker_config_digest=NULL,updated_at=GREATEST(target.updated_at,now())
  FROM external_dns_integrations integration
 WHERE target.kind='external-dns' AND target.integration_id=integration.id;

ALTER TABLE edge_runtime_targets ADD CONSTRAINT edge_runtime_external_dns_identity_v2 CHECK (
    (kind<>'external-dns' AND external_provider_kind='' AND
     external_credential_secret_ref='' AND external_provider_config_ref='' AND external_egress_config_ref='') OR
    (kind='external-dns' AND external_provider_kind IN ('aws','azure','cloudflare','google','rfc2136') AND (
        (management_mode='managed' AND
         external_credential_secret_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         external_provider_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         external_egress_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$') OR
        (management_mode='adopted' AND external_credential_secret_ref='' AND
         external_provider_config_ref='' AND external_egress_config_ref='')
    ))
);

CREATE OR REPLACE FUNCTION validate_edge_runtime_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_mode text;
    durable_provider_kind text;
    durable_txt_owner text;
    durable_policy text;
    durable_credential_ref text;
    durable_provider_ref text;
    durable_egress_ref text;
    durable_profile text;
    durable_domains text;
BEGIN
    IF NEW.kind='external-dns' AND NEW.active THEN
        SELECT i.mode,i.provider_kind,i.txt_owner_id,i.sync_policy,
               COALESCE(i.credential_secret_ref,''),COALESCE(i.provider_config_ref,''),
               COALESCE(i.egress_config_ref,''),COALESCE(i.operator_profile_ref,''),
               COALESCE((
                   SELECT string_agg(suffix.value,',' ORDER BY suffix.value)
                     FROM jsonb_array_elements_text(i.allowed_domain_suffixes) AS suffix(value)
               ),'')
          INTO durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,
               durable_credential_ref,durable_provider_ref,durable_egress_ref,
               durable_profile,durable_domains
          FROM external_dns_integrations i
         WHERE i.id=NEW.integration_id;
        IF NOT FOUND OR
           ROW(NEW.management_mode,NEW.external_provider_kind,NEW.external_txt_owner_id,
               NEW.external_policy,NEW.external_domains)
           IS DISTINCT FROM
           ROW(durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,durable_domains) OR
           (NEW.management_mode='adopted' AND
            (NEW.profile_config_map<>durable_profile OR durable_credential_ref<>'' OR
             durable_provider_ref<>'' OR durable_egress_ref<>'')) OR
           (NEW.management_mode='managed' AND
            (durable_profile<>'' OR
             ROW(NEW.external_credential_secret_ref,NEW.external_provider_config_ref,NEW.external_egress_config_ref)
             IS DISTINCT FROM ROW(durable_credential_ref,durable_provider_ref,durable_egress_ref))) THEN
            RAISE EXCEPTION 'External DNS edge target does not match its safe integration metadata'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.kind,NEW.integration_id,
               NEW.management_mode,NEW.namespace,NEW.profile_config_map,
               NEW.external_txt_owner_id,NEW.external_policy,NEW.external_domains,
               NEW.external_provider_kind,NEW.external_credential_secret_ref,
               NEW.external_provider_config_ref,NEW.external_egress_config_ref,
               NEW.desired_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.kind,OLD.integration_id,
               OLD.management_mode,OLD.namespace,OLD.profile_config_map,
               OLD.external_txt_owner_id,OLD.external_policy,OLD.external_domains,
               OLD.external_provider_kind,OLD.external_credential_secret_ref,
               OLD.external_provider_config_ref,OLD.external_egress_config_ref,
               OLD.desired_digest,OLD.created_at) THEN
            RAISE EXCEPTION 'Edge runtime target identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Edge runtime target lease epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Edge runtime target lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.observed_identity_digest<>'' AND
           NEW.observed_identity_digest<>OLD.observed_identity_digest THEN
            RAISE EXCEPTION 'Edge runtime observed Kubernetes identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.last_observed_at IS NOT NULL AND
            (NEW.last_observed_at IS NULL OR NEW.last_observed_at<OLD.last_observed_at)) THEN
            RAISE EXCEPTION 'Edge runtime target time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

-- Make ExternalDNS integrations operational, revisioned desired state. The
-- credential columns continue to store Kubernetes object names only; Secret
-- values and provider payloads never enter Kuberploy's database.
ALTER TABLE external_dns_integrations
    ADD COLUMN runtime_revision bigint NOT NULL DEFAULT 1 CHECK (runtime_revision > 0),
    ADD COLUMN lifecycle text NOT NULL DEFAULT 'active'
        CHECK (lifecycle IN ('active','deactivated')),
    ADD COLUMN deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN deactivated_at timestamptz,
    ADD COLUMN protected_git_state text NOT NULL DEFAULT 'pending'
        CHECK (protected_git_state IN ('pending','materialized','dematerialized')),
    ADD COLUMN protected_git_revision bigint,
    ADD COLUMN protected_git_content_digest text NOT NULL DEFAULT '',
    ADD COLUMN protected_git_commit text NOT NULL DEFAULT '',
    ADD COLUMN protected_git_observed_at timestamptz,
    ADD CONSTRAINT external_dns_lifecycle_consistent CHECK (
        (lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
        (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL)
    ),
    ADD CONSTRAINT external_dns_protected_git_receipt CHECK (
      (protected_git_state='pending' AND protected_git_revision IS NULL AND
       protected_git_content_digest='' AND protected_git_commit='' AND protected_git_observed_at IS NULL) OR
      (protected_git_state='materialized' AND protected_git_revision=runtime_revision AND
       protected_git_content_digest ~ '^sha256:[0-9a-f]{64}$' AND
       protected_git_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$' AND protected_git_observed_at IS NOT NULL) OR
      (protected_git_state='dematerialized' AND lifecycle='deactivated' AND
       protected_git_revision=runtime_revision AND protected_git_content_digest='' AND
       protected_git_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$' AND protected_git_observed_at IS NOT NULL)
    );

CREATE INDEX external_dns_integrations_active_runtime_idx
    ON external_dns_integrations(runtime_revision,id) WHERE lifecycle='active';

CREATE OR REPLACE FUNCTION protect_external_dns_integration_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    desired_changed boolean;
BEGIN
    IF ROW(NEW.id,NEW.slug,NEW.txt_owner_id,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.slug,OLD.txt_owner_id,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'external-dns integration identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at OR
       (OLD.deactivated_at IS NOT NULL AND NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) OR
       (OLD.lifecycle='deactivated' AND NEW.lifecycle<>'deactivated') THEN
        RAISE EXCEPTION 'external-dns integration lifecycle cannot regress' USING ERRCODE='23514';
    END IF;
    desired_changed := ROW(NEW.name,NEW.mode,NEW.provider_kind,NEW.allowed_domain_suffixes,
        NEW.sync_policy,NEW.destructive_sync_confirmed,NEW.credential_secret_ref,
        NEW.provider_config_ref,NEW.egress_config_ref,NEW.operator_profile_ref)
      IS DISTINCT FROM ROW(OLD.name,OLD.mode,OLD.provider_kind,OLD.allowed_domain_suffixes,
        OLD.sync_policy,OLD.destructive_sync_confirmed,OLD.credential_secret_ref,
        OLD.provider_config_ref,OLD.egress_config_ref,OLD.operator_profile_ref);
    IF desired_changed AND NEW.runtime_revision <> OLD.runtime_revision + 1 OR
       NOT desired_changed AND NEW.runtime_revision <> OLD.runtime_revision THEN
        RAISE EXCEPTION 'external-dns runtime revision is not an exact desired-state revision' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- An active runtime target must attest the current durable revision. A UI
-- update therefore makes the old observation ineligible immediately.
CREATE OR REPLACE FUNCTION validate_edge_runtime_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_mode text;
    durable_provider_kind text;
    durable_txt_owner text;
    durable_policy text;
    durable_credential_ref text;
    durable_provider_ref text;
    durable_egress_ref text;
    durable_profile text;
    durable_domains text;
    durable_revision bigint;
    durable_lifecycle text;
BEGIN
    IF NEW.kind='external-dns' AND NEW.active THEN
        SELECT i.mode,i.provider_kind,i.txt_owner_id,i.sync_policy,
               COALESCE(i.credential_secret_ref,''),COALESCE(i.provider_config_ref,''),
               COALESCE(i.egress_config_ref,''),COALESCE(i.operator_profile_ref,''),
               COALESCE((SELECT string_agg(suffix.value,',' ORDER BY suffix.value)
                 FROM jsonb_array_elements_text(i.allowed_domain_suffixes) AS suffix(value)),''),
               i.runtime_revision,i.lifecycle
          INTO durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,
               durable_credential_ref,durable_provider_ref,durable_egress_ref,
               durable_profile,durable_domains,durable_revision,durable_lifecycle
          FROM external_dns_integrations i WHERE i.id=NEW.integration_id;
        IF NOT FOUND OR durable_lifecycle<>'active' OR NEW.profile_revision<>durable_revision OR
           ROW(NEW.management_mode,NEW.external_provider_kind,NEW.external_txt_owner_id,
               NEW.external_policy,NEW.external_domains)
           IS DISTINCT FROM ROW(durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,durable_domains) OR
           (NEW.management_mode='adopted' AND
            (NEW.profile_config_map<>durable_profile OR durable_credential_ref<>'' OR
             durable_provider_ref<>'' OR durable_egress_ref<>'')) OR
           (NEW.management_mode='managed' AND
            (durable_profile<>'' OR ROW(NEW.external_credential_secret_ref,
             NEW.external_provider_config_ref,NEW.external_egress_config_ref)
             IS DISTINCT FROM ROW(durable_credential_ref,durable_provider_ref,durable_egress_ref))) THEN
            RAISE EXCEPTION 'External DNS edge target does not match its current safe integration revision'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.kind,NEW.integration_id,
               NEW.management_mode,NEW.namespace,NEW.profile_config_map,
               NEW.external_txt_owner_id,NEW.external_policy,NEW.external_domains,
               NEW.external_provider_kind,NEW.external_credential_secret_ref,
               NEW.external_provider_config_ref,NEW.external_egress_config_ref,
               NEW.desired_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.kind,OLD.integration_id,
               OLD.management_mode,OLD.namespace,OLD.profile_config_map,
               OLD.external_txt_owner_id,OLD.external_policy,OLD.external_domains,
               OLD.external_provider_kind,OLD.external_credential_secret_ref,
               OLD.external_provider_config_ref,OLD.external_egress_config_ref,
               OLD.desired_digest,OLD.created_at) THEN
            RAISE EXCEPTION 'Edge runtime target identity is immutable' USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Edge runtime target lease epoch is invalid' USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest) IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Edge runtime target lease identity changed without a new epoch' USING ERRCODE='23514';
        END IF;
        IF OLD.observed_identity_digest<>'' AND NEW.observed_identity_digest<>OLD.observed_identity_digest THEN
            RAISE EXCEPTION 'Edge runtime observed Kubernetes identity is immutable' USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR (OLD.last_observed_at IS NOT NULL AND
           (NEW.last_observed_at IS NULL OR NEW.last_observed_at<OLD.last_observed_at)) THEN
            RAISE EXCEPTION 'Edge runtime target time cannot regress' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE IF NOT EXISTS user_password_credentials (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    login_normalized text NOT NULL UNIQUE,
    password_hash text NOT NULL CHECK (length(password_hash) BETWEEN 64 AND 512),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (login_normalized = lower(btrim(login_normalized))),
    CHECK (length(login_normalized) BETWEEN 1 AND 100)
);

CREATE TABLE IF NOT EXISTS outbox_valkey_dataset (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    dataset_id uuid NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now()
);

-- Project-scoped pull credential catalog. Entries bind a human name to an
-- operator-owned registry target; credential bytes and Kubernetes Secret
-- coordinates never enter these tables or tenant API responses.
CREATE TABLE IF NOT EXISTS project_registry_pull_credentials (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    registry_target_id uuid NOT NULL REFERENCES registry_targets(id) ON DELETE RESTRICT,
    name text NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name),
    UNIQUE (project_id, registry_target_id),
    CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}[A-Za-z0-9]$' OR name ~ '^[A-Za-z0-9]$')
);

CREATE TABLE IF NOT EXISTS application_registry_pull_selections (
    application_id uuid PRIMARY KEY REFERENCES applications(id) ON DELETE CASCADE,
    mode text NOT NULL CHECK (mode IN ('public','project-credential')),
    project_credential_id uuid REFERENCES project_registry_pull_credentials(id) ON DELETE RESTRICT,
    updated_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((mode='public' AND project_credential_id IS NULL) OR
           (mode='project-credential' AND project_credential_id IS NOT NULL))
);

CREATE OR REPLACE FUNCTION enforce_application_registry_pull_selection_scope()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    app_project uuid;
    credential_project uuid;
BEGIN
    SELECT project_id INTO STRICT app_project FROM applications WHERE id=NEW.application_id;
    IF NEW.mode='project-credential' THEN
        SELECT project_id INTO STRICT credential_project
          FROM project_registry_pull_credentials WHERE id=NEW.project_credential_id;
        IF app_project <> credential_project THEN
            RAISE EXCEPTION 'registry pull credential belongs to another project' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS application_registry_pull_selection_scope ON application_registry_pull_selections;
CREATE CONSTRAINT TRIGGER application_registry_pull_selection_scope
    AFTER INSERT OR UPDATE ON application_registry_pull_selections
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_application_registry_pull_selection_scope();

CREATE OR REPLACE FUNCTION enforce_project_registry_pull_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    pull_ref text;
BEGIN
    SELECT pull_credential_ref INTO STRICT pull_ref FROM registry_targets WHERE id=NEW.registry_target_id;
    IF pull_ref='' THEN
        RAISE EXCEPTION 'registry target has no pull credential' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS project_registry_pull_target ON project_registry_pull_credentials;
CREATE CONSTRAINT TRIGGER project_registry_pull_target
    AFTER INSERT OR UPDATE ON project_registry_pull_credentials
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_project_registry_pull_target();

-- Git projection commands bind rendered application bytes to the explicit
-- runtime chart release used to validate them. Releases originally used OCI
-- digests; the public operator contract now uses a readable semantic version.
-- Keep existing digest-backed rows valid while requiring every new textual
-- identity to be an exact stable or RC version.
ALTER TABLE git_deployment_write_commands
    DROP CONSTRAINT git_deployment_write_commands_chart_digest_check,
    ADD CONSTRAINT git_deployment_write_commands_chart_digest_check CHECK (
        chart_digest ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'
    );

ALTER TABLE deployment_config_previews
    DROP CONSTRAINT deployment_config_previews_git_shape,
    ADD CONSTRAINT deployment_config_previews_git_shape CHECK (
        (git_binding_id IS NULL AND git_base_revision IS NULL AND git_path IS NULL
            AND git_expected_etag IS NULL AND git_chart_digest IS NULL
            AND git_policy_version IS NULL) OR
        (git_binding_id IS NOT NULL
            AND git_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
            AND git_path IS NOT NULL AND length(git_path) BETWEEN 1 AND 1024
            AND git_path !~ '(^/|/\.\.?(/|$)|//|\\)'
            AND git_expected_etag ~ '^"sha256:[0-9a-f]{64}"$'
            AND git_chart_digest ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'
            AND length(git_policy_version) BETWEEN 1 AND 128
            AND git_policy_version !~ E'[\\x00\\r\\n]')
    );
