-- A current no-change materialization deliberately references the older
-- verified command whose bytes it reuses. Keep the materialization receipt
-- bound to the current projection while treating the referenced command's
-- environment revision and generation as immutable origin metadata.
LOCK TABLE public.helm_application_continuation_receipts IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE public.helm_protected_application_intents IN SHARE ROW EXCLUSIVE MODE;

DO $migration$
DECLARE
    definition text;
    origin_revision text := 'AND current_command.environment_revision=NEW.current_environment_revision';
    origin_generation text := 'AND current_command.environment_generation=NEW.current_environment_generation';
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_application_continuation_receipt()'::pg_catalog.regprocedure
    );
    IF (pg_catalog.length(definition)-pg_catalog.length(
          pg_catalog.replace(definition,origin_revision,'')))<>
         pg_catalog.length(origin_revision) OR
       (pg_catalog.length(definition)-pg_catalog.length(
          pg_catalog.replace(definition,origin_generation,'')))<>
         pg_catalog.length(origin_generation) OR
       pg_catalog.strpos(definition,
         'materialization.environment_revision=NEW.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'materialization.environment_generation=NEW.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'environment.indexed_revision=NEW.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'environment.projection_generation=NEW.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'current_command.project_id=release.project_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_id=release.environment_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.platform_binding_id=NEW.platform_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_binding_id=NEW.environment_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.cluster_id=NEW.cluster_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=NEW.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.policy_digest=NEW.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=NEW.current_app_project_content')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_target_ref=payload.environment_target_ref')=0 OR
       pg_catalog.strpos(definition,
         'newer_materialization.environment_generation=NEW.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'invalid.generation=NEW.current_environment_generation')=0 THEN
        RAISE EXCEPTION 'unexpected continuation receipt validator prerequisite';
    END IF;
    definition := pg_catalog.replace(definition,origin_revision,'');
    definition := pg_catalog.replace(definition,origin_generation,'');
    EXECUTE definition;

    origin_revision := 'AND current_command.environment_revision=receipt.current_environment_revision';
    origin_generation := 'AND current_command.environment_generation=receipt.current_environment_generation';
    definition := pg_catalog.pg_get_functiondef(
        'public.helm_application_continuation_is_exact(uuid)'::pg_catalog.regprocedure
    );
    IF (pg_catalog.length(definition)-pg_catalog.length(
          pg_catalog.replace(definition,origin_revision,'')))<>
         pg_catalog.length(origin_revision) OR
       (pg_catalog.length(definition)-pg_catalog.length(
          pg_catalog.replace(definition,origin_generation,'')))<>
         pg_catalog.length(origin_generation) OR
       pg_catalog.strpos(definition,
         'materialization.environment_revision=receipt.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'materialization.environment_generation=receipt.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'current_environment_binding.indexed_revision=receipt.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'current_environment_binding.projection_generation=receipt.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'current_command.project_id=intent.project_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_id=intent.environment_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.platform_binding_id=intent.platform_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_binding_id=intent.environment_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.cluster_id=intent.cluster_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=receipt.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.policy_digest=receipt.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=receipt.current_app_project_content')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_target_ref=intent.environment_target_ref')=0 OR
       pg_catalog.strpos(definition,
         'newer_materialization.environment_generation=receipt.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'invalid.generation=receipt.current_environment_generation')=0 THEN
        RAISE EXCEPTION 'unexpected exact continuation prerequisite';
    END IF;
    definition := pg_catalog.replace(definition,origin_revision,'');
    definition := pg_catalog.replace(definition,origin_generation,'');
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.validate_helm_application_continuation_receipt()
SET search_path=pg_catalog,pg_temp;

ALTER FUNCTION public.helm_application_continuation_is_exact(uuid)
SET search_path=pg_catalog,pg_temp;

REVOKE ALL ON FUNCTION public.helm_application_continuation_is_exact(uuid) FROM PUBLIC;

-- Verify the replacement removed only command-origin projection equality and
-- retained every current materialization, route, byte, runtime, and freshness
-- authority used by insertion and pristine claim admission.
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
         'current_command.environment_revision=NEW.current_environment_revision')>0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_generation=NEW.current_environment_generation')>0 OR
       pg_catalog.strpos(definition,
         'materialization.environment_revision=NEW.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'materialization.environment_generation=NEW.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'environment.indexed_revision=NEW.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'environment.projection_generation=NEW.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'current_command.project_id=release.project_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_id=release.environment_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.platform_binding_id=NEW.platform_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_binding_id=NEW.environment_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.cluster_id=NEW.cluster_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=NEW.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.committed_revision=NEW.current_desired_state_revision')=0 OR
       pg_catalog.strpos(definition,
         'current_command.policy_digest=NEW.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_repository=NEW.current_chart_repository')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_name=NEW.current_chart_name')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_version=NEW.current_chart_version')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_digest=NEW.current_chart_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.renderer_image=NEW.current_renderer_image')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_digest_enforcement=NEW.current_chart_digest_enforcement')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=NEW.current_app_project_content')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_target_ref=payload.environment_target_ref')=0 OR
       pg_catalog.strpos(definition,
         'current_command.destination_namespace=desired_environment.namespace')=0 OR
       pg_catalog.strpos(definition,
         'newer_materialization.environment_generation=NEW.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'invalid.generation=NEW.current_environment_generation')=0 THEN
        RAISE EXCEPTION 'continuation receipt validator lost bounded authority';
    END IF;

    SELECT pg_catalog.pg_get_functiondef(procedure.oid),procedure.proconfig
      INTO definition,settings
      FROM pg_catalog.pg_proc procedure
     WHERE procedure.oid=
       'public.helm_application_continuation_is_exact(uuid)'::pg_catalog.regprocedure;
    IF settings IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']::text[] OR
       pg_catalog.strpos(definition,
         'current_command.environment_revision=receipt.current_environment_revision')>0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_generation=receipt.current_environment_generation')>0 OR
       pg_catalog.strpos(definition,
         'materialization.environment_revision=receipt.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'materialization.environment_generation=receipt.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'current_environment_binding.indexed_revision=receipt.current_environment_revision')=0 OR
       pg_catalog.strpos(definition,
         'current_environment_binding.projection_generation=receipt.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'current_command.project_id=intent.project_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_id=intent.environment_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.platform_binding_id=intent.platform_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_binding_id=intent.environment_binding_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.cluster_id=intent.cluster_id')=0 OR
       pg_catalog.strpos(definition,
         'current_command.content_sha256=receipt.current_desired_state_content_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.committed_revision=receipt.current_desired_state_revision')=0 OR
       pg_catalog.strpos(definition,
         'current_command.policy_digest=receipt.current_policy_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_repository=receipt.current_chart_repository')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_name=receipt.current_chart_name')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_version=receipt.current_chart_version')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_digest=receipt.current_chart_digest')=0 OR
       pg_catalog.strpos(definition,
         'current_command.renderer_image=receipt.current_renderer_image')=0 OR
       pg_catalog.strpos(definition,
         'current_command.chart_digest_enforcement=receipt.current_chart_digest_enforcement')=0 OR
       pg_catalog.strpos(definition,
         'current_command.app_project_content=receipt.current_app_project_content')=0 OR
       pg_catalog.strpos(definition,
         'current_command.environment_target_ref=intent.environment_target_ref')=0 OR
       pg_catalog.strpos(definition,
         'current_command.destination_namespace=desired_environment.namespace')=0 OR
       pg_catalog.strpos(definition,
         'newer_materialization.environment_generation=receipt.current_environment_generation')=0 OR
       pg_catalog.strpos(definition,
         'invalid.generation=receipt.current_environment_generation')=0 THEN
        RAISE EXCEPTION 'exact continuation helper lost bounded authority';
    END IF;
END;
$migration$;
