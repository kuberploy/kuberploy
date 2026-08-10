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
