-- Make ExternalDNS integrations operational, revisioned desired state. The
-- credential columns continue to store Kubernetes object names only; Secret
-- values and provider payloads never enter Kuberploy's database.
ALTER TABLE external_dns_integrations
    ADD COLUMN runtime_revision bigint NOT NULL DEFAULT 1 CHECK (runtime_revision > 0),
    ADD COLUMN lifecycle text NOT NULL DEFAULT 'active'
        CHECK (lifecycle IN ('active','deactivated')),
    ADD COLUMN deactivated_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN deactivated_at timestamptz,
    ADD COLUMN protected_git_state text NOT NULL DEFAULT 'pending'
        CHECK (protected_git_state IN ('pending','materialized','dematerialized')),
    ADD COLUMN protected_git_revision bigint,
    ADD COLUMN protected_git_content_digest text NOT NULL DEFAULT '',
    ADD COLUMN protected_git_commit text NOT NULL DEFAULT '',
    ADD COLUMN protected_git_observed_at timestamptz,
    ADD CONSTRAINT external_dns_lifecycle_consistent CHECK (
        (lifecycle='active' AND deactivated_by IS NULL AND deactivated_at IS NULL) OR
        (lifecycle='deactivated' AND deactivated_by IS NOT NULL AND deactivated_at IS NOT NULL)
    ),
    ADD CONSTRAINT external_dns_protected_git_receipt CHECK (
      (protected_git_state='pending' AND protected_git_revision IS NULL AND
       protected_git_content_digest='' AND protected_git_commit='' AND protected_git_observed_at IS NULL) OR
      (protected_git_state='materialized' AND protected_git_revision=runtime_revision AND
       protected_git_content_digest ~ '^sha256:[0-9a-f]{64}$' AND
       protected_git_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$' AND protected_git_observed_at IS NOT NULL) OR
      (protected_git_state='dematerialized' AND lifecycle='deactivated' AND
       protected_git_revision=runtime_revision AND protected_git_content_digest='' AND
       protected_git_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$' AND protected_git_observed_at IS NOT NULL)
    );

CREATE INDEX external_dns_integrations_active_runtime_idx
    ON external_dns_integrations(runtime_revision,id) WHERE lifecycle='active';

CREATE OR REPLACE FUNCTION protect_external_dns_integration_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    desired_changed boolean;
BEGIN
    IF ROW(NEW.id,NEW.slug,NEW.txt_owner_id,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.slug,OLD.txt_owner_id,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'external-dns integration identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at OR
       (OLD.deactivated_at IS NOT NULL AND NEW.deactivated_at IS DISTINCT FROM OLD.deactivated_at) OR
       (OLD.lifecycle='deactivated' AND NEW.lifecycle<>'deactivated') THEN
        RAISE EXCEPTION 'external-dns integration lifecycle cannot regress' USING ERRCODE='23514';
    END IF;
    desired_changed := ROW(NEW.name,NEW.mode,NEW.provider_kind,NEW.allowed_domain_suffixes,
        NEW.sync_policy,NEW.destructive_sync_confirmed,NEW.credential_secret_ref,
        NEW.provider_config_ref,NEW.egress_config_ref,NEW.operator_profile_ref)
      IS DISTINCT FROM ROW(OLD.name,OLD.mode,OLD.provider_kind,OLD.allowed_domain_suffixes,
        OLD.sync_policy,OLD.destructive_sync_confirmed,OLD.credential_secret_ref,
        OLD.provider_config_ref,OLD.egress_config_ref,OLD.operator_profile_ref);
    IF desired_changed AND NEW.runtime_revision <> OLD.runtime_revision + 1 OR
       NOT desired_changed AND NEW.runtime_revision <> OLD.runtime_revision THEN
        RAISE EXCEPTION 'external-dns runtime revision is not an exact desired-state revision' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

-- An active runtime target must attest the current durable revision. A UI
-- update therefore makes the old observation ineligible immediately.
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
    durable_revision bigint;
    durable_lifecycle text;
BEGIN
    IF NEW.kind='external-dns' AND NEW.active THEN
        SELECT i.mode,i.provider_kind,i.txt_owner_id,i.sync_policy,
               COALESCE(i.credential_secret_ref,''),COALESCE(i.provider_config_ref,''),
               COALESCE(i.egress_config_ref,''),COALESCE(i.operator_profile_ref,''),
               COALESCE((SELECT string_agg(suffix.value,',' ORDER BY suffix.value)
                 FROM jsonb_array_elements_text(i.allowed_domain_suffixes) AS suffix(value)),''),
               i.runtime_revision,i.lifecycle
          INTO durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,
               durable_credential_ref,durable_provider_ref,durable_egress_ref,
               durable_profile,durable_domains,durable_revision,durable_lifecycle
          FROM external_dns_integrations i WHERE i.id=NEW.integration_id;
        IF NOT FOUND OR durable_lifecycle<>'active' OR NEW.profile_revision<>durable_revision OR
           ROW(NEW.management_mode,NEW.external_provider_kind,NEW.external_txt_owner_id,
               NEW.external_policy,NEW.external_domains)
           IS DISTINCT FROM ROW(durable_mode,durable_provider_kind,durable_txt_owner,durable_policy,durable_domains) OR
           (NEW.management_mode='adopted' AND
            (NEW.profile_config_map<>durable_profile OR durable_credential_ref<>'' OR
             durable_provider_ref<>'' OR durable_egress_ref<>'')) OR
           (NEW.management_mode='managed' AND
            (durable_profile<>'' OR ROW(NEW.external_credential_secret_ref,
             NEW.external_provider_config_ref,NEW.external_egress_config_ref)
             IS DISTINCT FROM ROW(durable_credential_ref,durable_provider_ref,durable_egress_ref))) THEN
            RAISE EXCEPTION 'External DNS edge target does not match its current safe integration revision'
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
            RAISE EXCEPTION 'Edge runtime target identity is immutable' USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Edge runtime target lease epoch is invalid' USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest) IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Edge runtime target lease identity changed without a new epoch' USING ERRCODE='23514';
        END IF;
        IF OLD.observed_identity_digest<>'' AND NEW.observed_identity_digest<>OLD.observed_identity_digest THEN
            RAISE EXCEPTION 'Edge runtime observed Kubernetes identity is immutable' USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR (OLD.last_observed_at IS NOT NULL AND
           (NEW.last_observed_at IS NULL OR NEW.last_observed_at<OLD.last_observed_at)) THEN
            RAISE EXCEPTION 'Edge runtime target time cannot regress' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
