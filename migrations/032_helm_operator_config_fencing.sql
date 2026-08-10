-- Fence every external Helm render across rolling operator configuration
-- changes. Historical work cannot be attributed to the complete operator
-- policy, so it is stamped with a non-runnable sentinel and any non-terminal
-- command is failed closed during the upgrade.

ALTER TABLE helm_render_commands
    ADD COLUMN operator_config_digest text NOT NULL
        DEFAULT 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
        CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    ADD COLUMN worker_operator_config_digest text
        CHECK (worker_operator_config_digest IS NULL OR
               worker_operator_config_digest ~ '^sha256:[0-9a-f]{64}$');
ALTER TABLE helm_render_commands ALTER COLUMN operator_config_digest DROP DEFAULT;

ALTER TABLE helm_render_results
    ADD COLUMN operator_config_digest text NOT NULL
        DEFAULT 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
        CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$');
ALTER TABLE helm_render_results ALTER COLUMN operator_config_digest DROP DEFAULT;

-- Old readiness cannot prove the complete operator policy that produced it.
DELETE FROM helm_renderer_readiness;
ALTER TABLE helm_renderer_readiness
    ADD COLUMN operator_config_digest text NOT NULL
        CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$');

-- Existing queued or leased commands were created without an operator digest.
-- Do not let a new worker adopt them, including after a crashed old worker.
UPDATE helm_render_commands SET
    state='failed',
    consecutive_failures=LEAST(consecutive_failures+1,10),
    last_failure_code='operator-config-upgrade',
    lease_owner=NULL,
    lease_until=NULL,
    worker_contract=NULL,
    worker_renderer_image=NULL,
    worker_renderer_version=NULL,
    worker_policy_version=NULL,
    worker_limits_digest=NULL,
    worker_operator_config_digest=NULL,
    completed_at=GREATEST(updated_at,clock_timestamp()),
    updated_at=GREATEST(updated_at,clock_timestamp())
WHERE state IN ('queued','processing');

ALTER TABLE helm_render_commands ADD CONSTRAINT helm_render_commands_operator_lease_check CHECK (
    (lease_owner IS NULL AND worker_operator_config_digest IS NULL) OR
    (lease_owner IS NOT NULL AND
     worker_operator_config_digest=operator_config_digest)
);

CREATE OR REPLACE FUNCTION validate_helm_render_command()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_namespace text;
    durable_release_name text;
BEGIN
    SELECT namespace INTO durable_namespace
      FROM environments WHERE id=NEW.environment_id FOR KEY SHARE;
    IF durable_namespace IS DISTINCT FROM NEW.namespace THEN
        RAISE EXCEPTION 'Helm render destination does not match its durable environment'
            USING ERRCODE='23514';
    END IF;
    SELECT slug INTO durable_release_name
      FROM applications
      WHERE id=NEW.application_id AND project_id=NEW.project_id
      FOR KEY SHARE;
    IF durable_release_name IS DISTINCT FROM NEW.release_name THEN
        RAISE EXCEPTION 'Helm render release does not match its durable application'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
        NEW.state<>'queued' OR NEW.attempts<>0 OR
        NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
        NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
        NEW.lease_until IS NOT NULL OR NEW.worker_contract IS NOT NULL OR
        NEW.worker_renderer_image IS NOT NULL OR
        NEW.worker_renderer_version IS NOT NULL OR
        NEW.worker_policy_version IS NOT NULL OR
        NEW.worker_limits_digest IS NOT NULL OR
        NEW.worker_operator_config_digest IS NOT NULL OR
        NEW.completed_at IS NOT NULL OR
        NEW.available_at<>NEW.created_at OR NEW.updated_at<>NEW.created_at
    ) THEN
        RAISE EXCEPTION 'Helm render commands must be inserted as pristine queued work'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('succeeded','failed') THEN
            RAISE EXCEPTION 'Terminal Helm render commands are immutable'
                USING ERRCODE='23514';
        END IF;
        IF ROW(NEW.id,NEW.idempotency_scope,NEW.idempotency_key,
               NEW.approval_id,NEW.approval_revision,NEW.project_id,
               NEW.environment_id,NEW.application_id,NEW.namespace,
               NEW.release_name,NEW.descriptor_yaml,NEW.values_yaml,
               NEW.descriptor_digest,NEW.values_digest,NEW.input_digest,
               NEW.operator_config_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.idempotency_scope,OLD.idempotency_key,
               OLD.approval_id,OLD.approval_revision,OLD.project_id,
               OLD.environment_id,OLD.application_id,OLD.namespace,
               OLD.release_name,OLD.descriptor_yaml,OLD.values_yaml,
               OLD.descriptor_digest,OLD.values_digest,OLD.input_digest,
               OLD.operator_config_digest,OLD.created_at) THEN
            RAISE EXCEPTION 'Helm render command identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.attempts<OLD.attempts OR NEW.attempts>OLD.attempts+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Helm render command fencing or time regressed'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_renderer_image,
               NEW.worker_renderer_version,NEW.worker_policy_version,
               NEW.worker_limits_digest,NEW.worker_operator_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_renderer_image,
               OLD.worker_renderer_version,OLD.worker_policy_version,
               OLD.worker_limits_digest,OLD.worker_operator_config_digest) THEN
            RAISE EXCEPTION 'Helm render lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION validate_helm_render_result()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_input text;
    durable_state text;
    durable_operator_config_digest text;
    durable_worker_operator_config_digest text;
BEGIN
    SELECT input_digest,state,operator_config_digest,worker_operator_config_digest
      INTO durable_input,durable_state,durable_operator_config_digest,
           durable_worker_operator_config_digest
      FROM helm_render_commands WHERE id=NEW.command_id FOR KEY SHARE;
    IF durable_state IS DISTINCT FROM 'processing' OR
       durable_input IS DISTINCT FROM NEW.input_digest OR
       durable_operator_config_digest IS DISTINCT FROM NEW.operator_config_digest OR
       durable_worker_operator_config_digest IS DISTINCT FROM NEW.operator_config_digest THEN
        RAISE EXCEPTION 'Helm render result does not match one processing command and operator lease'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Helm render results are immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP INDEX helm_renderer_readiness_match_idx;
CREATE INDEX helm_renderer_readiness_match_idx
    ON helm_renderer_readiness(contract_version,renderer_image,renderer_version,
                               policy_version,limits_digest,operator_config_digest,
                               observed_at DESC);

CREATE OR REPLACE FUNCTION protect_helm_renderer_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id OR NEW.worker_epoch<OLD.worker_epoch OR
       NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Helm renderer readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.renderer_image<>OLD.renderer_image OR
        NEW.renderer_version<>OLD.renderer_version OR
        NEW.policy_version<>OLD.policy_version OR
        NEW.limits_digest<>OLD.limits_digest OR
        NEW.operator_config_digest<>OLD.operator_config_digest OR
        NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Helm renderer readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
