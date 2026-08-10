-- Metadata-only external-dns integration profiles. Provider credentials,
-- Secret data, API endpoints, arbitrary provider JSON, webhook configuration,
-- controller observations and rendered Kubernetes objects are deliberately
-- absent. Platform operators bind profiles to exact central environments.

CREATE OR REPLACE FUNCTION external_dns_domain_suffixes_valid(value jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    IF jsonb_typeof(value) <> 'array' OR jsonb_array_length(value) NOT BETWEEN 1 AND 64 THEN
        RETURN false;
    END IF;
    RETURN NOT EXISTS (
        SELECT 1
        FROM jsonb_array_elements_text(value) AS suffix(value)
        WHERE length(suffix.value) NOT BETWEEN 1 AND 253
           OR suffix.value <> lower(suffix.value)
           OR suffix.value ~ '[[:cntrl:]]'
           OR suffix.value !~ '^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$'
    ) AND (
        SELECT count(*) = count(DISTINCT suffix.value)
        FROM jsonb_array_elements_text(value) AS suffix(value)
    );
END;
$$;

CREATE TABLE IF NOT EXISTS external_dns_integrations (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (
        length(slug) BETWEEN 1 AND 63 AND
        slug ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'
    ),
    name text NOT NULL CHECK (
        length(name) BETWEEN 1 AND 100 AND
        name=btrim(name) AND name !~ '[[:cntrl:]]'
    ),
    mode text NOT NULL CHECK (mode IN ('managed','adopted')),
    provider_kind text NOT NULL CHECK (
        provider_kind IN ('aws','azure','cloudflare','google','rfc2136')
    ),
    txt_owner_id text NOT NULL UNIQUE CHECK (
        length(txt_owner_id) BETWEEN 1 AND 128 AND
        txt_owner_id ~ '^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$'
    ),
    allowed_domain_suffixes jsonb NOT NULL
        CHECK (external_dns_domain_suffixes_valid(allowed_domain_suffixes)),
    sync_policy text NOT NULL DEFAULT 'upsert-only'
        CHECK (sync_policy IN ('upsert-only','sync')),
    destructive_sync_confirmed boolean NOT NULL DEFAULT false,
    credential_secret_ref text,
    provider_config_ref text,
    egress_config_ref text,
    operator_profile_ref text,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK (updated_at >= created_at),
    CHECK (
        (sync_policy='upsert-only' AND NOT destructive_sync_confirmed) OR
        (sync_policy='sync' AND destructive_sync_confirmed)
    ),
    CHECK (
        (mode='managed' AND
         credential_secret_ref IS NOT NULL AND
         provider_config_ref IS NOT NULL AND
         egress_config_ref IS NOT NULL AND
         credential_secret_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         provider_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         egress_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         operator_profile_ref IS NULL) OR
        (mode='adopted' AND operator_profile_ref IS NOT NULL AND
         credential_secret_ref IS NULL AND
         provider_config_ref IS NULL AND egress_config_ref IS NULL AND
         operator_profile_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$')
    )
);

CREATE TABLE IF NOT EXISTS external_dns_integration_environments (
    integration_id uuid NOT NULL
        REFERENCES external_dns_integrations(id) ON DELETE CASCADE,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (integration_id,environment_id)
);
CREATE INDEX IF NOT EXISTS external_dns_integration_environments_environment_idx
    ON external_dns_integration_environments(environment_id,integration_id);

CREATE OR REPLACE FUNCTION protect_external_dns_integration_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.slug,NEW.txt_owner_id,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.slug,OLD.txt_owner_id,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'external-dns integration identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'external-dns integration time cannot move backwards' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS external_dns_integrations_identity ON external_dns_integrations;
CREATE TRIGGER external_dns_integrations_identity
    BEFORE UPDATE ON external_dns_integrations
    FOR EACH ROW EXECUTE FUNCTION protect_external_dns_integration_identity();
