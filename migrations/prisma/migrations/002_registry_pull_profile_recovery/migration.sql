-- A profile mismatch is caused by operator runtime configuration. Permit the
-- exact failed artifact to recover after that configuration is corrected;
-- every other failed state remains terminal. The worker still holds a fenced
-- lease and independently revalidates the complete profile, namespace,
-- credential reference, revision, and derived Secret name before it can move
-- the row back to ready.
CREATE OR REPLACE FUNCTION public.validate_runtime_registry_pull_artifact() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    durable_namespace text;
    durable_pull_ref text;
BEGIN
    SELECT namespace INTO durable_namespace
      FROM environments WHERE id=NEW.environment_id FOR KEY SHARE;
    SELECT pull_credential_ref INTO durable_pull_ref
      FROM registry_targets WHERE id=NEW.registry_target_id FOR KEY SHARE;
    IF durable_namespace IS DISTINCT FROM NEW.namespace OR
       durable_pull_ref IS NULL OR durable_pull_ref='' OR
       durable_pull_ref IS DISTINCT FROM NEW.pull_credential_ref THEN
        RAISE EXCEPTION 'Runtime registry pull artifact scope does not match durable metadata'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.environment_id,NEW.namespace,NEW.registry_target_id,
               NEW.pull_credential_ref,NEW.profile_name,NEW.profile_revision,
               NEW.secret_name,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.environment_id,OLD.namespace,OLD.registry_target_id,
               OLD.pull_credential_ref,OLD.profile_name,OLD.profile_revision,
               OLD.secret_name,OLD.created_at) THEN
            RAISE EXCEPTION 'Runtime registry pull artifact identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Runtime registry pull artifact epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) AND
           NEW.lease_owner IS NOT NULL THEN
            RAISE EXCEPTION 'Runtime registry pull artifact lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.runtime_state='failed' AND NEW.runtime_state<>'failed' THEN
            IF OLD.last_failure_code<>'profile-mismatch' OR
               OLD.lease_owner IS NULL OR OLD.worker_contract<>'registry-pull.v1' OR
               OLD.worker_config_digest IS NULL OR NEW.runtime_state<>'ready' OR
               NEW.lease_owner IS NOT NULL OR NEW.worker_contract IS NOT NULL OR
               NEW.worker_config_digest IS NOT NULL OR NEW.lease_epoch<>OLD.lease_epoch OR
               NEW.last_failure_code<>'' OR NEW.consecutive_failures<>0 OR
               NEW.last_observed_at IS NULL OR NEW.observed_uid='' OR
               NEW.observed_resource_version='' THEN
                RAISE EXCEPTION 'Failed runtime registry pull artifacts are terminal'
                    USING ERRCODE='23514';
            END IF;
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.last_observed_at IS NOT NULL AND NEW.last_observed_at IS NOT NULL AND
            NEW.last_observed_at<OLD.last_observed_at) THEN
            RAISE EXCEPTION 'Runtime registry pull artifact time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
