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
