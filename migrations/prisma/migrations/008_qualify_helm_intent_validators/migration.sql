-- Migration 007 executes the protected intent validators from hardened
-- SECURITY DEFINER adoption functions whose search path excludes public.
-- Preserve every validator rule while making their public dependencies exact
-- and giving each validator its own fail-closed search path.
DO $migration$
DECLARE
    definition text;
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_protected_payload_intent()'::pg_catalog.regprocedure
    );
    IF pg_catalog.strpos(definition,'release_row helm_release_revisions%ROWTYPE;')=0 OR
       pg_catalog.strpos(definition,'platform_row git_repository_bindings%ROWTYPE;')=0 OR
       pg_catalog.strpos(definition,'environment_row git_repository_bindings%ROWTYPE;')=0 OR
       pg_catalog.strpos(definition,'FROM helm_release_revisions')=0 OR
       pg_catalog.strpos(definition,'FROM helm_release_heads')=0 OR
       pg_catalog.strpos(definition,'FROM git_repository_bindings')=0 OR
       pg_catalog.strpos(definition,'FROM git_projection_generations')=0 OR
       pg_catalog.strpos(definition,'FROM git_projected_documents')=0 OR
       pg_catalog.strpos(definition,'FROM helm_render_results')=0 OR
       pg_catalog.strpos(definition,'JOIN helm_render_commands')=0 OR
       pg_catalog.strpos(definition,'NEW.publisher_contract,NEW.original_publisher_config_digest,NEW.message')=0 OR
       pg_catalog.strpos(definition,'OLD.publisher_contract,OLD.original_publisher_config_digest,OLD.message')=0 THEN
        RAISE EXCEPTION 'unexpected protected payload validator prerequisite';
    END IF;
    definition := pg_catalog.replace(definition,
        'release_row helm_release_revisions%ROWTYPE;',
        'release_row public.helm_release_revisions%ROWTYPE;');
    definition := pg_catalog.replace(definition,
        'platform_row git_repository_bindings%ROWTYPE;',
        'platform_row public.git_repository_bindings%ROWTYPE;');
    definition := pg_catalog.replace(definition,
        'environment_row git_repository_bindings%ROWTYPE;',
        'environment_row public.git_repository_bindings%ROWTYPE;');
    definition := pg_catalog.replace(definition,
        'FROM helm_release_revisions', 'FROM public.helm_release_revisions');
    definition := pg_catalog.replace(definition,
        'FROM helm_release_heads', 'FROM public.helm_release_heads');
    definition := pg_catalog.replace(definition,
        'FROM git_repository_bindings', 'FROM public.git_repository_bindings');
    definition := pg_catalog.replace(definition,
        'FROM git_projection_generations', 'FROM public.git_projection_generations');
    definition := pg_catalog.replace(definition,
        'FROM git_projected_documents', 'FROM public.git_projected_documents');
    definition := pg_catalog.replace(definition,
        'FROM helm_render_results', 'FROM public.helm_render_results');
    definition := pg_catalog.replace(definition,
        'JOIN helm_render_commands', 'JOIN public.helm_render_commands');
    definition := pg_catalog.replace(definition,
        'convert_from(NEW.content', 'pg_catalog.convert_from(NEW.content');
    definition := pg_catalog.replace(definition,
        'jsonb_build_object(', 'pg_catalog.jsonb_build_object(');
    EXECUTE definition;

    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_protected_application_intent()'::pg_catalog.regprocedure
    );
    IF pg_catalog.strpos(definition,'release_row helm_release_revisions%ROWTYPE;')=0 OR
       pg_catalog.strpos(definition,'payload_row helm_protected_payload_intents%ROWTYPE;')=0 OR
       pg_catalog.strpos(definition,'base_row helm_protected_application_intents%ROWTYPE;')=0 OR
       pg_catalog.strpos(definition,'platform_row git_repository_bindings%ROWTYPE;')=0 OR
       pg_catalog.strpos(definition,'FROM helm_release_revisions')=0 OR
       pg_catalog.strpos(definition,'FROM helm_protected_payload_intents')=0 OR
       pg_catalog.strpos(definition,'FROM helm_release_heads')=0 OR
       pg_catalog.strpos(definition,'FROM git_repository_bindings')=0 OR
       pg_catalog.strpos(definition,'FROM helm_protected_application_intents')=0 OR
       pg_catalog.strpos(definition,'NEW.publisher_contract,NEW.original_publisher_config_digest,NEW.message')=0 OR
       pg_catalog.strpos(definition,'OLD.publisher_contract,OLD.original_publisher_config_digest,OLD.message')=0 THEN
        RAISE EXCEPTION 'unexpected protected Application validator prerequisite';
    END IF;
    definition := pg_catalog.replace(definition,
        'release_row helm_release_revisions%ROWTYPE;',
        'release_row public.helm_release_revisions%ROWTYPE;');
    definition := pg_catalog.replace(definition,
        'payload_row helm_protected_payload_intents%ROWTYPE;',
        'payload_row public.helm_protected_payload_intents%ROWTYPE;');
    definition := pg_catalog.replace(definition,
        'base_row helm_protected_application_intents%ROWTYPE;',
        'base_row public.helm_protected_application_intents%ROWTYPE;');
    definition := pg_catalog.replace(definition,
        'platform_row git_repository_bindings%ROWTYPE;',
        'platform_row public.git_repository_bindings%ROWTYPE;');
    definition := pg_catalog.replace(definition,
        'FROM helm_release_revisions', 'FROM public.helm_release_revisions');
    definition := pg_catalog.replace(definition,
        'FROM helm_protected_payload_intents', 'FROM public.helm_protected_payload_intents');
    definition := pg_catalog.replace(definition,
        'FROM helm_release_heads', 'FROM public.helm_release_heads');
    definition := pg_catalog.replace(definition,
        'FROM git_repository_bindings', 'FROM public.git_repository_bindings');
    definition := pg_catalog.replace(definition,
        'FROM helm_protected_application_intents',
        'FROM public.helm_protected_application_intents');
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.validate_helm_protected_payload_intent()
SET search_path=pg_catalog,pg_temp;

ALTER FUNCTION public.validate_helm_protected_application_intent()
SET search_path=pg_catalog,pg_temp;

-- Fail migration if either replacement left an ambient public lookup or if
-- the hardened function-local search path was not installed exactly.
DO $migration$
DECLARE
    definition text;
    settings text[];
BEGIN
    SELECT pg_catalog.pg_get_functiondef(procedure.oid),procedure.proconfig
      INTO definition,settings
      FROM pg_catalog.pg_proc procedure
     WHERE procedure.oid='public.validate_helm_protected_payload_intent()'::pg_catalog.regprocedure;
    IF settings IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']::text[] OR
       pg_catalog.strpos(definition,' helm_release_revisions%ROWTYPE')>0 OR
       pg_catalog.strpos(definition,' git_repository_bindings%ROWTYPE')>0 OR
       pg_catalog.strpos(definition,'FROM helm_release_revisions')>0 OR
       pg_catalog.strpos(definition,'FROM helm_release_heads')>0 OR
       pg_catalog.strpos(definition,'FROM git_repository_bindings')>0 OR
       pg_catalog.strpos(definition,'FROM git_projection_generations')>0 OR
       pg_catalog.strpos(definition,'FROM git_projected_documents')>0 OR
       pg_catalog.strpos(definition,'FROM helm_render_results')>0 OR
       pg_catalog.strpos(definition,'JOIN helm_render_commands')>0 THEN
        RAISE EXCEPTION 'protected payload validator remains ambient';
    END IF;

    SELECT pg_catalog.pg_get_functiondef(procedure.oid),procedure.proconfig
      INTO definition,settings
      FROM pg_catalog.pg_proc procedure
     WHERE procedure.oid='public.validate_helm_protected_application_intent()'::pg_catalog.regprocedure;
    IF settings IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']::text[] OR
       pg_catalog.strpos(definition,' helm_release_revisions%ROWTYPE')>0 OR
       pg_catalog.strpos(definition,' helm_protected_payload_intents%ROWTYPE')>0 OR
       pg_catalog.strpos(definition,' helm_protected_application_intents%ROWTYPE')>0 OR
       pg_catalog.strpos(definition,' git_repository_bindings%ROWTYPE')>0 OR
       pg_catalog.strpos(definition,'FROM helm_release_revisions')>0 OR
       pg_catalog.strpos(definition,'FROM helm_protected_payload_intents')>0 OR
       pg_catalog.strpos(definition,'FROM helm_release_heads')>0 OR
       pg_catalog.strpos(definition,'FROM git_repository_bindings')>0 OR
       pg_catalog.strpos(definition,'FROM helm_protected_application_intents')>0 THEN
        RAISE EXCEPTION 'protected Application validator remains ambient';
    END IF;
END;
$migration$;
