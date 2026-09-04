-- Disabling an auto-deploy policy preserves its historical deployment snapshot.
-- The live deployment generation and App source can legitimately change after
-- that snapshot was recorded, so only enabled revisions require both to remain
-- current. Scope ownership and foreign keys remain enforced for every revision.
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
             AND d.application_id=a.id AND d.environment_id=e.id
             AND (NOT NEW.enabled OR d.generation=NEW.source_deployment_generation)
        JOIN service_accounts sa ON sa.id=NEW.service_actor_id AND sa.project_id=a.project_id
        WHERE a.id=policy_row.application_id AND a.project_id=policy_row.project_id
          AND (NOT NEW.enabled OR a.build_source_id IS NOT NULL)
          AND (NOT NEW.enabled OR sa.disabled_at IS NULL)
    ) THEN
        RAISE EXCEPTION 'auto-deploy policy resource binding mismatch' USING ERRCODE='23503';
    END IF;
    IF NEW.created_at<policy_row.created_at THEN
        RAISE EXCEPTION 'auto-deploy revision predates policy' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
