-- The cleanup-plan target is a UUID. Keeping the PL/pgSQL local variable as
-- text made the first offline-maintenance receipt fail at runtime with an
-- undefined text/uuid equality operator.
CREATE OR REPLACE FUNCTION public.validate_registry_runtime_maintenance_target() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_mode text;
    plan_target uuid;
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
