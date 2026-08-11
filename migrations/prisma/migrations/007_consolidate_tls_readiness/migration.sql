-- The TLS certificate observer has the same worker lease/readiness lifecycle
-- as every other runtime component. Its per-certificate target observations
-- remain in their dedicated authority table.

ALTER TABLE runtime_readiness DROP CONSTRAINT runtime_readiness_runtime_kind_check;
ALTER TABLE runtime_readiness ADD CONSTRAINT runtime_readiness_runtime_kind_check CHECK (runtime_kind IN (
    'source-build','managed-registry','git-projection','runtime-secret',
    'argo-desired-state','runtime-registry-pull','edge','helm-renderer',
    'helm-protected-publisher','environment-foundation','auto-deploy',
    'certificate-issuer-observer','tls-certificate-observer'
));

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'tls-certificate-observer','global',worker_id,worker_epoch,contract_version,config_digest,
    '{}'::jsonb,'{}'::jsonb,started_at,observed_at,lease_until,updated_at
FROM tls_certificate_observation_workers;

DROP TABLE tls_certificate_observation_workers;
DROP FUNCTION protect_tls_certificate_observation_worker();

CREATE FUNCTION validate_tls_certificate_runtime_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.runtime_kind='tls-certificate-observer' THEN
            RAISE EXCEPTION 'TLS certificate observer readiness is retained' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.runtime_kind<>'tls-certificate-observer' THEN RETURN NEW; END IF;
    IF NEW.scope_key<>'global' OR NEW.registry_target_id IS NOT NULL OR NEW.platform_binding_id IS NOT NULL OR
       NEW.identity<>'{}'::jsonb OR NEW.observation<>'{}'::jsonb OR
       NEW.worker_id !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$' OR
       NEW.updated_at<>NEW.observed_at THEN
        RAISE EXCEPTION 'invalid TLS certificate observer readiness' USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.worker_epoch<>1 THEN
            RAISE EXCEPTION 'TLS certificate observer readiness must start at epoch one' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'TLS certificate observer identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch+1 AND NEW.started_at>=OLD.observed_at THEN RETURN NEW; END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND NEW.contract_version=OLD.contract_version AND
       NEW.config_digest=OLD.config_digest AND NEW.started_at=OLD.started_at AND
       NEW.observed_at>=OLD.observed_at AND NEW.observed_at<OLD.lease_until AND
       NEW.lease_until>OLD.lease_until THEN RETURN NEW; END IF;
    RAISE EXCEPTION 'invalid TLS certificate observer readiness transition' USING ERRCODE='23514';
END;
$$;
CREATE TRIGGER runtime_readiness_tls_certificate_validate
    BEFORE INSERT OR UPDATE OR DELETE ON runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION validate_tls_certificate_runtime_readiness();
