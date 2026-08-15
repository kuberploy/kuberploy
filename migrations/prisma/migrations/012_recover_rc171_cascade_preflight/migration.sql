-- RC171 shipped migration 011 with a logically no-op UPDATE that still invoked
-- the immutable terminal-intent trigger.  The migration runner installs one
-- narrowly scoped, fail-closed BEFORE UPDATE shim before replaying the exact
-- published migration.  This additive migration attests the resulting cascade
-- schema and removes only that exact shim.  Fresh databases never need it.

LOCK TABLE public.helm_protected_application_intents IN ACCESS EXCLUSIVE MODE;

DO $migration$
DECLARE
    shim_proc pg_catalog.pg_proc%ROWTYPE;
    shim_trigger pg_catalog.pg_trigger%ROWTYPE;
    shim_function_count integer;
    shim_trigger_count integer;
    expected_body constant text := $body$BEGIN
    IF OLD.state IN ('verified','failed','superseded') AND
       NEW IS NOT DISTINCT FROM OLD AND
       (SELECT pg_catalog.count(*)=3
          FROM pg_catalog.pg_attribute AS attribute
          JOIN pg_catalog.pg_class AS relation ON relation.oid=attribute.attrelid
          JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
         WHERE namespace.nspname='public'
           AND relation.relname='helm_protected_application_intents'
           AND attribute.attname IN ('cascade_required','cascade_receipt_id','cascade_contract')
           AND NOT attribute.attisdropped) THEN
        RETURN NULL;
    END IF;
    RETURN NEW;
END;$body$;
BEGIN
    IF (
        SELECT pg_catalog.count(*)<>3
        FROM pg_catalog.pg_attribute AS attribute
        JOIN pg_catalog.pg_class AS relation ON relation.oid=attribute.attrelid
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
        WHERE namespace.nspname='public'
          AND relation.relname='helm_protected_application_intents'
          AND attribute.attname IN ('cascade_required','cascade_receipt_id','cascade_contract')
          AND NOT attribute.attisdropped
    ) THEN
        RAISE EXCEPTION 'migration 011 cascade columns are incomplete' USING ERRCODE='23514';
    END IF;

    IF pg_catalog.to_regclass('public.helm_application_cascade_preflights') IS NULL OR
       pg_catalog.to_regclass('public.helm_application_cascade_adoption_receipts') IS NULL OR
       pg_catalog.to_regclass('public.helm_application_cascade_observer_activations') IS NULL OR
       pg_catalog.to_regclass('public.helm_application_cascade_observation_jobs') IS NULL OR
       pg_catalog.to_regclass('public.helm_application_cascade_receipts') IS NULL THEN
        RAISE EXCEPTION 'migration 011 cascade relations are incomplete' USING ERRCODE='23514';
    END IF;

    IF pg_catalog.to_regprocedure('public.validate_helm_application_cascade_observer_activation()') IS NULL OR
       pg_catalog.to_regprocedure('public.helm_application_cascade_active_observer_is_exact(uuid,text,bigint,text,text,text,text,bigint,text,text,timestamptz)') IS NULL OR
       pg_catalog.to_regprocedure('public.activate_helm_application_cascade_observer(uuid,text,text,text,text,bigint,text,text,text,bigint)') IS NULL OR
       pg_catalog.to_regprocedure('public.helm_application_active_publisher_is_exact(uuid,text,text,text,timestamptz)') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_active_publisher_claim()') IS NULL OR
       pg_catalog.to_regprocedure('public.helm_application_cascade_preflight_is_fresh(uuid)') IS NULL OR
       pg_catalog.to_regprocedure('public.helm_application_cascade_expected_child_spec_digest(uuid)') IS NULL OR
       pg_catalog.to_regprocedure('public.helm_application_cascade_expected_root_spec_digest(uuid)') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_protected_cascade_lane()') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_cascade_gate()') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_cascade_exact_gate()') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_cascade_observation_job()') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_cascade_receipt()') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_cascade_preflight()') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_cascade_adoption_receipt()') IS NULL OR
       pg_catalog.to_regprocedure('public.validate_helm_application_cascade_adoption_postimage()') IS NULL OR
       pg_catalog.to_regprocedure('public.adopt_helm_application_cascade_preflight(uuid,text,bigint,text,text,text,bigint)') IS NULL OR
       pg_catalog.to_regprocedure('public.helm_application_cascade_observation_is_exact(uuid,text,timestamptz)') IS NULL OR
       pg_catalog.to_regprocedure('public.helm_application_cascade_is_exact(uuid,text,timestamptz)') IS NULL THEN
        RAISE EXCEPTION 'migration 011 cascade functions are incomplete' USING ERRCODE='23514';
    END IF;

    SELECT pg_catalog.count(*)::integer INTO shim_function_count
    FROM pg_catalog.pg_proc AS proc
    JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
    WHERE namespace.nspname='public'
      AND proc.proname='kuberploy_rc171_terminal_noop_shim';

    SELECT pg_catalog.count(*)::integer INTO shim_trigger_count
    FROM pg_catalog.pg_trigger AS trigger
    JOIN pg_catalog.pg_class AS relation ON relation.oid=trigger.tgrelid
    JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
    WHERE namespace.nspname='public'
      AND trigger.tgname='aa_kuberploy_rc171_terminal_noop_shim';

    IF shim_function_count=0 AND shim_trigger_count=0 THEN
        RETURN;
    END IF;
    IF shim_function_count<>1 OR shim_trigger_count<>1 THEN
        RAISE EXCEPTION 'RC171 migration shim is partial or ambiguous' USING ERRCODE='23514';
    END IF;

    SELECT proc.* INTO STRICT shim_proc
    FROM pg_catalog.pg_proc AS proc
    JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
    WHERE namespace.nspname='public'
      AND proc.proname='kuberploy_rc171_terminal_noop_shim'
      AND proc.pronargs=0;

    SELECT trigger.* INTO STRICT shim_trigger
    FROM pg_catalog.pg_trigger AS trigger
    JOIN pg_catalog.pg_class AS relation ON relation.oid=trigger.tgrelid
    JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
    WHERE namespace.nspname='public'
      AND relation.relname='helm_protected_application_intents'
      AND trigger.tgname='aa_kuberploy_rc171_terminal_noop_shim';

    IF shim_proc.prosrc<>expected_body OR shim_proc.prosecdef OR
       shim_proc.provolatile<>'v' OR shim_proc.prokind<>'f' OR
       shim_proc.pronargs<>0 OR shim_proc.prorettype<>'pg_catalog.trigger'::pg_catalog.regtype OR
       shim_proc.proconfig IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']::text[] OR
       shim_trigger.tgenabled<>'O' OR shim_trigger.tgisinternal OR
       shim_trigger.tgtype<>19 OR shim_trigger.tgnargs<>0 OR
       shim_trigger.tgqual IS NOT NULL OR shim_trigger.tgfoid<>shim_proc.oid THEN
        RAISE EXCEPTION 'RC171 migration shim does not match its closed contract' USING ERRCODE='23514';
    END IF;

    EXECUTE 'DROP TRIGGER aa_kuberploy_rc171_terminal_noop_shim ON public.helm_protected_application_intents';
    EXECUTE 'DROP FUNCTION public.kuberploy_rc171_terminal_noop_shim()';

    IF EXISTS (
        SELECT 1 FROM pg_catalog.pg_proc AS proc
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=proc.pronamespace
        WHERE namespace.nspname='public'
          AND proc.proname='kuberploy_rc171_terminal_noop_shim'
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_trigger AS trigger
        JOIN pg_catalog.pg_class AS relation ON relation.oid=trigger.tgrelid
        JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=relation.relnamespace
        WHERE namespace.nspname='public'
          AND trigger.tgname='aa_kuberploy_rc171_terminal_noop_shim'
    ) THEN
        RAISE EXCEPTION 'RC171 migration shim cleanup was incomplete' USING ERRCODE='23514';
    END IF;
END;
$migration$;
