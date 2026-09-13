-- Secret delivery and lifecycle events are permanent history. Validate their
-- live authority when written, without requiring deleted resource metadata to
-- remain forever. Existing rows and immutable-history triggers are unchanged.
-- One statement keeps the authority replacement atomic in every SQL runner.
DO $migration$
BEGIN
    CREATE FUNCTION public.validate_secret_binding_history_identity() RETURNS trigger
        LANGUAGE plpgsql
        AS $function$
    BEGIN
        PERFORM 1 FROM public.secret_bindings WHERE id=NEW.binding_id FOR KEY SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'secret history requires an existing binding'
                USING ERRCODE='23503', CONSTRAINT='secret_binding_history_binding_identity';
        END IF;
        IF NEW.version_id IS NOT NULL THEN
            PERFORM 1 FROM public.secret_binding_versions
                WHERE id=NEW.version_id AND binding_id=NEW.binding_id FOR KEY SHARE;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'secret history requires the exact binding version'
                    USING ERRCODE='23503', CONSTRAINT='secret_binding_history_version_identity';
            END IF;
        END IF;
        RETURN NEW;
    END;
    $function$;

    CREATE TRIGGER secret_binding_deliveries_identity
        BEFORE INSERT ON public.secret_binding_deliveries
        FOR EACH ROW EXECUTE FUNCTION public.validate_secret_binding_history_identity();
    CREATE TRIGGER secret_binding_events_identity
        BEFORE INSERT ON public.secret_binding_events
        FOR EACH ROW EXECUTE FUNCTION public.validate_secret_binding_history_identity();

    ALTER TABLE public.secret_binding_deliveries
        DROP CONSTRAINT secret_binding_deliveries_version_id_binding_id_fkey;
    ALTER TABLE public.secret_binding_events
        DROP CONSTRAINT secret_binding_events_binding_id_fkey,
        DROP CONSTRAINT secret_binding_events_version_id_binding_id_fkey;
END;
$migration$;
