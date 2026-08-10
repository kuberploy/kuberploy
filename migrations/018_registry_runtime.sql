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
