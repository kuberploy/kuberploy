-- A no-change materialization can reuse an older verified command whose bytes
-- are unchanged while the current policy digest advances. The materialization
-- receipt is current policy authority; the command policy is origin metadata.
LOCK TABLE public.helm_application_continuation_receipts IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE public.helm_protected_application_intents IN SHARE ROW EXCLUSIVE MODE;

DO $migration$
DECLARE
    definition text;
    command_policy text := 'AND current_command.policy_digest=NEW.current_policy_digest';
    receipt_policy text := 'AND current_command.policy_digest=receipt.current_policy_digest';
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_application_continuation_receipt()'::pg_catalog.regprocedure
    );
    IF (pg_catalog.length(definition)-pg_catalog.length(
          pg_catalog.replace(definition,command_policy,'')))<>
         pg_catalog.length(command_policy) OR
       pg_catalog.strpos(definition,
         'materialization.policy_digest=NEW.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=NEW.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=NEW.current_app_project_content')=0 THEN
        RAISE EXCEPTION 'unexpected continuation validator policy authority';
    END IF;
    definition := pg_catalog.replace(definition,command_policy,'');
    EXECUTE definition;

    definition := pg_catalog.pg_get_functiondef(
        'public.helm_application_continuation_is_exact(uuid)'::pg_catalog.regprocedure
    );
    IF (pg_catalog.length(definition)-pg_catalog.length(
          pg_catalog.replace(definition,receipt_policy,'')))<>
         pg_catalog.length(receipt_policy) OR
       pg_catalog.strpos(definition,
         'materialization.policy_digest=receipt.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=receipt.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=receipt.current_app_project_content')=0 THEN
        RAISE EXCEPTION 'unexpected exact continuation policy authority';
    END IF;
    definition := pg_catalog.replace(definition,receipt_policy,'');
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.validate_helm_application_continuation_receipt()
SET search_path=pg_catalog,pg_temp;

ALTER FUNCTION public.helm_application_continuation_is_exact(uuid)
SET search_path=pg_catalog,pg_temp;

REVOKE ALL ON FUNCTION public.helm_application_continuation_is_exact(uuid) FROM PUBLIC;

DO $migration$
DECLARE
    definition text;
    settings text[];
BEGIN
    SELECT pg_catalog.pg_get_functiondef(procedure.oid),procedure.proconfig
      INTO definition,settings
      FROM pg_catalog.pg_proc procedure
     WHERE procedure.oid=
       'public.validate_helm_application_continuation_receipt()'::pg_catalog.regprocedure;
    IF settings IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']::text[] OR
       pg_catalog.strpos(definition,
         'current_command.policy_digest=NEW.current_policy_digest')>0 OR
       pg_catalog.strpos(definition,
         'materialization.policy_digest=NEW.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=NEW.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=NEW.current_app_project_content')=0 THEN
        RAISE EXCEPTION 'continuation validator policy migration verification failed';
    END IF;

    SELECT pg_catalog.pg_get_functiondef(procedure.oid),procedure.proconfig
      INTO definition,settings
      FROM pg_catalog.pg_proc procedure
     WHERE procedure.oid=
       'public.helm_application_continuation_is_exact(uuid)'::pg_catalog.regprocedure;
    IF settings IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']::text[] OR
       pg_catalog.strpos(definition,
         'current_command.policy_digest=receipt.current_policy_digest')>0 OR
       pg_catalog.strpos(definition,
         'materialization.policy_digest=receipt.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=receipt.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=receipt.current_app_project_content')=0 THEN
        RAISE EXCEPTION 'exact continuation policy migration verification failed';
    END IF;
END;
$migration$;
