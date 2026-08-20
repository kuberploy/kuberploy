-- A managed runtime bundle can change when Kuberploy is upgraded even though
-- the administrator's ExternalDNS input is unchanged. Permit one exact runtime
-- revision advance only when the trusted reconciler resets the current
-- materialized protected-Git receipt to pending for republication.
LOCK TABLE public.external_dns_integrations IN SHARE ROW EXCLUSIVE MODE;

CREATE OR REPLACE FUNCTION public.protect_external_dns_integration_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    desired_changed boolean;
    runtime_republish boolean;
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
    runtime_republish := NOT desired_changed AND OLD.lifecycle='active' AND NEW.lifecycle='active' AND
        OLD.protected_git_state='materialized' AND OLD.protected_git_revision=OLD.runtime_revision AND
        OLD.protected_git_content_digest ~ '^sha256:[0-9a-f]{64}$' AND OLD.protected_git_commit<>'' AND
        OLD.protected_git_observed_at IS NOT NULL AND NEW.protected_git_state='pending' AND
        NEW.protected_git_revision IS NULL AND NEW.protected_git_content_digest='' AND
        NEW.protected_git_commit='' AND NEW.protected_git_observed_at IS NULL;
    IF (desired_changed OR runtime_republish) AND NEW.runtime_revision <> OLD.runtime_revision + 1 OR
       NOT (desired_changed OR runtime_republish) AND NEW.runtime_revision <> OLD.runtime_revision THEN
        RAISE EXCEPTION 'external-dns runtime revision is not an exact desired-state revision' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

ALTER FUNCTION public.protect_external_dns_integration_identity()
SET search_path=pg_catalog,pg_temp;

DO $migration$
DECLARE
    definition text;
    settings text[];
BEGIN
    SELECT pg_catalog.pg_get_functiondef(procedure.oid),procedure.proconfig
      INTO definition,settings
      FROM pg_catalog.pg_proc procedure
     WHERE procedure.oid='public.protect_external_dns_integration_identity()'::pg_catalog.regprocedure;
    IF settings IS DISTINCT FROM ARRAY['search_path=pg_catalog, pg_temp']::text[] OR
       pg_catalog.strpos(definition,'runtime_republish boolean')=0 OR
       pg_catalog.strpos(definition,'protected_git_state')=0 OR
       pg_catalog.strpos(definition,'runtime_revision')=0 THEN
        RAISE EXCEPTION 'external-dns runtime republish authority migration verification failed';
    END IF;
END;
$migration$;
