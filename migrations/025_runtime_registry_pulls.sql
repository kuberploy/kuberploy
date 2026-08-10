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
