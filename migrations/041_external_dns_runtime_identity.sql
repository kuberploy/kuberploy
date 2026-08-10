-- ExternalDNS readiness must attest the provider and managed reference
-- identities recorded by the platform integration. No Secret values or
-- provider configuration payloads are persisted or read here.
ALTER TABLE edge_runtime_targets
    ADD COLUMN external_provider_kind text NOT NULL DEFAULT '',
    ADD COLUMN external_credential_secret_ref text NOT NULL DEFAULT '',
    ADD COLUMN external_provider_config_ref text NOT NULL DEFAULT '',
    ADD COLUMN external_egress_config_ref text NOT NULL DEFAULT '';

-- Observations made under the previous, weaker identity contract cannot stay
-- active. Preserve their audit receipts, but require a new profile revision
-- whose digest covers the added fields before readiness can recover.
UPDATE edge_runtime_targets target
   SET external_provider_kind=integration.provider_kind,
       external_credential_secret_ref=COALESCE(integration.credential_secret_ref,''),
       external_provider_config_ref=COALESCE(integration.provider_config_ref,''),
       external_egress_config_ref=COALESCE(integration.egress_config_ref,''),
       active=false,runtime_state='awaiting',lease_owner=NULL,lease_until=NULL,
       worker_contract=NULL,worker_config_digest=NULL,updated_at=GREATEST(target.updated_at,now())
  FROM external_dns_integrations integration
 WHERE target.kind='external-dns' AND target.integration_id=integration.id;

ALTER TABLE edge_runtime_targets ADD CONSTRAINT edge_runtime_external_dns_identity_v2 CHECK (
    (kind<>'external-dns' AND external_provider_kind='' AND
     external_credential_secret_ref='' AND external_provider_config_ref='' AND external_egress_config_ref='') OR
    (kind='external-dns' AND external_provider_kind IN ('aws','azure','cloudflare','google','rfc2136') AND (
        (management_mode='managed' AND
         external_credential_secret_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         external_provider_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$' AND
         external_egress_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$') OR
        (management_mode='adopted' AND external_credential_secret_ref='' AND
         external_provider_config_ref='' AND external_egress_config_ref='')
    ))
);

CREATE OR REPLACE FUNCTION validate_edge_runtime_target()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    durable_mode text;
    durable_provider_kind text;
    durable_txt_owner text;
    durable_policy text;
    durable_credential_ref text;
    durable_provider_ref text;
    durable_egress_ref text;
    durable_profile text;
    durable_domains text;
BEGIN
    IF NEW.kind='external-dns' AND NEW.active THEN
        SELECT i.mode,i.provider_kind,i.txt_owner_id,i.sync_policy,
               COALESCE(i.credential_secret_ref,''),COALESCE(i.provider_config_ref,''),
               COALESCE(i.egress_config_ref,''),COALESCE(i.operator_profile_ref,''),
               COALESCE((
                   SELECT string_agg(suffix.value,',' ORDER BY suffix.value)
                     FROM jsonb_array_elements_text(i.allowed_domain_suffixes) AS suffix(value)
               ),'')
          INTO durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,
               durable_credential_ref,durable_provider_ref,durable_egress_ref,
               durable_profile,durable_domains
          FROM external_dns_integrations i
         WHERE i.id=NEW.integration_id;
        IF NOT FOUND OR
           ROW(NEW.management_mode,NEW.external_provider_kind,NEW.external_txt_owner_id,
               NEW.external_policy,NEW.external_domains)
           IS DISTINCT FROM
           ROW(durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,durable_domains) OR
           (NEW.management_mode='adopted' AND
            (NEW.profile_config_map<>durable_profile OR durable_credential_ref<>'' OR
             durable_provider_ref<>'' OR durable_egress_ref<>'')) OR
           (NEW.management_mode='managed' AND
            (durable_profile<>'' OR
             ROW(NEW.external_credential_secret_ref,NEW.external_provider_config_ref,NEW.external_egress_config_ref)
             IS DISTINCT FROM ROW(durable_credential_ref,durable_provider_ref,durable_egress_ref))) THEN
            RAISE EXCEPTION 'External DNS edge target does not match its safe integration metadata'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.kind,NEW.integration_id,
               NEW.management_mode,NEW.namespace,NEW.profile_config_map,
               NEW.external_txt_owner_id,NEW.external_policy,NEW.external_domains,
               NEW.external_provider_kind,NEW.external_credential_secret_ref,
               NEW.external_provider_config_ref,NEW.external_egress_config_ref,
               NEW.desired_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.kind,OLD.integration_id,
               OLD.management_mode,OLD.namespace,OLD.profile_config_map,
               OLD.external_txt_owner_id,OLD.external_policy,OLD.external_domains,
               OLD.external_provider_kind,OLD.external_credential_secret_ref,
               OLD.external_provider_config_ref,OLD.external_egress_config_ref,
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
