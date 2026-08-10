-- Custom ingress certificates reuse the write-only strict SealedSecret
-- lifecycle, but are isolated from general workload secrets by an immutable
-- binding purpose and Kubernetes Secret type. Only public X.509 metadata and
-- the platform-keyed content identity are retained here.

ALTER TABLE secret_bindings
    ADD COLUMN IF NOT EXISTS purpose text NOT NULL DEFAULT 'runtime-secret';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname='secret_bindings_purpose_check'
          AND conrelid='secret_bindings'::regclass
    ) THEN
        ALTER TABLE secret_bindings
            ADD CONSTRAINT secret_bindings_purpose_check CHECK (
                purpose='runtime-secret' OR
                (purpose='tls-certificate' AND provider='sealed-secrets')
            );
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION protect_secret_binding_purpose()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.purpose IS DISTINCT FROM OLD.purpose THEN
        RAISE EXCEPTION 'secret binding purpose is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS secret_bindings_purpose_protect ON secret_bindings;
CREATE TRIGGER secret_bindings_purpose_protect
    BEFORE UPDATE ON secret_bindings
    FOR EACH ROW EXECUTE FUNCTION protect_secret_binding_purpose();

ALTER TABLE secret_binding_versions
    ADD COLUMN IF NOT EXISTS target_secret_type text NOT NULL DEFAULT 'Opaque';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname='secret_binding_versions_target_type_check'
          AND conrelid='secret_binding_versions'::regclass
    ) THEN
        ALTER TABLE secret_binding_versions
            ADD CONSTRAINT secret_binding_versions_target_type_check CHECK (
                target_secret_type IN ('Opaque','kubernetes.io/tls')
            );
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_secret_binding_version_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    binding_purpose text;
    binding_provider text;
BEGIN
    IF TG_OP='UPDATE' AND NEW.target_secret_type IS DISTINCT FROM OLD.target_secret_type THEN
        RAISE EXCEPTION 'secret binding target type is immutable' USING ERRCODE='23514';
    END IF;
    SELECT purpose,provider
      INTO binding_purpose,binding_provider
      FROM secret_bindings
      WHERE id=NEW.binding_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'secret binding does not exist' USING ERRCODE='23503';
    END IF;
    IF NOT (
        (binding_purpose='runtime-secret' AND NEW.target_secret_type='Opaque') OR
        (binding_purpose='tls-certificate' AND binding_provider='sealed-secrets'
         AND NEW.provider='sealed-secrets' AND NEW.target_secret_type='kubernetes.io/tls')
    ) THEN
        RAISE EXCEPTION 'secret binding purpose and target type mismatch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS secret_binding_versions_target_protect ON secret_binding_versions;
CREATE TRIGGER secret_binding_versions_target_protect
    BEFORE INSERT OR UPDATE ON secret_binding_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_secret_binding_version_target();

CREATE TABLE IF NOT EXISTS tls_certificate_versions (
    version_id uuid PRIMARY KEY,
    binding_id uuid NOT NULL,
    version_number bigint NOT NULL CHECK (version_number>0),
    secret_content_fingerprint bytea NOT NULL CHECK (
        octet_length(secret_content_fingerprint)=32
    ),
    leaf_fingerprint text NOT NULL CHECK (
        leaf_fingerprint ~ '^sha256:[0-9a-f]{64}$'
    ),
    public_key_fingerprint text NOT NULL CHECK (
        public_key_fingerprint ~ '^sha256:[0-9a-f]{64}$'
    ),
    dns_names jsonb NOT NULL CHECK (
        jsonb_typeof(dns_names)='array' AND
        jsonb_array_length(dns_names) BETWEEN 1 AND 128
    ),
    ip_addresses jsonb NOT NULL CHECK (
        jsonb_typeof(ip_addresses)='array' AND
        jsonb_array_length(ip_addresses) BETWEEN 0 AND 128 AND
        jsonb_array_length(dns_names)+jsonb_array_length(ip_addresses)<=128
    ),
    not_before timestamptz NOT NULL,
    not_after timestamptz NOT NULL,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    UNIQUE (binding_id,version_number),
    UNIQUE (version_id,binding_id),
    FOREIGN KEY (version_id,binding_id)
        REFERENCES secret_binding_versions(id,binding_id) ON DELETE RESTRICT,
    CHECK (not_after>not_before)
);

CREATE INDEX IF NOT EXISTS tls_certificate_versions_binding_idx
    ON tls_certificate_versions(binding_id,version_number);

CREATE OR REPLACE FUNCTION validate_tls_certificate_version()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    stored_binding_id uuid;
    stored_number bigint;
    stored_binding_purpose text;
    stored_binding_provider text;
    stored_version_provider text;
    stored_target_type text;
    stored_state text;
    stored_fingerprint bytea;
    stored_artifact boolean;
    staging_actor uuid;
    item jsonb;
    value text;
    previous text := '';
BEGIN
    SELECT v.binding_id,v.version_number,b.purpose,b.provider,v.provider,
           v.target_secret_type,v.state,v.content_fingerprint,
           (v.provider_object_name IS NOT NULL)
      INTO stored_binding_id,stored_number,stored_binding_purpose,
           stored_binding_provider,stored_version_provider,stored_target_type,
           stored_state,stored_fingerprint,stored_artifact
      FROM secret_binding_versions v
      JOIN secret_bindings b ON b.id=v.binding_id
      WHERE v.id=NEW.version_id;
    IF NOT FOUND OR stored_binding_id<>NEW.binding_id OR
       stored_number<>NEW.version_number OR
       stored_binding_purpose<>'tls-certificate' OR
       stored_binding_provider<>'sealed-secrets' OR
       stored_version_provider<>'sealed-secrets' OR
       stored_target_type<>'kubernetes.io/tls' OR
       stored_state NOT IN ('awaiting-readiness','active','retained') OR
       NOT stored_artifact OR
       stored_fingerprint IS DISTINCT FROM NEW.secret_content_fingerprint THEN
        RAISE EXCEPTION 'certificate attestation does not match its sealed TLS version'
            USING ERRCODE='23514';
    END IF;
    SELECT actor_id INTO staging_actor
      FROM secret_binding_events
      WHERE binding_id=NEW.binding_id AND version_id=NEW.version_id
        AND kind='version-staging';
    IF NOT FOUND OR staging_actor IS DISTINCT FROM NEW.created_by THEN
        RAISE EXCEPTION 'certificate actor does not match the staging event'
            USING ERRCODE='23514';
    END IF;
    IF NEW.created_at IS DISTINCT FROM (
        SELECT created_at FROM secret_binding_versions WHERE id=NEW.version_id
    ) THEN
        RAISE EXCEPTION 'certificate creation time does not match its secret version'
            USING ERRCODE='23514';
    END IF;

    FOR item IN
        SELECT entry.value
        FROM jsonb_array_elements(NEW.dns_names) AS entry(value)
    LOOP
        IF jsonb_typeof(item)<>'string' THEN
            RAISE EXCEPTION 'certificate DNS names must be strings' USING ERRCODE='23514';
        END IF;
        value := item #>> '{}';
        IF value<>lower(value) OR value<>btrim(value) OR length(value)>253 OR
           NOT (
               value ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*$' OR
               value ~ '^\*\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?)*$'
           ) OR (previous<>'' AND value COLLATE "C"<=previous COLLATE "C") THEN
            RAISE EXCEPTION 'certificate DNS names are not canonical'
                USING ERRCODE='23514';
        END IF;
        previous := value;
    END LOOP;

    previous := '';
    FOR item IN
        SELECT entry.value
        FROM jsonb_array_elements(NEW.ip_addresses) AS entry(value)
    LOOP
        IF jsonb_typeof(item)<>'string' THEN
            RAISE EXCEPTION 'certificate IP addresses must be strings' USING ERRCODE='23514';
        END IF;
        value := item #>> '{}';
        BEGIN
            IF value<>btrim(value) OR value LIKE '::ffff:%' OR
               host(value::inet)<>value OR
               (previous<>'' AND value COLLATE "C"<=previous COLLATE "C") THEN
                RAISE EXCEPTION 'certificate IP addresses are not canonical'
                    USING ERRCODE='23514';
            END IF;
        EXCEPTION WHEN invalid_text_representation THEN
            RAISE EXCEPTION 'certificate IP address is invalid' USING ERRCODE='23514';
        END;
        previous := value;
    END LOOP;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_versions_validate ON tls_certificate_versions;
CREATE TRIGGER tls_certificate_versions_validate
    BEFORE INSERT ON tls_certificate_versions
    FOR EACH ROW EXECUTE FUNCTION validate_tls_certificate_version();

CREATE OR REPLACE FUNCTION protect_tls_certificate_version()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'certificate attestations are append-only' USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_versions_protect ON tls_certificate_versions;
CREATE TRIGGER tls_certificate_versions_protect
    BEFORE UPDATE OR DELETE ON tls_certificate_versions
    FOR EACH ROW EXECUTE FUNCTION protect_tls_certificate_version();

-- A one-time successful SealedSecret activation is not current serving
-- readiness. These rows continuously fence the exact active secret artifact,
-- public X.509 attestation, observer configuration and lease owner. Provider
-- material is never persisted here.
CREATE TABLE IF NOT EXISTS tls_certificate_observations (
    version_id uuid PRIMARY KEY,
    binding_id uuid NOT NULL,
    target_digest text NOT NULL CHECK (target_digest ~ '^sha256:[0-9a-f]{64}$'),
    observation_contract_version text NOT NULL DEFAULT '' CHECK (
        observation_contract_version='' OR
        observation_contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    observation_config_digest text NOT NULL DEFAULT '' CHECK (
        observation_config_digest='' OR
        observation_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    state text NOT NULL CHECK (state IN ('awaiting','ready','degraded','requeue')),
    next_observation_at timestamptz NOT NULL,
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 30),
    failure_code text NOT NULL DEFAULT '' CHECK (
        failure_code='' OR failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'
    ),
    last_observed_at timestamptz,
    last_ready_at timestamptz,
    lease_owner text CHECK (
        lease_owner IS NULL OR lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_claimed_at timestamptz,
    lease_until timestamptz,
    lease_contract_version text CHECK (
        lease_contract_version IS NULL OR
        lease_contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    lease_config_digest text CHECK (
        lease_config_digest IS NULL OR
        lease_config_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    lease_target_digest text CHECK (
        lease_target_digest IS NULL OR
        lease_target_digest ~ '^sha256:[0-9a-f]{64}$'
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (version_id,binding_id)
        REFERENCES tls_certificate_versions(version_id,binding_id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at),
    CHECK ((observation_contract_version='')=(observation_config_digest='')),
    CHECK (
        (state='awaiting' AND failure_code='' AND last_observed_at IS NULL AND last_ready_at IS NULL) OR
        (state='ready' AND failure_code='' AND last_observed_at IS NOT NULL AND last_ready_at=last_observed_at AND
         observation_contract_version<>'') OR
        (state='degraded' AND failure_code<>'' AND last_observed_at IS NOT NULL AND observation_contract_version<>'') OR
        (state='requeue' AND failure_code<>'' AND observation_contract_version<>'')
    ),
    CHECK (last_ready_at IS NULL OR last_observed_at IS NOT NULL AND last_ready_at<=last_observed_at),
    CHECK (
        (lease_owner IS NULL AND lease_claimed_at IS NULL AND lease_until IS NULL AND
         lease_contract_version IS NULL AND lease_config_digest IS NULL AND lease_target_digest IS NULL) OR
        (lease_owner IS NOT NULL AND lease_claimed_at IS NOT NULL AND lease_until>lease_claimed_at AND
         lease_contract_version IS NOT NULL AND lease_config_digest IS NOT NULL AND
         lease_target_digest=target_digest)
    )
);
CREATE INDEX IF NOT EXISTS tls_certificate_observations_claim_idx
    ON tls_certificate_observations(next_observation_at,version_id)
    WHERE lease_owner IS NULL;
CREATE INDEX IF NOT EXISTS tls_certificate_observations_binding_idx
    ON tls_certificate_observations(binding_id,version_id,state,last_ready_at);

CREATE OR REPLACE FUNCTION protect_tls_certificate_observation()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    old_payload jsonb;
    new_payload jsonb;
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'awaiting' OR NEW.observation_contract_version<>'' OR
           NEW.observation_config_digest<>'' OR NEW.consecutive_failures<>0 OR
           NEW.failure_code<>'' OR NEW.last_observed_at IS NOT NULL OR
           NEW.last_ready_at IS NOT NULL OR NEW.lease_epoch<>0 OR
           NEW.lease_owner IS NOT NULL OR NEW.created_at<>NEW.updated_at THEN
            RAISE EXCEPTION 'certificate observation must start pristine'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'certificate observations are retained'
            USING ERRCODE='23514';
    END IF;
    IF ROW(NEW.version_id,NEW.binding_id,NEW.target_digest,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.version_id,OLD.binding_id,OLD.target_digest,OLD.created_at) OR
       NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'certificate observation identity is immutable'
            USING ERRCODE='23514';
    END IF;
    old_payload := to_jsonb(OLD) - ARRAY['lease_owner','lease_epoch','lease_claimed_at','lease_until',
        'lease_contract_version','lease_config_digest','lease_target_digest','updated_at'];
    new_payload := to_jsonb(NEW) - ARRAY['lease_owner','lease_epoch','lease_claimed_at','lease_until',
        'lease_contract_version','lease_config_digest','lease_target_digest','updated_at'];

    -- Claim or reclaim. The observation payload remains byte-identical and a
    -- previous owner may be replaced only after its exact lease expired.
    IF NEW.lease_epoch=OLD.lease_epoch+1 AND NEW.lease_owner IS NOT NULL THEN
        IF old_payload IS DISTINCT FROM new_payload OR
           NOT (OLD.lease_owner IS NULL OR OLD.lease_until<=NEW.lease_claimed_at) OR
           NEW.lease_target_digest<>OLD.target_digest OR
           NEW.updated_at<>NEW.lease_claimed_at THEN
            RAISE EXCEPTION 'invalid certificate observation claim'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;

    -- Heartbeat. Every lease identity field remains exact and only the expiry
    -- and monotonic updated time may advance.
    IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
       NEW.lease_owner=OLD.lease_owner AND NEW.lease_claimed_at=OLD.lease_claimed_at AND
       NEW.lease_contract_version=OLD.lease_contract_version AND
       NEW.lease_config_digest=OLD.lease_config_digest AND
       NEW.lease_target_digest=OLD.lease_target_digest AND
       NEW.lease_until>OLD.lease_until AND old_payload IS NOT DISTINCT FROM new_payload THEN
        RETURN NEW;
    END IF;

    -- Completion/requeue. Only the exact live lease can clear itself, and the
    -- published readiness identity must be the one that held that lease.
    IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
       NEW.lease_owner IS NULL AND NEW.lease_claimed_at IS NULL AND NEW.lease_until IS NULL AND
       NEW.lease_contract_version IS NULL AND NEW.lease_config_digest IS NULL AND
       NEW.lease_target_digest IS NULL AND NEW.updated_at<OLD.lease_until AND
       NEW.observation_contract_version=OLD.lease_contract_version AND
       NEW.observation_config_digest=OLD.lease_config_digest THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invalid certificate observation transition'
        USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_observations_protect ON tls_certificate_observations;
CREATE TRIGGER tls_certificate_observations_protect
    BEFORE INSERT OR UPDATE OR DELETE ON tls_certificate_observations
    FOR EACH ROW EXECUTE FUNCTION protect_tls_certificate_observation();

CREATE TABLE IF NOT EXISTS tls_certificate_observation_workers (
    worker_id text PRIMARY KEY CHECK (
        worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    contract_version text NOT NULL CHECK (
        contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'
    ),
    config_digest text NOT NULL CHECK (config_digest ~ '^sha256:[0-9a-f]{64}$'),
    started_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL CHECK (observed_at>=started_at),
    lease_until timestamptz NOT NULL CHECK (lease_until>observed_at),
    updated_at timestamptz NOT NULL CHECK (updated_at=observed_at)
);

CREATE OR REPLACE FUNCTION protect_tls_certificate_observation_worker()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'certificate observation worker receipts are retained'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        RETURN NEW;
    END IF;
    IF NEW.worker_id<>OLD.worker_id THEN
        RAISE EXCEPTION 'certificate observation worker identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch+1 AND NEW.started_at>=OLD.observed_at THEN
        RETURN NEW;
    END IF;
    IF NEW.worker_epoch=OLD.worker_epoch AND NEW.contract_version=OLD.contract_version AND
       NEW.config_digest=OLD.config_digest AND NEW.started_at=OLD.started_at AND
       NEW.observed_at>=OLD.observed_at AND NEW.observed_at<OLD.lease_until AND
       NEW.lease_until>OLD.lease_until THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invalid certificate observation worker transition'
        USING ERRCODE='23514';
END;
$$;
DROP TRIGGER IF EXISTS tls_certificate_observation_workers_protect ON tls_certificate_observation_workers;
CREATE TRIGGER tls_certificate_observation_workers_protect
    BEFORE UPDATE OR DELETE ON tls_certificate_observation_workers
    FOR EACH ROW EXECUTE FUNCTION protect_tls_certificate_observation_worker();
