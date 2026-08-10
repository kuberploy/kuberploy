-- Durable proof that a production worker is observing the exact dynamic
-- ClusterIssuer catalog. API capability and admin mutations fail closed when
-- this lease is absent, stale, superseded, or bound to a different runtime
-- identity/target set.
CREATE TABLE cert_manager_issuer_observer_readiness (
    worker_id text PRIMARY KEY CHECK (worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    contract_version text NOT NULL CHECK (contract_version='cert-manager-cluster-issuer-observer.v1'),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    target_digest text NOT NULL CHECK (target_digest ~ '^sha256:[0-9a-f]{64}$'),
    target_count integer NOT NULL CHECK (target_count>=0 AND target_count<=128),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_epoch bigint NOT NULL CHECK (lease_epoch>0),
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (config_digest),
    CHECK (observed_at>=started_at AND updated_at>=observed_at AND lease_until>observed_at AND lease_until<=observed_at+interval '15 minutes')
);
CREATE INDEX cert_manager_issuer_observer_readiness_match_idx
    ON cert_manager_issuer_observer_readiness(contract_version,config_digest,target_digest,target_count,observed_at,lease_until);

CREATE OR REPLACE FUNCTION protect_cert_manager_issuer_observer_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'cert-manager issuer observer readiness cannot be deleted' USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.lease_epoch<>1 OR NEW.updated_at<>NEW.observed_at OR NEW.observed_at>clock_timestamp()+interval '30 seconds' THEN
            RAISE EXCEPTION 'invalid initial cert-manager issuer observer readiness lease' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.observed_at<OLD.observed_at OR NEW.updated_at<OLD.updated_at OR
       NEW.observed_at>clock_timestamp()+interval '30 seconds' THEN
        RAISE EXCEPTION 'invalid cert-manager issuer observer readiness mutation' USING ERRCODE='23514';
    END IF;
    IF NEW.worker_id=OLD.worker_id AND NEW.contract_version=OLD.contract_version AND NEW.config_digest=OLD.config_digest AND
       NEW.started_at=OLD.started_at AND NEW.lease_epoch=OLD.lease_epoch THEN
        IF OLD.lease_until<=NEW.observed_at OR NEW.updated_at<>NEW.observed_at OR NEW.lease_until<=OLD.lease_until THEN
            RAISE EXCEPTION 'invalid cert-manager issuer observer heartbeat' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.contract_version<>OLD.contract_version OR NEW.config_digest<>OLD.config_digest OR
       OLD.lease_until>NEW.observed_at OR NEW.lease_epoch<>OLD.lease_epoch+1 OR
       NEW.started_at<OLD.started_at OR NEW.updated_at<>NEW.observed_at THEN
        RAISE EXCEPTION 'invalid cert-manager issuer observer lease replacement' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;
CREATE TRIGGER cert_manager_issuer_observer_readiness_protect
BEFORE INSERT OR UPDATE OR DELETE ON cert_manager_issuer_observer_readiness
FOR EACH ROW EXECUTE FUNCTION protect_cert_manager_issuer_observer_readiness();
