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
