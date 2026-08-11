-- Runtime components share one lease/observation lifecycle. Exact component
-- identities remain closed and kind-specific in PostgreSQL; Secret material,
-- provider payloads, and arbitrary runtime state are never stored here.

CREATE TABLE runtime_readiness (
    runtime_kind text NOT NULL CHECK (runtime_kind IN (
        'source-build','managed-registry','git-projection','runtime-secret',
        'argo-desired-state','runtime-registry-pull','edge','helm-renderer',
        'helm-protected-publisher','environment-foundation','auto-deploy',
        'certificate-issuer-observer'
    )),
    scope_key text NOT NULL CHECK (
        scope_key='global' OR
        scope_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
    ),
    worker_id text NOT NULL CHECK (
        length(worker_id) BETWEEN 1 AND 256 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        length(contract_version) BETWEEN 8 AND 64 AND
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    identity jsonb NOT NULL CHECK (jsonb_typeof(identity)='object' AND octet_length(identity::text)<=8192),
    observation jsonb NOT NULL CHECK (jsonb_typeof(observation)='object' AND octet_length(observation::text)<=2048),
    registry_target_id uuid REFERENCES registry_targets(id) ON DELETE RESTRICT,
    platform_binding_id uuid REFERENCES git_repository_bindings(id) ON DELETE RESTRICT,
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (runtime_kind,scope_key,worker_id),
    CHECK (observed_at>=started_at AND updated_at>=observed_at AND lease_until>observed_at),
    CHECK (
        (runtime_kind='managed-registry' AND registry_target_id IS NOT NULL AND platform_binding_id IS NULL
            AND scope_key=registry_target_id::text) OR
        (runtime_kind='argo-desired-state' AND registry_target_id IS NULL AND platform_binding_id IS NOT NULL
            AND scope_key='global') OR
        (runtime_kind NOT IN ('managed-registry','argo-desired-state')
            AND registry_target_id IS NULL AND platform_binding_id IS NULL AND scope_key='global')
    )
);
CREATE UNIQUE INDEX runtime_readiness_certificate_config_idx
    ON runtime_readiness(runtime_kind,config_digest)
    WHERE runtime_kind='certificate-issuer-observer';
CREATE INDEX runtime_readiness_match_idx
    ON runtime_readiness(runtime_kind,scope_key,contract_version,config_digest,observed_at DESC,lease_until);

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'source-build','global',worker_id,1,'source-build.v1',config_digest,
    jsonb_build_object('githubAppId',github_app_id,'builderNamespace',builder_namespace,'builderAgentImage',builder_agent_image),
    '{}'::jsonb,started_at,observed_at,observed_at+interval '5 minutes',observed_at
FROM source_build_runtime_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,registry_target_id,started_at,observed_at,lease_until,updated_at)
SELECT 'managed-registry',registry_target_id::text,worker_id,worker_epoch,contract_version,config_digest,
    '{}'::jsonb,'{}'::jsonb,registry_target_id,started_at,observed_at,lease_until,observed_at
FROM managed_registry_runtime_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'git-projection','global',worker_id,worker_epoch,contract_version,config_digest,
    jsonb_build_object('githubAppId',github_app_id),'{}'::jsonb,started_at,observed_at,lease_until,observed_at
FROM git_projection_runtime_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'runtime-secret','global',worker_id,worker_epoch,contract_version,config_digest,
    jsonb_build_object('fingerprintKeyId',fingerprint_key_id,'sealingKeyFingerprint',sealing_key_fingerprint),
    '{}'::jsonb,started_at,observed_at,lease_until,observed_at
FROM runtime_secret_runtime_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,platform_binding_id,started_at,observed_at,lease_until,updated_at)
SELECT 'argo-desired-state','global',worker_id,worker_epoch,contract_version,config_digest,
    jsonb_build_object(
        'githubAppId',github_app_id,'clusterId',cluster_id,'argoNamespace',argo_namespace,
        'rootApplicationName',root_application_name,'repositorySecretName',repository_secret_name,
        'chartRepository',chart_repository,'chartName',chart_name,'chartVersion',chart_version,
        'chartDigest',chart_digest,'rendererImage',renderer_image,
        'chartDigestEnforcement',chart_digest_enforcement
    ),'{}'::jsonb,platform_binding_id,started_at,observed_at,lease_until,observed_at
FROM argo_desired_state_runtime_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'runtime-registry-pull','global',worker_id,worker_epoch,contract_version,config_digest,
    jsonb_build_object('profileCount',profile_count),'{}'::jsonb,started_at,observed_at,lease_until,observed_at
FROM runtime_registry_pull_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'edge','global',worker_id,worker_epoch,contract_version,config_digest,
    jsonb_build_object('targetCount',target_count),'{}'::jsonb,started_at,observed_at,lease_until,observed_at
FROM edge_runtime_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'helm-renderer','global',worker_id,worker_epoch,contract_version,operator_config_digest,
    jsonb_build_object('rendererImage',renderer_image,'rendererVersion',renderer_version,
        'policyVersion',policy_version,'limitsDigest',limits_digest),
    '{}'::jsonb,started_at,observed_at,lease_until,observed_at
FROM helm_renderer_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'helm-protected-publisher','global',worker_id,worker_epoch,contract_version,config_digest,
    jsonb_build_object('policyVersion',policy_version),'{}'::jsonb,started_at,observed_at,lease_until,observed_at
FROM helm_protected_publisher_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'environment-foundation','global',worker_id,worker_epoch,contract_version,publisher_config_digest,
    jsonb_build_object('profileDigest',profile_digest),jsonb_build_object('activeIntentCount',active_intent_count),
    started_at,observed_at,lease_until,observed_at
FROM environment_foundation_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'auto-deploy','global',worker_id,lease_epoch,contract_version,operator_config_digest,
    '{}'::jsonb,'{}'::jsonb,started_at,observed_at,lease_until,observed_at
FROM auto_deploy_runtime_readiness;

INSERT INTO runtime_readiness(runtime_kind,scope_key,worker_id,worker_epoch,contract_version,config_digest,
    identity,observation,started_at,observed_at,lease_until,updated_at)
SELECT 'certificate-issuer-observer','global',worker_id,lease_epoch,contract_version,config_digest,
    '{}'::jsonb,jsonb_build_object('targetDigest',target_digest,'targetCount',target_count),
    started_at,observed_at,lease_until,updated_at
FROM cert_manager_issuer_observer_readiness;

DROP TABLE source_build_runtime_readiness;
DROP TABLE managed_registry_runtime_readiness;
DROP TABLE git_projection_runtime_readiness;
DROP TABLE runtime_secret_runtime_readiness;
DROP TABLE argo_desired_state_runtime_readiness;
DROP TABLE runtime_registry_pull_readiness;
DROP TABLE edge_runtime_readiness;
DROP TABLE helm_renderer_readiness;
DROP TABLE helm_protected_publisher_readiness;
DROP TABLE environment_foundation_readiness;
DROP TABLE auto_deploy_runtime_readiness;
DROP TABLE cert_manager_issuer_observer_readiness;

CREATE OR REPLACE FUNCTION validate_runtime_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_kind text;
    binding_mode text;
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.runtime_kind IN ('auto-deploy','certificate-issuer-observer') THEN
            RAISE EXCEPTION 'protected runtime readiness cannot be deleted' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;

    IF NEW.runtime_kind='source-build' THEN
        IF NEW.contract_version<>'source-build.v1' OR NEW.identity - ARRAY['githubAppId','builderNamespace','builderAgentImage'] <> '{}'::jsonb OR
           NOT (NEW.identity ?& ARRAY['githubAppId','builderNamespace','builderAgentImage']) OR NEW.observation<>'{}'::jsonb OR
           NEW.identity->>'githubAppId' !~ '^[1-9][0-9]*$' OR
           NEW.identity->>'builderNamespace' !~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$' OR
           NEW.identity->>'builderAgentImage' !~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' OR
           NEW.lease_until>NEW.observed_at+interval '5 minutes' THEN
            RAISE EXCEPTION 'invalid source-build runtime readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='managed-registry' THEN
        IF NEW.identity<>'{}'::jsonb OR NEW.observation<>'{}'::jsonb OR NOT EXISTS (
            SELECT 1 FROM registry_targets WHERE id=NEW.registry_target_id AND mode='managed'
        ) THEN
            RAISE EXCEPTION 'invalid managed-registry runtime readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='git-projection' THEN
        IF NEW.identity - ARRAY['githubAppId'] <> '{}'::jsonb OR NOT (NEW.identity ? 'githubAppId') OR
           NEW.identity->>'githubAppId' !~ '^[1-9][0-9]*$' OR NEW.observation<>'{}'::jsonb THEN
            RAISE EXCEPTION 'invalid Git projection runtime readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='runtime-secret' THEN
        IF NEW.identity - ARRAY['fingerprintKeyId','sealingKeyFingerprint'] <> '{}'::jsonb OR
           NOT (NEW.identity ?& ARRAY['fingerprintKeyId','sealingKeyFingerprint']) OR NEW.observation<>'{}'::jsonb OR
           NEW.identity->>'fingerprintKeyId' !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$' OR
           NEW.identity->>'sealingKeyFingerprint' !~ '^sha256:[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'invalid runtime-secret readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='argo-desired-state' THEN
        IF NEW.identity - ARRAY['githubAppId','clusterId','argoNamespace','rootApplicationName','repositorySecretName',
                'chartRepository','chartName','chartVersion','chartDigest','rendererImage','chartDigestEnforcement'] <> '{}'::jsonb OR
           NOT (NEW.identity ?& ARRAY['githubAppId','clusterId','argoNamespace','rootApplicationName','repositorySecretName',
                'chartRepository','chartName','chartVersion','chartDigest','rendererImage','chartDigestEnforcement']) OR
           NEW.identity->>'githubAppId' !~ '^[1-9][0-9]*$' OR
           NEW.identity->>'clusterId' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' OR
           NEW.identity->>'argoNamespace' !~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$' OR
           NEW.identity->>'rootApplicationName' !~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$' OR
           NEW.identity->>'repositorySecretName' !~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$' OR
           NEW.identity->>'chartRepository' !~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$' OR
           NEW.identity->>'chartName'<>'kuberploy-runtime' OR
           NEW.identity->>'chartVersion' !~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$' OR
           NEW.identity->>'chartDigest' !~ '^sha256:[0-9a-f]{64}$' OR
           NEW.identity->>'rendererImage' !~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$' OR
           NEW.identity->>'chartDigestEnforcement'<>'native-oci-digest-v1' OR NEW.observation<>'{}'::jsonb THEN
            RAISE EXCEPTION 'invalid Argo desired-state runtime readiness' USING ERRCODE='23514';
        END IF;
        SELECT kind,credential_mode INTO binding_kind,binding_mode FROM git_repository_bindings
        WHERE id=NEW.platform_binding_id AND cluster_id::text=NEW.identity->>'clusterId';
        IF binding_kind IS DISTINCT FROM 'platform' OR binding_mode IS DISTINCT FROM 'github-app' THEN
            RAISE EXCEPTION 'Argo readiness requires a protected GitHub App platform binding' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='runtime-registry-pull' THEN
        IF NEW.identity - ARRAY['profileCount'] <> '{}'::jsonb OR NOT (NEW.identity ? 'profileCount') OR
           NEW.identity->>'profileCount' !~ '^[0-9]+$' OR (NEW.identity->>'profileCount')::integer NOT BETWEEN 1 AND 32 OR
           NEW.observation<>'{}'::jsonb THEN
            RAISE EXCEPTION 'invalid registry-pull runtime readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='edge' THEN
        IF NEW.contract_version<>'edge-observer.v1' OR NEW.identity - ARRAY['targetCount'] <> '{}'::jsonb OR
           NOT (NEW.identity ? 'targetCount') OR NEW.identity->>'targetCount' !~ '^[0-9]+$' OR
           (NEW.identity->>'targetCount')::integer NOT BETWEEN 1 AND 66 OR NEW.observation<>'{}'::jsonb THEN
            RAISE EXCEPTION 'invalid edge runtime readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='helm-renderer' THEN
        IF NEW.contract_version<>'external-helm-renderer.v1' OR
           NEW.identity - ARRAY['rendererImage','rendererVersion','policyVersion','limitsDigest'] <> '{}'::jsonb OR
           NOT (NEW.identity ?& ARRAY['rendererImage','rendererVersion','policyVersion','limitsDigest']) OR
           NEW.identity->>'rendererImage'<>'docker.io/alpine/helm:4.2.3' OR NEW.identity->>'rendererVersion'<>'4.2.3' OR
           NEW.identity->>'policyVersion'<>'external-helm-p0.v1' OR NEW.identity->>'limitsDigest' !~ '^sha256:[0-9a-f]{64}$' OR
           NEW.observation<>'{}'::jsonb THEN
            RAISE EXCEPTION 'invalid Helm renderer readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='helm-protected-publisher' THEN
        IF NEW.contract_version<>'helm-protected-publisher.v1' OR NEW.identity<>jsonb_build_object('policyVersion','helm-protected-git.v1') OR
           NEW.observation<>'{}'::jsonb THEN
            RAISE EXCEPTION 'invalid Helm protected publisher readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='environment-foundation' THEN
        IF NEW.contract_version<>'environment-foundation.v1' OR NEW.identity - ARRAY['profileDigest'] <> '{}'::jsonb OR
           NEW.identity->>'profileDigest' !~ '^sha256:[0-9a-f]{64}$' OR
           NEW.observation - ARRAY['activeIntentCount'] <> '{}'::jsonb OR
           NEW.observation->>'activeIntentCount' !~ '^[0-9]+$' OR
           (NEW.observation->>'activeIntentCount')::integer NOT BETWEEN 0 AND 10000 THEN
            RAISE EXCEPTION 'invalid environment-foundation readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='auto-deploy' THEN
        IF NEW.contract_version<>'auto-deploy.v1' OR NEW.identity<>'{}'::jsonb OR NEW.observation<>'{}'::jsonb OR
           NEW.lease_until>NEW.observed_at+interval '5 minutes' OR NEW.observed_at>clock_timestamp()+interval '30 seconds' THEN
            RAISE EXCEPTION 'invalid auto-deploy readiness' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='certificate-issuer-observer' THEN
        IF NEW.contract_version<>'cert-manager-cluster-issuer-observer.v1' OR NEW.identity<>'{}'::jsonb OR
           NEW.observation - ARRAY['targetDigest','targetCount'] <> '{}'::jsonb OR
           NOT (NEW.observation ?& ARRAY['targetDigest','targetCount']) OR
           NEW.observation->>'targetDigest' !~ '^sha256:[0-9a-f]{64}$' OR
           NEW.observation->>'targetCount' !~ '^[0-9]+$' OR
           (NEW.observation->>'targetCount')::integer NOT BETWEEN 0 AND 128 OR
           NEW.lease_until>NEW.observed_at+interval '15 minutes' OR NEW.observed_at>clock_timestamp()+interval '30 seconds' THEN
            RAISE EXCEPTION 'invalid certificate issuer observer readiness' USING ERRCODE='23514';
        END IF;
    END IF;

    IF TG_OP='INSERT' THEN
        IF NEW.worker_epoch<>1 AND NEW.runtime_kind IN ('auto-deploy','certificate-issuer-observer') THEN
            RAISE EXCEPTION 'protected readiness must start at epoch one' USING ERRCODE='23514';
        END IF;
        IF NEW.runtime_kind='certificate-issuer-observer' AND NEW.updated_at<>NEW.observed_at THEN
            RAISE EXCEPTION 'invalid initial certificate issuer observer lease' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;

    IF ROW(NEW.runtime_kind,NEW.scope_key,NEW.registry_target_id,NEW.platform_binding_id)
       IS DISTINCT FROM ROW(OLD.runtime_kind,OLD.scope_key,OLD.registry_target_id,OLD.platform_binding_id) OR
       (NEW.worker_id<>OLD.worker_id AND NEW.runtime_kind<>'certificate-issuer-observer') THEN
        RAISE EXCEPTION 'runtime readiness identity is immutable' USING ERRCODE='23514';
    END IF;

    IF NEW.runtime_kind='auto-deploy' THEN
        IF NEW.contract_version=OLD.contract_version AND NEW.config_digest=OLD.config_digest AND NEW.started_at=OLD.started_at THEN
            IF OLD.lease_until>NEW.observed_at AND NEW.worker_epoch=OLD.worker_epoch AND
               NEW.observed_at>OLD.observed_at AND NEW.lease_until>OLD.lease_until THEN NULL;
            ELSIF OLD.lease_until<=NEW.observed_at AND NEW.worker_epoch=OLD.worker_epoch+1 AND
                  NEW.observed_at>OLD.observed_at THEN NULL;
            ELSE RAISE EXCEPTION 'invalid auto-deploy readiness heartbeat' USING ERRCODE='23514';
            END IF;
        ELSIF NEW.started_at<=OLD.started_at OR NEW.worker_epoch<>OLD.worker_epoch+1 THEN
            RAISE EXCEPTION 'invalid auto-deploy readiness identity replacement' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.runtime_kind='certificate-issuer-observer' THEN
        IF NEW.observed_at<OLD.observed_at OR NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'invalid certificate issuer observer readiness mutation' USING ERRCODE='23514';
        END IF;
        IF NEW.contract_version=OLD.contract_version AND NEW.config_digest=OLD.config_digest AND
           NEW.started_at=OLD.started_at AND NEW.worker_epoch=OLD.worker_epoch THEN
            IF OLD.lease_until<=NEW.observed_at OR NEW.updated_at<>NEW.observed_at OR NEW.lease_until<=OLD.lease_until THEN
                RAISE EXCEPTION 'invalid certificate issuer observer heartbeat' USING ERRCODE='23514';
            END IF;
        ELSIF NEW.contract_version<>OLD.contract_version OR NEW.config_digest<>OLD.config_digest OR
              OLD.lease_until>NEW.observed_at OR NEW.worker_epoch<>OLD.worker_epoch+1 OR
              NEW.started_at<OLD.started_at OR NEW.updated_at<>NEW.observed_at THEN
            RAISE EXCEPTION 'invalid certificate issuer observer lease replacement' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.worker_epoch<OLD.worker_epoch OR NEW.worker_epoch>OLD.worker_epoch+1 THEN
            RAISE EXCEPTION 'runtime readiness epoch is invalid' USING ERRCODE='23514';
        END IF;
        IF NEW.worker_epoch=OLD.worker_epoch AND (
            NEW.contract_version<>OLD.contract_version OR NEW.config_digest<>OLD.config_digest OR
            NEW.identity<>OLD.identity OR NEW.started_at<>OLD.started_at OR NEW.observed_at<OLD.observed_at
        ) THEN
            RAISE EXCEPTION 'runtime readiness identity or time regressed' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER runtime_readiness_validate
    BEFORE INSERT OR UPDATE OR DELETE ON runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION validate_runtime_readiness();

DROP FUNCTION validate_managed_registry_runtime_readiness_target();
DROP FUNCTION protect_managed_registry_runtime_readiness_epoch();
DROP FUNCTION protect_git_projection_runtime_readiness_epoch();
DROP FUNCTION protect_runtime_secret_runtime_readiness_epoch();
DROP FUNCTION protect_argo_desired_state_runtime_readiness();
DROP FUNCTION protect_runtime_registry_pull_readiness_epoch();
DROP FUNCTION protect_edge_runtime_readiness_epoch();
DROP FUNCTION protect_helm_renderer_readiness_epoch();
DROP FUNCTION protect_helm_protected_publisher_readiness_epoch();
DROP FUNCTION protect_environment_foundation_readiness();
DROP FUNCTION protect_auto_deploy_runtime_readiness();
DROP FUNCTION protect_cert_manager_issuer_observer_readiness();
