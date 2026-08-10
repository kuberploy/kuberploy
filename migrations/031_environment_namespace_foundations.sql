-- Immutable protected-Git intents for cluster-level environment namespace
-- foundations. Identity is joined from environments and the sole platform Git
-- authority by the store; this table has no generic Kubernetes/YAML input API.
CREATE TABLE environment_foundation_intents (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL,
    project_id uuid NOT NULL,
    namespace text NOT NULL CHECK (
        namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    argo_project text NOT NULL CHECK (
        argo_project ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    platform_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    target_ref text NOT NULL CHECK (
        target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$' AND
        target_ref !~ '(\.\.|//)'
    ),
    planned_head_revision text NOT NULL CHECK (
        planned_head_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    binding_generation bigint NOT NULL CHECK (binding_generation > 0),
    profile_digest text NOT NULL CHECK (profile_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    publisher_contract text NOT NULL CHECK (
        publisher_contract='environment-foundation-protected-git.v1'
    ),
    publisher_policy text NOT NULL CHECK (
        publisher_policy='platform-protected-git.v1'
    ),
    manifest_path text NOT NULL CHECK (
		manifest_path = 'clusters/'||cluster_id::text||'/argocd/foundations/'||environment_id::text||'.yaml'
    ),
    manifest bytea NOT NULL CHECK (octet_length(manifest) BETWEEN 1 AND 262144),
    manifest_digest text NOT NULL CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    intent_digest text NOT NULL UNIQUE CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    commit_trailer text NOT NULL CHECK (
        commit_trailer = 'Kuberploy-Environment-Foundation-Intent: '||id::text
    ),
    state text NOT NULL CHECK (state IN ('pending','claimed','ready','failed','superseded')),
    active boolean NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_until timestamptz,
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    write_base_observed_at timestamptz,
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_parent_revision text NOT NULL DEFAULT '' CHECK (
        committed_parent_revision='' OR committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    provider_request text NOT NULL DEFAULT '' CHECK (
        length(provider_request)<=256 AND provider_request !~ '[[:cntrl:]]'
    ),
    published_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (environment_id,project_id)
        REFERENCES environments(id,project_id) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,target_ref)
        REFERENCES git_repository_bindings(id,target_ref) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id,cluster_id)
        REFERENCES git_repository_bindings(id,cluster_id) ON DELETE RESTRICT,
    CHECK (updated_at >= created_at AND next_attempt_at >= created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
	CHECK ((write_base_revision='')=(write_base_observed_at IS NULL)),
	CHECK (write_base_observed_at IS NULL OR
	       (write_base_observed_at>=created_at AND write_base_observed_at<=updated_at)),
    CHECK ((state IN ('pending','claimed','ready'))=active),
    CHECK (
        (state='claimed' AND lease_owner IS NOT NULL AND lease_epoch>0 AND lease_until>updated_at AND attempts>0) OR
        (state<>'claimed' AND lease_owner IS NULL AND lease_until IS NULL)
    ),
    CHECK (
        (committed_revision='' AND committed_parent_revision='' AND
         provider_request='' AND published_at IS NULL) OR
		(committed_revision<>'' AND committed_parent_revision=write_base_revision AND
         provider_request<>'' AND published_at IS NOT NULL)
    ),
    CHECK (
        (state='ready' AND committed_revision<>'' AND completed_at=published_at) OR
        (state IN ('pending','claimed') AND committed_revision='' AND completed_at IS NULL) OR
        (state='failed' AND committed_revision='' AND completed_at IS NOT NULL AND consecutive_failures>0) OR
        (state='superseded' AND completed_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX environment_foundation_active_environment_idx
    ON environment_foundation_intents(environment_id) WHERE active;
CREATE INDEX environment_foundation_due_idx
    ON environment_foundation_intents(next_attempt_at,id)
    WHERE active AND state IN ('pending','claimed');
CREATE INDEX environment_foundation_exact_ready_idx
    ON environment_foundation_intents(profile_digest,publisher_config_digest,state)
    WHERE active;

CREATE OR REPLACE FUNCTION protect_environment_foundation_intent()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.environment_id,NEW.project_id,NEW.namespace,NEW.argo_project,
           NEW.platform_binding_id,NEW.cluster_id,NEW.target_ref,NEW.planned_head_revision,
           NEW.binding_generation,NEW.profile_digest,NEW.publisher_config_digest,
           NEW.publisher_contract,NEW.publisher_policy,
           NEW.manifest_path,NEW.manifest,NEW.manifest_digest,NEW.intent_digest,
           NEW.commit_trailer,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.environment_id,OLD.project_id,OLD.namespace,OLD.argo_project,
           OLD.platform_binding_id,OLD.cluster_id,OLD.target_ref,OLD.planned_head_revision,
           OLD.binding_generation,OLD.profile_digest,OLD.publisher_config_digest,
           OLD.publisher_contract,OLD.publisher_policy,
           OLD.manifest_path,OLD.manifest,OLD.manifest_digest,OLD.intent_digest,
           OLD.commit_trailer,OLD.created_at) THEN
        RAISE EXCEPTION 'Environment foundation intent identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Environment foundation lease epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='pending' AND NEW.state IN ('claimed','superseded')) OR
        (OLD.state='claimed' AND NEW.state IN ('claimed','pending','ready','failed','superseded')) OR
        (OLD.state='ready' AND NEW.state IN ('ready','superseded')) OR
        (OLD.state IN ('failed','superseded') AND NEW.state=OLD.state)
    ) THEN
        RAISE EXCEPTION 'Environment foundation state transition is invalid'
            USING ERRCODE='23514';
    END IF;
    IF OLD.committed_revision<>'' AND
       ROW(NEW.committed_revision,NEW.committed_parent_revision,
           NEW.provider_request,NEW.published_at)
       IS DISTINCT FROM
       ROW(OLD.committed_revision,OLD.committed_parent_revision,
           OLD.provider_request,OLD.published_at) THEN
        RAISE EXCEPTION 'Environment foundation publication receipt is immutable'
            USING ERRCODE='23514';
    END IF;
	IF OLD.write_base_revision<>'' AND
	   ROW(NEW.write_base_revision,NEW.write_base_observed_at)
	   IS DISTINCT FROM ROW(OLD.write_base_revision,OLD.write_base_observed_at) THEN
		RAISE EXCEPTION 'Environment foundation write base is immutable'
			USING ERRCODE='23514';
	END IF;
    IF NEW.updated_at<OLD.updated_at OR
       (OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at) OR
       (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) THEN
        RAISE EXCEPTION 'Environment foundation durable time or receipt regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER environment_foundation_intent_protect
    BEFORE UPDATE ON environment_foundation_intents
    FOR EACH ROW EXECUTE FUNCTION protect_environment_foundation_intent();

CREATE TABLE environment_foundation_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (contract_version='environment-foundation.v1'),
    profile_digest text NOT NULL CHECK (profile_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_config_digest text NOT NULL CHECK (
        publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    active_intent_count integer NOT NULL CHECK (active_intent_count BETWEEN 0 AND 10000),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at>=started_at AND lease_until>observed_at)
);
CREATE INDEX environment_foundation_readiness_exact_idx
    ON environment_foundation_readiness(
        contract_version,profile_digest,publisher_config_digest,
        active_intent_count,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_environment_foundation_readiness()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_epoch<OLD.worker_epoch OR NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Environment foundation readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND
       (ROW(NEW.contract_version,NEW.profile_digest,NEW.publisher_config_digest,
            NEW.started_at)
        IS DISTINCT FROM
        ROW(OLD.contract_version,OLD.profile_digest,OLD.publisher_config_digest,
            OLD.started_at) OR
        NEW.observed_at<OLD.observed_at) THEN
        RAISE EXCEPTION 'Environment foundation readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER environment_foundation_readiness_protect
    BEFORE UPDATE ON environment_foundation_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_environment_foundation_readiness();
