-- A released failed maintenance attempt must retain its deterministic sweep
-- marker. The next fenced acquisition uses that exact immutable marker plus a
-- fresh registry-wide absence checkpoint to repair a lost GC receipt without
-- executing Distribution garbage collection twice.
CREATE OR REPLACE FUNCTION public.protect_registry_runtime_maintenance_epoch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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
       NEW.sweep_job_uid=OLD.sweep_job_uid AND
       NEW.restored_at IS NULL AND NEW.released_at IS NULL
    ) THEN
        RAISE EXCEPTION 'Released registry maintenance may only be reacquired for a fresh replay checkpoint'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
