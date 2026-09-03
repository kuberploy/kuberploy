-- Release candidate 432 corrected this function after release candidate 431
-- had already published 001_initial. Accept only that exact published checksum,
-- then normalize the history to the frozen 001_initial bytes shipped now. Any
-- other checksum remains a hard migration-integrity failure.
DO $migration$
DECLARE
    initial_count bigint;
    initial_checksum text;
BEGIN
    SELECT count(*), min(checksum)
      INTO initial_count, initial_checksum
      FROM public._prisma_migrations
     WHERE migration_name = '001_initial'
       AND finished_at IS NOT NULL
       AND rolled_back_at IS NULL
       AND applied_steps_count = 1;

    IF initial_count <> 1 THEN
        RAISE EXCEPTION 'expected one completed 001_initial migration, found %', initial_count;
    END IF;

    IF initial_checksum = 'efc555eb9c9d8591e74899818b409202165a8978f9052204c6fe9e89cc70230d' THEN
        UPDATE public._prisma_migrations
           SET checksum = '1aa6590b46d37e6a71dfdc85df7a7d8b7376b41e18deb02cab6b16e52e4cad79'
         WHERE migration_name = '001_initial'
           AND checksum = initial_checksum
           AND finished_at IS NOT NULL
           AND rolled_back_at IS NULL
           AND applied_steps_count = 1;
    ELSIF initial_checksum <> '1aa6590b46d37e6a71dfdc85df7a7d8b7376b41e18deb02cab6b16e52e4cad79' THEN
        RAISE EXCEPTION '001_initial checksum is not an approved published checksum';
    END IF;

    EXECUTE $function$
CREATE OR REPLACE FUNCTION public.validate_auto_deploy_policy_revision() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE policy_row auto_deploy_policies%ROWTYPE;
BEGIN
    SELECT * INTO STRICT policy_row FROM auto_deploy_policies WHERE id=NEW.policy_id;
    IF NOT EXISTS (
        SELECT 1 FROM applications a
        JOIN environments e ON e.id=policy_row.environment_id AND e.project_id=a.project_id
        JOIN deployments d ON d.id=NEW.source_deployment_id
             AND d.application_id=a.id AND d.environment_id=e.id AND d.generation=NEW.source_deployment_generation
        JOIN service_accounts sa ON sa.id=NEW.service_actor_id AND sa.project_id=a.project_id
        WHERE a.id=policy_row.application_id AND a.project_id=policy_row.project_id
          AND a.build_source_id IS NOT NULL
          AND (NOT NEW.enabled OR sa.disabled_at IS NULL)
    ) THEN
        RAISE EXCEPTION 'auto-deploy policy resource binding mismatch' USING ERRCODE='23503';
    END IF;
    IF NEW.created_at<policy_row.created_at THEN
        RAISE EXCEPTION 'auto-deploy revision predates policy' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$
$function$;
END;
$migration$;
