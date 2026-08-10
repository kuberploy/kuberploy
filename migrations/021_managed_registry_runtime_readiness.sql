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
