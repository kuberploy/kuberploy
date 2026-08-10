-- Edge-runtime readiness is an observation-only contract. It persists no
-- Kubernetes manifests, Secret data, provider credentials, provider endpoints,
-- or arbitrary API paths. The worker may only attest exact operator-approved
-- Traefik, cert-manager, and external-dns profiles.
CREATE TABLE edge_runtime_targets (
    target_key text NOT NULL CHECK (
        length(target_key) BETWEEN 7 AND 64 AND
        target_key ~ '^(traefik|cert-manager|external-dns/[0-9a-f-]{36})$'
    ),
    profile_revision bigint NOT NULL CHECK (profile_revision > 0),
    kind text NOT NULL CHECK (kind IN ('traefik','cert-manager','external-dns')),
    integration_id uuid REFERENCES external_dns_integrations(id) ON DELETE RESTRICT,
    management_mode text NOT NULL CHECK (management_mode IN ('managed','adopted')),
    namespace text NOT NULL CHECK (
        length(namespace) BETWEEN 1 AND 63 AND
        namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    profile_config_map text NOT NULL CHECK (
        length(profile_config_map) BETWEEN 1 AND 253 AND
        profile_config_map ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'
    ),
    external_txt_owner_id text NOT NULL DEFAULT '',
    external_policy text NOT NULL DEFAULT '',
    external_domains text NOT NULL DEFAULT '',
    desired_digest text NOT NULL CHECK (desired_digest ~ '^sha256:[0-9a-f]{64}$'),
    runtime_config_digest text NOT NULL CHECK (runtime_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    active boolean NOT NULL DEFAULT true,
    runtime_state text NOT NULL DEFAULT 'awaiting'
        CHECK (runtime_state IN ('awaiting','ready','failed')),
    next_observation_at timestamptz NOT NULL,
    last_observed_at timestamptz,
    observed_identity_digest text NOT NULL DEFAULT '',
    observed_resource_versions text NOT NULL DEFAULT '',
    consecutive_failures integer NOT NULL DEFAULT 0
        CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR
        last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND
         lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_until timestamptz,
    worker_contract text CHECK (
        worker_contract IS NULL OR worker_contract='edge-observer.v1'
    ),
    worker_config_digest text CHECK (
        worker_config_digest IS NULL OR
        worker_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (target_key,profile_revision),
    CHECK (updated_at >= created_at AND next_observation_at >= created_at),
    CHECK (
        (kind='traefik' AND target_key='traefik' AND integration_id IS NULL AND
         external_txt_owner_id='' AND external_policy='' AND external_domains='') OR
        (kind='cert-manager' AND target_key='cert-manager' AND integration_id IS NULL AND
         external_txt_owner_id='' AND external_policy='' AND external_domains='') OR
        (kind='external-dns' AND integration_id IS NOT NULL AND
         target_key='external-dns/' || integration_id::text AND
         external_txt_owner_id ~ '^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$' AND
         external_policy IN ('upsert-only','sync') AND
         length(external_domains) BETWEEN 1 AND 16384 AND
         external_domains !~ '[[:space:][:cntrl:]]')
    ),
    CHECK (
        (last_observed_at IS NULL AND observed_identity_digest='' AND
         observed_resource_versions='') OR
        (last_observed_at IS NOT NULL AND
         observed_identity_digest ~ '^sha256:[0-9a-f]{64}$' AND
         observed_resource_versions ~ '^sha256:[0-9a-f]{64}$')
    ),
    CHECK (runtime_state<>'ready' OR last_observed_at IS NOT NULL),
    CHECK ((last_failure_code='') = (consecutive_failures=0)),
    CHECK (
        (lease_owner IS NULL AND lease_until IS NULL AND worker_contract IS NULL AND
         worker_config_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_until IS NOT NULL AND
         worker_contract='edge-observer.v1' AND
         worker_config_digest=runtime_config_digest AND lease_epoch>0 AND
         lease_until>updated_at)
    )
);

CREATE UNIQUE INDEX edge_runtime_targets_active_key_idx
    ON edge_runtime_targets(target_key) WHERE active;
CREATE INDEX edge_runtime_targets_due_idx
    ON edge_runtime_targets(next_observation_at,target_key,profile_revision)
    WHERE active AND runtime_state<>'failed';
CREATE INDEX edge_runtime_targets_readiness_idx
    ON edge_runtime_targets(runtime_config_digest,runtime_state,last_observed_at)
    WHERE active;

CREATE OR REPLACE FUNCTION validate_edge_runtime_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_mode text;
    durable_txt_owner text;
    durable_policy text;
    durable_profile text;
    durable_domains text;
BEGIN
    IF NEW.kind='external-dns' AND NEW.active THEN
        SELECT i.mode,i.txt_owner_id,i.sync_policy,
               COALESCE(i.operator_profile_ref,''),
               COALESCE((
                   SELECT string_agg(suffix.value,',' ORDER BY suffix.value)
                     FROM jsonb_array_elements_text(i.allowed_domain_suffixes) AS suffix(value)
               ),'')
          INTO durable_mode,durable_txt_owner,durable_policy,durable_profile,durable_domains
          FROM external_dns_integrations i
         WHERE i.id=NEW.integration_id;
        IF NOT FOUND OR
           ROW(NEW.management_mode,NEW.external_txt_owner_id,NEW.external_policy,NEW.external_domains)
           IS DISTINCT FROM
           ROW(durable_mode,durable_txt_owner,durable_policy,durable_domains) OR
           (NEW.management_mode='adopted' AND NEW.profile_config_map<>durable_profile) OR
           (NEW.management_mode='managed' AND durable_profile<>'') THEN
            RAISE EXCEPTION 'External DNS edge target does not match its safe integration metadata'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.kind,NEW.integration_id,
               NEW.management_mode,NEW.namespace,NEW.profile_config_map,
               NEW.external_txt_owner_id,NEW.external_policy,NEW.external_domains,
               NEW.desired_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.kind,OLD.integration_id,
               OLD.management_mode,OLD.namespace,OLD.profile_config_map,
               OLD.external_txt_owner_id,OLD.external_policy,OLD.external_domains,
               OLD.desired_digest,OLD.created_at) THEN
            RAISE EXCEPTION 'Edge runtime target identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Edge runtime target lease epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Edge runtime target lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.observed_identity_digest<>'' AND
           NEW.observed_identity_digest<>OLD.observed_identity_digest THEN
            RAISE EXCEPTION 'Edge runtime observed Kubernetes identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.last_observed_at IS NOT NULL AND
            (NEW.last_observed_at IS NULL OR NEW.last_observed_at<OLD.last_observed_at)) THEN
            RAISE EXCEPTION 'Edge runtime target time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER edge_runtime_target_validate
    BEFORE INSERT OR UPDATE ON edge_runtime_targets
    FOR EACH ROW EXECUTE FUNCTION validate_edge_runtime_target();

-- A capability may only consume a fresh exact worker observation. A restarted
-- worker must advance its durable epoch by exactly one; stale processes cannot
-- overwrite the new worker's readiness lease.
CREATE TABLE edge_runtime_readiness (
    worker_id text PRIMARY KEY CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch > 0),
    contract_version text NOT NULL CHECK (contract_version='edge-observer.v1'),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    target_count integer NOT NULL CHECK (target_count BETWEEN 1 AND 66),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    lease_until timestamptz NOT NULL,
    CHECK (observed_at >= started_at AND lease_until > observed_at)
);

CREATE INDEX edge_runtime_readiness_match_idx
    ON edge_runtime_readiness(
        contract_version,config_digest,target_count,observed_at DESC
    );

CREATE OR REPLACE FUNCTION protect_edge_runtime_readiness_epoch()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'Edge runtime worker identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch<OLD.worker_epoch OR NEW.worker_epoch>OLD.worker_epoch+1 THEN
        RAISE EXCEPTION 'Edge runtime readiness epoch is invalid'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND (
        NEW.contract_version<>OLD.contract_version OR
        NEW.config_digest<>OLD.config_digest OR
        NEW.target_count<>OLD.target_count OR
        NEW.started_at<>OLD.started_at OR
        NEW.observed_at<OLD.observed_at
    ) THEN
        RAISE EXCEPTION 'Edge runtime readiness identity or time regressed'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER edge_runtime_readiness_epoch
    BEFORE UPDATE ON edge_runtime_readiness
    FOR EACH ROW EXECUTE FUNCTION protect_edge_runtime_readiness_epoch();
