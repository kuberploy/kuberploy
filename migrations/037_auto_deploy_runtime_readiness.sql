-- Exact, lease-fenced readiness for the control-plane auto-deploy controller.
-- Capability and policy mutation surfaces must match this identity; a chart
-- flag alone never proves that the canonical image-only pipeline is running.

CREATE TABLE auto_deploy_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,127}$'),
    contract_version text NOT NULL CHECK (contract_version='auto-deploy.v1'),
    operator_config_digest text NOT NULL CHECK (operator_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_epoch bigint NOT NULL CHECK (lease_epoch>0),
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at),
    CHECK (lease_until>observed_at),
    CHECK (lease_until<=observed_at+interval '5 minutes')
);

CREATE OR REPLACE FUNCTION protect_auto_deploy_runtime_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF NEW.observed_at>clock_timestamp()+interval '30 seconds' OR NEW.started_at>NEW.observed_at THEN
        RAISE EXCEPTION 'auto-deploy readiness timestamp is invalid' USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.lease_epoch<>1 THEN
            RAISE EXCEPTION 'auto-deploy readiness must start at epoch one' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' OR NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'auto-deploy readiness identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.contract_version=OLD.contract_version AND NEW.operator_config_digest=OLD.operator_config_digest AND
       NEW.started_at=OLD.started_at THEN
        IF OLD.lease_until>NEW.observed_at AND NEW.lease_epoch=OLD.lease_epoch AND
           NEW.observed_at>OLD.observed_at AND NEW.lease_until>OLD.lease_until THEN
            NULL; -- active heartbeat
        ELSIF OLD.lease_until<=NEW.observed_at AND NEW.lease_epoch=OLD.lease_epoch+1 AND
              NEW.observed_at>OLD.observed_at AND NEW.lease_until>NEW.observed_at THEN
            NULL; -- expired lease reacquisition by the same process identity
        ELSE
            RAISE EXCEPTION 'invalid auto-deploy readiness heartbeat' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.started_at<=OLD.started_at OR NEW.observed_at<NEW.started_at OR NEW.lease_epoch<>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'invalid auto-deploy readiness identity replacement' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END; $$;

CREATE TRIGGER auto_deploy_runtime_readiness_protect
    BEFORE INSERT OR UPDATE OR DELETE ON auto_deploy_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_auto_deploy_runtime_readiness();
