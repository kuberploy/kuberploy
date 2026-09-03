DO $kuberploy_baseline$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_catalog.pg_tables
        WHERE schemaname = current_schema()
          AND tablename = 'schema_migrations'
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'pre-Prisma release-candidate databases require a fresh PostgreSQL database';
    END IF;
END
$kuberploy_baseline$;

--
-- PostgreSQL database dump
--


-- Dumped from database version 18.6 (Debian 18.6-1.pgdg13+2)
-- Dumped by pg_dump version 18.6 (Debian 18.6-1.pgdg13+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: enforce_application_registry_pull_selection_scope(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_application_registry_pull_selection_scope() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    app_project uuid;
    credential_project uuid;
BEGIN
    app_project := NEW.project_id;
    IF NEW.registry_pull_mode='project-credential' THEN
        SELECT project_id INTO STRICT credential_project
          FROM project_registry_pull_credentials WHERE id=NEW.registry_pull_project_credential_id;
        IF app_project <> credential_project THEN
            RAISE EXCEPTION 'registry pull credential belongs to another project' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enforce_project_registry_pull_target(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_project_registry_pull_target() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    pull_ref text;
BEGIN
    SELECT pull_credential_ref INTO STRICT pull_ref FROM registry_targets WHERE id=NEW.registry_target_id;
    IF pull_ref='' THEN
        RAISE EXCEPTION 'registry target has no pull credential' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enforce_protected_deployment_desired_revision(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_protected_deployment_desired_revision() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
  authoritative_revision text;
BEGIN
  IF NEW.state = 'git-committed' AND NEW.operation_id IS NOT NULL THEN
    SELECT document.config_revision
      INTO authoritative_revision
    FROM git_write_commands AS command
    JOIN git_pull_request_publications AS publication
      ON publication.operation_id = command.operation_id
     AND publication.binding_id = command.binding_id
     AND publication.target_ref = command.target_ref
     AND publication.state = 'merge-verified'
    JOIN git_projected_documents AS document
      ON document.binding_id = command.binding_id
     AND document.generation = command.indexed_generation
     AND document.path = command.path
     AND document.valid
     AND document.content_sha256 = command.content_sha256
     AND document.raw = command.content
    JOIN operations AS operation
      ON operation.id = command.operation_id
    WHERE command.command_kind = 'deployment'
      AND command.publication_mode = 'pull-request'
      AND command.state = 'indexed'
      AND command.indexed_generation > 0
      AND command.deployment_id = NEW.id
      AND NEW.operation_id = command.operation_id
      AND NEW.generation = operation.generation;

    IF authoritative_revision IS NOT NULL THEN
      NEW.desired_revision := authoritative_revision;
    END IF;
  END IF;
  RETURN NEW;
END;
$$;


--
-- Name: enforce_secret_binding_scope(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_secret_binding_scope() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    durable_organization_id uuid;
BEGIN
    SELECT team_id INTO durable_organization_id
      FROM public.projects
      WHERE id=NEW.project_id
      FOR KEY SHARE;
    IF NOT FOUND OR NEW.organization_id IS DISTINCT FROM durable_organization_id THEN
        RAISE EXCEPTION 'Secret binding organization does not match project ownership'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enforce_secret_binding_version_target(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_secret_binding_version_target() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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


--
-- Name: enqueue_auto_deploy_runs(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enqueue_auto_deploy_runs() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state<>'succeeded' OR NEW.release_id IS NULL OR
       (TG_OP='UPDATE' AND OLD.state='succeeded') THEN RETURN NEW; END IF;
    INSERT INTO auto_deploy_runs(attempt_id,policy_id,policy_revision,definition_id,definition_digest,release_id,
        template_digest,source_deployment_id,source_deployment_generation,source_config_etag,idempotency_key,available_at,created_at,updated_at)
    SELECT a.id,p.id,p.current_revision,a.definition_id,a.definition_digest,NEW.release_id,r.template_digest,
        r.source_deployment_id,r.source_deployment_generation,r.source_config_etag,
        'auto-deploy/'||p.id::text||'/'||p.current_revision::text||'/'||a.id::text,
        NEW.completed_at,NEW.completed_at,NEW.completed_at
    FROM build_attempts a
    JOIN applications app ON app.id=a.service_id
      AND app.build_source_id=a.definition_id
      AND app.build_source_digest=a.definition_digest
    JOIN auto_deploy_policies p ON p.application_id=a.service_id
    JOIN auto_deploy_policy_revisions r ON r.policy_id=p.id AND r.revision=p.current_revision AND r.enabled
    WHERE a.id=NEW.attempt_id
    ON CONFLICT(attempt_id,policy_id) DO NOTHING;
    RETURN NEW;
END; $$;


--
-- Name: enqueue_build_release_projection(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enqueue_build_release_projection() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.state <> 'succeeded' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.state = 'succeeded' THEN
        RETURN NEW;
    END IF;
    INSERT INTO build_release_projections(attempt_id,available_at,created_at,updated_at)
    VALUES(NEW.id,NEW.completed_at,NEW.completed_at,NEW.completed_at)
    ON CONFLICT(attempt_id) DO NOTHING;
    RETURN NEW;
END;
$$;


--
-- Name: external_dns_domain_suffixes_valid(jsonb); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.external_dns_domain_suffixes_valid(value jsonb) RETURNS boolean
    LANGUAGE plpgsql IMMUTABLE
    AS $_$
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
$_$;


--
-- Name: fence_legacy_argo_desired_state_recovery(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.fence_legacy_argo_desired_state_recovery() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.policy_digest IS DISTINCT FROM OLD.policy_digest THEN
        RAISE EXCEPTION 'Argo desired-state policy digest is immutable'
            USING ERRCODE='23514';
    END IF;
    IF OLD.policy_digest IS NULL AND OLD.write_base_revision<>'' AND
       OLD.state IN ('pending','claimed','git-committed') THEN
        IF NEW.write_base_revision='' OR
           NEW.write_base_revision IS DISTINCT FROM OLD.write_base_revision OR
           NEW.write_base_observed_at IS DISTINCT FROM OLD.write_base_observed_at OR
           (OLD.state='pending' AND NEW.state NOT IN ('pending','claimed','failed')) OR
           (OLD.state='claimed' AND NEW.state NOT IN ('pending','claimed','git-committed','failed')) OR
           (OLD.state='git-committed' AND NEW.state NOT IN ('git-committed','verified')) THEN
            RAISE EXCEPTION 'legacy Argo desired-state recovery cannot regress publication authority'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_auto_deploy_policy(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_auto_deploy_policy() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF NEW.id<>OLD.id OR NEW.project_id<>OLD.project_id OR NEW.application_id<>OLD.application_id OR
       NEW.environment_id<>OLD.environment_id OR NEW.created_by<>OLD.created_by OR
       NEW.created_at<>OLD.created_at OR NEW.current_revision<>OLD.current_revision+1 THEN
        RAISE EXCEPTION 'invalid auto-deploy policy transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;


--
-- Name: protect_auto_deploy_run(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_auto_deploy_run() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
	IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending' OR NEW.attempts<>0 OR NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR
           NEW.operation_id IS NOT NULL OR NEW.deployment_id IS NOT NULL OR NEW.failure_code<>'' OR NEW.completed_at IS NOT NULL OR
           NEW.updated_at<>NEW.created_at OR NEW.available_at<NEW.created_at THEN
            RAISE EXCEPTION 'auto-deploy run must be inserted pristine and pending' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.attempt_id<>OLD.attempt_id OR NEW.policy_id<>OLD.policy_id OR NEW.policy_revision<>OLD.policy_revision OR
       NEW.definition_id<>OLD.definition_id OR NEW.definition_digest<>OLD.definition_digest OR NEW.release_id<>OLD.release_id OR
       NEW.template_digest<>OLD.template_digest OR NEW.source_deployment_id<>OLD.source_deployment_id OR
       NEW.source_deployment_generation<>OLD.source_deployment_generation OR NEW.source_config_etag<>OLD.source_config_etag OR
       NEW.idempotency_key<>OLD.idempotency_key OR NEW.created_at<>OLD.created_at OR NEW.updated_at<OLD.updated_at OR
       NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'auto-deploy run immutable identity changed' USING ERRCODE='23514';
    END IF;
    IF OLD.state='pending' AND NEW.state='processing' THEN
        IF NEW.attempts<>OLD.attempts+1 OR NEW.lease_epoch<>OLD.lease_epoch+1 OR NEW.lease_owner IS NULL OR
           NEW.available_at<>OLD.available_at OR NEW.failure_code<>'' OR NEW.completed_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid auto-deploy run acquisition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state='processing' THEN
		IF OLD.lease_until>NEW.updated_at AND NEW.attempts=OLD.attempts AND NEW.lease_epoch=OLD.lease_epoch AND NEW.lease_owner=OLD.lease_owner AND NEW.lease_until>OLD.lease_until AND
		   NEW.available_at=OLD.available_at AND NEW.failure_code=OLD.failure_code AND NEW.completed_at IS NOT DISTINCT FROM OLD.completed_at AND
		   NEW.operation_id IS NOT DISTINCT FROM OLD.operation_id AND NEW.deployment_id IS NOT DISTINCT FROM OLD.deployment_id THEN
            NULL; -- heartbeat
		ELSIF OLD.lease_until<=NEW.updated_at AND NEW.attempts=OLD.attempts+1 AND NEW.lease_epoch=OLD.lease_epoch+1 AND NEW.lease_owner IS NOT NULL AND
		   NEW.available_at=OLD.available_at AND NEW.failure_code='' AND NEW.completed_at IS NULL AND NEW.operation_id IS NULL AND NEW.deployment_id IS NULL THEN
            NULL; -- expired-lease recovery
        ELSE RAISE EXCEPTION 'invalid auto-deploy processing transition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state='pending' THEN
		IF OLD.lease_until<=NEW.updated_at OR NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR NEW.lease_owner IS NOT NULL OR
           NEW.available_at<NEW.updated_at OR NEW.failure_code='' OR NEW.completed_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid auto-deploy retry transition' USING ERRCODE='23514'; END IF;
    ELSIF OLD.state='processing' AND NEW.state IN ('submitted','failed') THEN
		IF OLD.lease_until<=NEW.updated_at OR NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR NEW.lease_owner IS NOT NULL OR NEW.completed_at<>NEW.updated_at THEN
            RAISE EXCEPTION 'invalid auto-deploy completion transition' USING ERRCODE='23514'; END IF;
    ELSE
        RAISE EXCEPTION 'invalid auto-deploy run state transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;


--
-- Name: protect_configuration_assigned_scope(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_configuration_assigned_scope() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE scope_kind text := TG_ARGV[0];
BEGIN
    IF EXISTS (SELECT 1 FROM configuration_profile_assignments WHERE scope_type=scope_kind AND scope_id=OLD.id) THEN
        RAISE EXCEPTION 'assigned configuration profile scope cannot be deleted' USING ERRCODE='23503';
    END IF;
    RETURN OLD;
END;
$$;


--
-- Name: protect_configuration_profile(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_configuration_profile() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF ROW(NEW.id,NEW.kind,NEW.name,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id,OLD.kind,OLD.name,OLD.created_by,OLD.created_at) OR
       OLD.lifecycle='deactivated' OR
       NOT ((NEW.lifecycle='active' AND NEW.current_revision IN (OLD.current_revision,OLD.current_revision+1)
             AND NEW.deactivated_by IS NULL AND NEW.deactivated_at IS NULL) OR
            (OLD.lifecycle='active' AND NEW.lifecycle='deactivated' AND NEW.current_revision=OLD.current_revision
             AND NEW.deactivated_by IS NOT NULL AND NEW.deactivated_at IS NOT NULL)) THEN
        RAISE EXCEPTION 'invalid configuration profile transition' USING ERRCODE='23514';
    END IF;
    IF NEW.kind='certificate-issuer' AND NEW.lifecycle='deactivated' AND
       EXISTS (SELECT 1 FROM cert_manager_issuer_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced certificate issuer profile cannot be deactivated' USING ERRCODE='23503';
    END IF;
    IF NEW.kind='middleware' AND NEW.lifecycle='deactivated' AND
       EXISTS (SELECT 1 FROM middleware_profile_references WHERE profile_id=OLD.id) THEN
        RAISE EXCEPTION 'referenced middleware profile cannot be deactivated' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_edge_sslip_ingress_observation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_edge_sslip_ingress_observation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target edge_runtime_targets%ROWTYPE;
BEGIN
    IF TG_OP='DELETE' THEN
        SELECT * INTO target
          FROM edge_runtime_targets
         WHERE target_key=OLD.target_key AND profile_revision=OLD.profile_revision
         FOR SHARE;
        IF FOUND AND target.active THEN
            RAISE EXCEPTION 'an active sslip ingress observation cannot be deleted'
                USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;

    SELECT * INTO target
      FROM edge_runtime_targets
     WHERE target_key=NEW.target_key AND profile_revision=NEW.profile_revision
     FOR SHARE;
    IF NOT FOUND OR NOT target.active OR target.kind<>'traefik' OR
       target.desired_digest<>NEW.desired_digest OR
       target.runtime_config_digest<>NEW.runtime_config_digest OR
       target.lease_owner<>NEW.worker_id OR target.lease_epoch<>NEW.lease_epoch OR
       target.worker_contract<>'edge-observer.v1' OR
       target.worker_config_digest<>NEW.runtime_config_digest OR
       target.lease_until IS NULL OR target.lease_until<=NEW.observed_at THEN
        RAISE EXCEPTION 'sslip observation is not fenced by the exact live Traefik lease'
            USING ERRCODE='23514';
    END IF;

    IF TG_OP='INSERT' THEN
        IF NEW.created_at<>NEW.observed_at OR NEW.updated_at<>NEW.observed_at THEN
            RAISE EXCEPTION 'sslip observation creation receipt is not pristine'
                USING ERRCODE='23514';
        END IF;
    ELSE
        IF ROW(NEW.target_key,NEW.profile_revision,NEW.desired_digest,
               NEW.public_ipv4,NEW.source_kind,NEW.service_uid,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.target_key,OLD.profile_revision,OLD.desired_digest,
               OLD.public_ipv4,OLD.source_kind,OLD.service_uid,OLD.created_at) OR
           NEW.lease_epoch<=OLD.lease_epoch OR NEW.observed_at<OLD.observed_at THEN
            RAISE EXCEPTION 'sslip endpoint identity is immutable or observation time regressed'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_environment_foundation_intent(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_environment_foundation_intent() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF ROW(NEW.id,NEW.environment_id,NEW.project_id,NEW.namespace,NEW.argo_project,
           NEW.platform_binding_id,NEW.target_ref,NEW.planned_head_revision,
           NEW.binding_generation,NEW.profile_digest,NEW.publisher_config_digest,
           NEW.publisher_contract,NEW.publisher_policy,
           NEW.manifest_path,NEW.manifest,NEW.manifest_digest,NEW.intent_digest,
           NEW.commit_trailer,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.environment_id,OLD.project_id,OLD.namespace,OLD.argo_project,
           OLD.platform_binding_id,OLD.target_ref,OLD.planned_head_revision,
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


--
-- Name: protect_environment_protection_policy(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_environment_protection_policy() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.protection_policy IS DISTINCT FROM OLD.protection_policy THEN
        RAISE EXCEPTION 'environment protection policy is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_external_dns_integration_identity(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_external_dns_integration_identity() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $_$
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
$_$;


--
-- Name: protect_git_binding_identity(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_git_binding_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF ROW(NEW.kind,NEW.scope_id,NEW.project_id,NEW.environment_id,
           NEW.provider,NEW.installation_id,NEW.repository_id,NEW.repository_owner,
           NEW.repository_name,NEW.target_ref,NEW.path_prefix,NEW.credential_mode,
           NEW.credential_secret_name)
       IS DISTINCT FROM
       ROW(OLD.kind,OLD.scope_id,OLD.project_id,OLD.environment_id,
           OLD.provider,OLD.installation_id,OLD.repository_id,OLD.repository_owner,
           OLD.repository_name,OLD.target_ref,OLD.path_prefix,OLD.credential_mode,
           OLD.credential_secret_name) THEN
        RAISE EXCEPTION 'Git binding identity is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_git_pull_request_publication(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_git_pull_request_publication() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending-candidate' OR NEW.write_base_revision<>'' OR NEW.candidate_revision<>'' OR
           NEW.pull_request_number<>0 OR NEW.pull_request_url<>'' OR NEW.pull_request_state<>'' OR NEW.merge_revision<>'' OR
           NEW.target_revision<>'' OR NEW.provider_observed_at IS NOT NULL OR NEW.version<>1 OR NEW.updated_at<>NEW.created_at THEN
            RAISE EXCEPTION 'pull request publication must start pristine pending-candidate' USING ERRCODE='23514';
        END IF;
        IF (SELECT count(*) FROM git_write_commands c JOIN git_repository_bindings b ON b.id=c.binding_id
            WHERE c.operation_id=NEW.operation_id AND c.publication_mode='pull-request' AND c.binding_id=NEW.binding_id
              AND c.target_ref=NEW.target_ref AND c.base_revision=NEW.base_revision AND b.provider=NEW.provider
              AND b.installation_id=NEW.installation_id AND b.repository_id=NEW.repository_id
              AND b.repository_owner=NEW.repository_owner AND b.repository_name=NEW.repository_name) <> 1 THEN
            RAISE EXCEPTION 'pull request publication identity does not match exactly one protected command' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.operation_id,NEW.binding_id,NEW.provider,NEW.installation_id,NEW.repository_id,NEW.repository_owner,NEW.repository_name,NEW.target_ref,NEW.base_revision,NEW.candidate_ref,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.operation_id,OLD.binding_id,OLD.provider,OLD.installation_id,OLD.repository_id,OLD.repository_owner,OLD.repository_name,OLD.target_ref,OLD.base_revision,OLD.candidate_ref,OLD.created_at) THEN
        RAISE EXCEPTION 'pull request publication identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN RAISE EXCEPTION 'pull request publication update is not fenced' USING ERRCODE='23514'; END IF;
    IF OLD.write_base_revision<>'' AND NEW.write_base_revision<>OLD.write_base_revision THEN RAISE EXCEPTION 'pull request write base is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.candidate_revision<>'' AND NEW.candidate_revision<>OLD.candidate_revision THEN RAISE EXCEPTION 'pull request candidate revision is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.pull_request_number>0 AND (NEW.pull_request_number<>OLD.pull_request_number OR NEW.pull_request_url<>OLD.pull_request_url) THEN RAISE EXCEPTION 'pull request identity is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.merge_revision<>'' AND NEW.merge_revision<>OLD.merge_revision THEN RAISE EXCEPTION 'pull request merge revision is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.target_revision<>'' AND NEW.target_revision<>OLD.target_revision THEN RAISE EXCEPTION 'verified target revision is immutable' USING ERRCODE='23514'; END IF;
    IF NOT ((OLD.state='pending-candidate' AND NEW.state='write-base-ready') OR
        (OLD.state='write-base-ready' AND NEW.state='candidate-ready') OR
        (OLD.state='candidate-ready' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-open' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='pull-request-closed' AND NEW.state IN ('pull-request-open','pull-request-closed','merge-pending')) OR
        (OLD.state='merge-pending' AND NEW.state IN ('merge-pending','merge-verified')) OR
        (OLD.state='merge-verified' AND NEW.state='merge-verified')) THEN
        RAISE EXCEPTION 'invalid pull request publication transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_git_reconciliation_lease_epoch(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_git_reconciliation_lease_epoch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'Git reconciliation lease epoch cannot move backwards'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND OLD.lease_owner IS NULL
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Git reconciliation acquisition must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND OLD.lease_owner IS NOT NULL
       AND (NEW.lease_owner<>OLD.lease_owner OR NEW.lease_epoch<>OLD.lease_epoch)
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Git reconciliation replacement must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_git_webhook_tombstone(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_git_webhook_tombstone() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'Git webhook tombstones are permanent'
        USING ERRCODE='23514';
END;
$$;


--
-- Name: protect_git_write_command(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_git_write_command() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.command_kind='variable-set' THEN
            RAISE EXCEPTION 'Git VariableSet write commands are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending' OR NEW.committed_revision<>'' OR NEW.committed_at IS NOT NULL OR
           NEW.indexed_generation<>0 OR NEW.indexed_at IS NOT NULL OR NEW.updated_at<>NEW.created_at THEN
            RAISE EXCEPTION 'Git write command must start pristine pending' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.operation_id,NEW.command_kind,NEW.deployment_id,NEW.actor_id,NEW.binding_id,NEW.project_id,
        NEW.environment_id,NEW.application_id,NEW.variable_scope,NEW.target_ref,NEW.path,NEW.base_revision,
        NEW.precondition,NEW.expected_etag,NEW.chart_identity,NEW.policy_version,NEW.content,NEW.content_sha256,
        NEW.message,NEW.action,NEW.publication_mode,NEW.request_digest,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.operation_id,OLD.command_kind,OLD.deployment_id,OLD.actor_id,OLD.binding_id,OLD.project_id,
        OLD.environment_id,OLD.application_id,OLD.variable_scope,OLD.target_ref,OLD.path,OLD.base_revision,
        OLD.precondition,OLD.expected_etag,OLD.chart_identity,OLD.policy_version,OLD.content,OLD.content_sha256,
        OLD.message,OLD.action,OLD.publication_mode,OLD.request_digest,OLD.created_at) THEN
        RAISE EXCEPTION 'Git write command identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at<OLD.updated_at THEN
        RAISE EXCEPTION 'Git write command time cannot regress' USING ERRCODE='23514';
    END IF;
    IF OLD.command_kind='variable-set' AND NOT (
        (OLD.publication_mode='direct' AND OLD.state='pending' AND NEW.state='git-committed') OR
        (OLD.publication_mode='direct' AND OLD.state='git-committed' AND NEW.state='indexed') OR
        (OLD.publication_mode='pull-request' AND OLD.state='pending' AND NEW.state='indexed' AND EXISTS (
            SELECT 1 FROM git_pull_request_publications p
            WHERE p.operation_id=OLD.operation_id AND p.state='merge-verified'))
    ) THEN
        RAISE EXCEPTION 'invalid Git VariableSet write command transition' USING ERRCODE='23514';
    END IF;
    IF OLD.command_kind='deployment' AND (
        (OLD.state='git-committed' AND NEW.state='pending') OR
        (OLD.state='indexed' AND NEW.state<>'indexed')) THEN
        RAISE EXCEPTION 'Git deployment write command state cannot regress' USING ERRCODE='23514';
    END IF;
    IF OLD.committed_revision<>'' AND
       (NEW.committed_revision<>OLD.committed_revision OR NEW.committed_at<>OLD.committed_at) THEN
        RAISE EXCEPTION 'Git write result is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_managed_audit_event(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_managed_audit_event() RETURNS trigger
    LANGUAGE plpgsql
    AS $_$
DECLARE
    event_type text;
    revision_value bigint;
    event_profile_kind text;
BEGIN
    IF TG_OP='DELETE' THEN event_type := OLD.target_type; ELSE event_type := NEW.target_type; END IF;
    IF TG_OP <> 'INSERT' THEN
        IF OLD.target_type IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy')
           OR (TG_OP='UPDATE' AND NEW.target_type IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy')) THEN
            RAISE EXCEPTION 'managed audit events are immutable' USING ERRCODE='23514';
        END IF;
        IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
    END IF;
    IF event_type NOT IN ('scheduling-profile','middleware-profile','certificate-issuer-profile','auto-deploy-policy') THEN
        RETURN NEW;
    END IF;
    IF NEW.detail->>'revision' !~ '^[1-9][0-9]*$' THEN
        RAISE EXCEPTION 'managed audit revision is invalid' USING ERRCODE='23514';
    END IF;
    revision_value := (NEW.detail->>'revision')::bigint;
    IF event_type IN ('scheduling-profile','middleware-profile') THEN
        event_profile_kind := CASE event_type WHEN 'scheduling-profile' THEN 'scheduling' ELSE 'middleware' END;
        IF NOT (NEW.detail ?& ARRAY['revision','idempotencyKey','specDigest','assignmentsDigest'])
           OR NEW.detail - ARRAY['revision','idempotencyKey','specDigest','assignmentsDigest'] <> '{}'::jsonb
           OR NEW.detail->>'idempotencyKey' !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
           OR NEW.detail->>'specDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NEW.detail->>'assignmentsDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR (event_type='scheduling-profile' AND NEW.action NOT IN ('scheduling-profile.create','scheduling-profile.revise','scheduling-profile.deactivate'))
           OR (event_type='middleware-profile' AND NEW.action NOT IN ('middleware-profile.create','middleware-profile.revise','middleware-profile.clone','middleware-profile.deactivate'))
           OR NOT EXISTS (SELECT 1 FROM configuration_profile_revisions r
                WHERE r.profile_id=NEW.target_id AND r.revision=revision_value AND r.profile_kind=event_profile_kind
                  AND r.spec_digest=NEW.detail->>'specDigest'
                  AND r.assignments_digest=NEW.detail->>'assignmentsDigest') THEN
            RAISE EXCEPTION 'configuration profile audit authority mismatch' USING ERRCODE='23514';
        END IF;
    ELSIF event_type='certificate-issuer-profile' THEN
        IF NEW.action NOT IN ('certificate-issuer-profile.create','certificate-issuer-profile.revise','certificate-issuer-profile.deactivate')
           OR NOT (NEW.detail ?& ARRAY['revision','idempotencyKey','specDigest'])
           OR NEW.detail - ARRAY['revision','idempotencyKey','specDigest'] <> '{}'::jsonb
           OR NEW.detail->>'idempotencyKey' !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'
           OR NEW.detail->>'specDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin')
           OR NOT EXISTS (SELECT 1 FROM configuration_profile_revisions r
                WHERE r.profile_id=NEW.target_id AND r.revision=revision_value AND r.profile_kind='certificate-issuer'
                  AND r.spec_digest=NEW.detail->>'specDigest') THEN
            RAISE EXCEPTION 'certificate issuer audit authority mismatch' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.action NOT IN ('auto-deploy-policy.create','auto-deploy-policy.revise','auto-deploy-policy.enable','auto-deploy-policy.disable')
           OR NOT (NEW.detail ?& ARRAY['revision','serviceActorId','templateDigest'])
           OR NEW.detail - ARRAY['revision','serviceActorId','templateDigest'] <> '{}'::jsonb
           OR NEW.detail->>'serviceActorId' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
           OR NEW.detail->>'templateDigest' !~ '^sha256:[0-9a-f]{64}$'
           OR NOT EXISTS (SELECT 1 FROM auto_deploy_policy_revisions r
                WHERE r.policy_id=NEW.target_id AND r.revision=revision_value
                  AND r.service_actor_id::text=NEW.detail->>'serviceActorId'
                  AND r.template_digest=NEW.detail->>'templateDigest') THEN
            RAISE EXCEPTION 'auto-deploy audit authority mismatch' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$_$;


--
-- Name: protect_permanent_github_claim(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_permanent_github_claim() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.permanent THEN
        RAISE EXCEPTION 'permanent GitHub claim tombstones cannot be changed or deleted'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND (NEW.kind <> OLD.kind OR NEW.claim_key <> OLD.claim_key) THEN
        RAISE EXCEPTION 'GitHub claim identity is immutable'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_preview_authority(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_preview_authority() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.preview_kind='variable-set' THEN
            RAISE EXCEPTION 'VariableSet previews are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.consumed_at IS NOT NULL OR NEW.created_at>now()+interval '1 minute' OR
           (NEW.preview_kind='variable-set' AND NEW.expires_at>NEW.created_at+interval '15 minutes') THEN
            RAISE EXCEPTION 'preview must start pristine and bounded' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF ROW(NEW.token_hash,NEW.preview_kind,NEW.actor_id,NEW.deployment_id,NEW.binding_id,NEW.project_id,
        NEW.environment_id,NEW.variable_scope,NEW.path,NEW.base_revision,NEW.base_etag,NEW.expected_etag,
        NEW.policy_version,NEW.chart_identity,NEW.candidate_hash,NEW.expires_at,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.token_hash,OLD.preview_kind,OLD.actor_id,OLD.deployment_id,OLD.binding_id,OLD.project_id,
        OLD.environment_id,OLD.variable_scope,OLD.path,OLD.base_revision,OLD.base_etag,OLD.expected_etag,
        OLD.policy_version,OLD.chart_identity,OLD.candidate_hash,OLD.expires_at,OLD.created_at) OR
       OLD.consumed_at IS NOT NULL OR NEW.consumed_at IS NULL OR NEW.consumed_at<OLD.created_at OR
       NEW.consumed_at>now()+interval '1 minute' THEN
        RAISE EXCEPTION 'preview update is not an exact consumption' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_registry_runtime_gc_receipt(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_registry_runtime_gc_receipt() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'Registry garbage-collection receipts are immutable'
        USING ERRCODE='23514';
END;
$$;


--
-- Name: protect_registry_runtime_maintenance_epoch(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_registry_runtime_maintenance_epoch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.registry_target_id<>OLD.registry_target_id OR
       NEW.execution_key<>OLD.execution_key OR NEW.plan_id<>OLD.plan_id OR
       NEW.candidate_set_digest<>OLD.candidate_set_digest THEN
        RAISE EXCEPTION 'Registry maintenance immutable identity changed'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_epoch<OLD.lease_epoch OR
       (NEW.lease_owner<>OLD.lease_owner OR NEW.lease_epoch<>OLD.lease_epoch)
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Registry maintenance acquisition must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    IF OLD.released_at IS NOT NULL AND NEW IS DISTINCT FROM OLD AND NOT (
       NEW.state='acquired' AND NEW.lease_epoch=OLD.lease_epoch+1 AND
       NEW.lease_owner<>'' AND NEW.lease_until>NEW.updated_at AND
       NEW.maintenance_mode IS NULL AND NEW.deployment_uid='' AND
       NEW.original_replicas IS NULL AND NEW.checkpoint_revision='' AND
       NEW.checkpoint_digest='' AND NEW.checkpoint_observed_at IS NULL AND
       NEW.sweep_job_uid=OLD.sweep_job_uid AND
       NEW.restored_at IS NULL AND NEW.released_at IS NULL
    ) THEN
        RAISE EXCEPTION 'Released registry maintenance may only be reacquired for a fresh replay checkpoint'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_registry_runtime_observation_epoch(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_registry_runtime_observation_epoch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.lease_epoch<OLD.lease_epoch THEN
        RAISE EXCEPTION 'Registry observation lease epoch cannot move backwards'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL
       AND (OLD.lease_owner IS NULL OR NEW.lease_owner<>OLD.lease_owner OR NEW.lease_epoch<>OLD.lease_epoch)
       AND NEW.lease_epoch<>OLD.lease_epoch+1 THEN
        RAISE EXCEPTION 'Registry observation acquisition must increment the lease epoch'
            USING ERRCODE='23514';
    END IF;
    IF NEW.completed_revision<OLD.completed_revision THEN
        RAISE EXCEPTION 'Registry observation revision cannot move backwards'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_secret_binding_event(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_secret_binding_event() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'secret binding events are permanent' USING ERRCODE='23514';
    END IF;
    IF ROW(NEW.id,NEW.binding_id,NEW.version_id,NEW.actor_id,NEW.kind,NEW.request_id,NEW.occurred_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.binding_id,OLD.version_id,OLD.actor_id,OLD.kind,OLD.request_id,OLD.occurred_at) OR
       (OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at) THEN
        RAISE EXCEPTION 'secret binding event is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_secret_binding_identity(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_secret_binding_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF ROW(NEW.organization_id,NEW.project_id,NEW.environment_id,NEW.application_id,
           NEW.target_namespace,NEW.name,NEW.provider,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.organization_id,OLD.project_id,OLD.environment_id,OLD.application_id,
           OLD.target_namespace,OLD.name,OLD.provider,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'secret binding identity is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'secret binding time cannot move backwards' USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='provisioning' AND NEW.state IN ('ready','failed')) OR
        (OLD.state='ready' AND NEW.state IN ('ready','deleting')) OR
        (OLD.state='failed' AND NEW.state='deleting') OR
        (OLD.state='deleting' AND NEW.state='deleted') OR
        OLD.state=NEW.state
    ) THEN
        RAISE EXCEPTION 'invalid secret binding transition' USING ERRCODE='23514';
    END IF;
    IF NEW.state='ready' AND NOT EXISTS (
        SELECT 1 FROM secret_binding_versions v
        WHERE v.binding_id=NEW.id AND v.version_number=NEW.active_version AND v.state='active'
    ) THEN
        RAISE EXCEPTION 'active secret binding version is not ready' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_secret_binding_purpose(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_secret_binding_purpose() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.purpose IS DISTINCT FROM OLD.purpose THEN
        RAISE EXCEPTION 'secret binding purpose is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_secret_binding_version(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_secret_binding_version() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF ROW(NEW.binding_id,NEW.version_number,NEW.provider,NEW.fingerprint_key_id,
           NEW.content_fingerprint,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.binding_id,OLD.version_number,OLD.provider,OLD.fingerprint_key_id,
           OLD.content_fingerprint,OLD.created_at) THEN
        RAISE EXCEPTION 'secret binding version identity is immutable' USING ERRCODE='23514';
    END IF;
    IF ROW(NEW.provider_object_name,NEW.target_secret_name,NEW.provider_revision,
           NEW.manifest_digest,NEW.sealed_key_fingerprint,NEW.ciphertext_digest)
       IS DISTINCT FROM
       ROW(OLD.provider_object_name,OLD.target_secret_name,OLD.provider_revision,
           OLD.manifest_digest,OLD.sealed_key_fingerprint,OLD.ciphertext_digest)
       AND NOT (OLD.state='staging' AND NEW.state='awaiting-readiness'
                AND OLD.provider_object_name IS NULL) THEN
        RAISE EXCEPTION 'secret binding provider artifact is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'secret binding version time cannot move backwards' USING ERRCODE='23514';
    END IF;
    IF NEW.failure_code IS DISTINCT FROM OLD.failure_code
       AND NOT (NEW.state='failed' AND OLD.failure_code='') THEN
        RAISE EXCEPTION 'secret binding failure is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.staged_at IS DISTINCT FROM OLD.staged_at
       AND NOT (OLD.state='staging' AND NEW.state='awaiting-readiness' AND OLD.staged_at IS NULL AND NEW.staged_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding staged time is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.readiness_observed_at IS DISTINCT FROM OLD.readiness_observed_at
       AND NOT (OLD.state IN ('staging','awaiting-readiness') AND NEW.state IN ('active','failed')
                AND OLD.readiness_observed_at IS NULL AND NEW.readiness_observed_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding readiness time is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.activated_at IS DISTINCT FROM OLD.activated_at
       AND NOT (OLD.state='awaiting-readiness' AND NEW.state='active' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding activation time is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.retained_at IS DISTINCT FROM OLD.retained_at
       AND NOT (OLD.state='active' AND NEW.state='retained' AND OLD.retained_at IS NULL AND NEW.retained_at IS NOT NULL) THEN
        RAISE EXCEPTION 'secret binding retention time is immutable' USING ERRCODE='23514';
    END IF;
    IF NOT (
        (OLD.state='staging' AND NEW.state IN ('awaiting-readiness','failed','deleted')) OR
        (OLD.state='awaiting-readiness' AND NEW.state IN ('active','failed','deleted')) OR
        (OLD.state='active' AND NEW.state IN ('retained','deleted')) OR
        (OLD.state='retained' AND NEW.state='deleted') OR
        (OLD.state='failed' AND NEW.state='deleted') OR
        OLD.state=NEW.state
    ) THEN
        RAISE EXCEPTION 'invalid secret binding version transition' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: protect_tls_certificate_observation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_tls_certificate_observation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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


--
-- Name: protect_tls_certificate_version(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.protect_tls_certificate_version() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'certificate attestations are append-only' USING ERRCODE='23514';
END;
$$;


--
-- Name: record_verified_argo_desired_state_materialization(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.record_verified_argo_desired_state_materialization() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$
BEGIN
    IF NEW.state='verified' AND NEW.policy_digest IS NOT NULL AND
       NEW.app_project_content IS NOT NULL AND
       (TG_OP='INSERT' OR OLD.state<>'verified') THEN
        INSERT INTO public.argo_desired_state_materialization_receipts(
            id,environment_binding_id,environment_revision,environment_generation,
            project_id,environment_id,platform_binding_id,
            platform_target_ref,environment_target_ref,desired_state_command_id,
            desired_state_generation,desired_state_revision,desired_state_content_sha256,
            catalog_digest,policy_digest,chart_repository,chart_name,chart_version,chart_digest,
            renderer_image,chart_digest_enforcement,app_project_content,created_at
        )
        SELECT NEW.id,NEW.environment_binding_id,NEW.environment_revision,
               NEW.environment_generation,NEW.project_id,NEW.environment_id,
               NEW.platform_binding_id,NEW.platform_target_ref,
               NEW.environment_target_ref,NEW.id,NEW.generation,
               NEW.committed_revision,NEW.content_sha256,NEW.catalog_digest,NEW.policy_digest,
               NEW.chart_repository,NEW.chart_name,NEW.chart_version,
               NEW.chart_digest,NEW.renderer_image,NEW.chart_digest_enforcement,
               NEW.app_project_content,NEW.verified_at
        FROM public.git_repository_bindings binding
        JOIN public.git_projection_generations generation
          ON generation.binding_id=binding.id
         AND generation.generation=NEW.environment_generation
        WHERE binding.id=NEW.environment_binding_id
          AND binding.target_head_revision=NEW.environment_revision
          AND binding.indexed_revision=NEW.environment_revision
          AND binding.projection_generation=NEW.environment_generation
          AND binding.state='ready' AND generation.state='active'
          AND generation.head_revision=NEW.environment_revision;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reject_auto_deploy_immutable_change(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_auto_deploy_immutable_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
	IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RAISE EXCEPTION 'auto-deploy immutable record cannot change' USING ERRCODE='23514';
END; $$;


--
-- Name: reject_configuration_profile_immutable_change(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_configuration_profile_immutable_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'configuration profile immutable record cannot change' USING ERRCODE='23514';
END;
$$;


--
-- Name: reject_external_registry_cleanup_plan(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_external_registry_cleanup_plan() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM registry_targets
        WHERE id=NEW.registry_target_id AND mode<>'managed'
    ) THEN
        RAISE EXCEPTION 'cleanup is forbidden for external registry targets'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reject_git_push_wake_change(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_git_push_wake_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF TG_OP='DELETE' AND TG_TABLE_NAME='git_projection_push_wake_targets' AND pg_trigger_depth()>1 THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'Git push wake receipts are immutable' USING ERRCODE='23514';
END; $$;


--
-- Name: reject_registry_target_mode_change(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_registry_target_mode_change() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.mode <> NEW.mode THEN
        RAISE EXCEPTION 'registry target mode is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: reject_secret_binding_delivery_mutation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_secret_binding_delivery_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'secret binding deliveries are immutable' USING ERRCODE='23514';
END;
$$;


--
-- Name: reject_secret_binding_reference_update(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.reject_secret_binding_reference_update() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'secret binding references cannot be rebound' USING ERRCODE='23514';
END;
$$;


--
-- Name: require_argo_desired_state_policy_digest(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.require_argo_desired_state_policy_digest() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.policy_digest IS NULL THEN
        RAISE EXCEPTION 'new Argo desired-state commands require an exact policy digest'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: require_closed_git_publication_command(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.require_closed_git_publication_command() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (SELECT count(*) FROM git_write_commands WHERE operation_id=NEW.operation_id) <> 1 THEN
        RAISE EXCEPTION 'Git publication must reference exactly one closed command' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_argo_desired_state_app_project_content(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_argo_desired_state_app_project_content() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$
BEGIN
    IF TG_OP='INSERT' AND (NEW.app_project_content IS NULL OR
       pg_catalog.octet_length(NEW.app_project_content)=0) THEN
        RAISE EXCEPTION 'new Argo desired state requires canonical AppProject bytes'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND NEW.app_project_content IS DISTINCT FROM OLD.app_project_content THEN
        RAISE EXCEPTION 'Argo desired-state AppProject bytes are immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.app_project_content IS NOT NULL AND
       NEW.content<>NEW.app_project_content||pg_catalog.convert_to(E'---\n','UTF8')||
          pg_catalog.substr(NEW.content,pg_catalog.octet_length(NEW.app_project_content)+5) THEN
        RAISE EXCEPTION 'Argo desired-state AppProject bytes do not frame the command bundle'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_argo_desired_state_command(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_argo_desired_state_command() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    platform_kind text;
    platform_mode text;
    platform_ref text;
    platform_state text;
    platform_head text;
    environment_kind text;
    environment_ref text;
    environment_project uuid;
    bound_environment uuid;
    environment_state text;
    environment_head text;
    environment_indexed text;
    environment_projection_generation bigint;
BEGIN
    SELECT kind,credential_mode,target_ref,state,target_head_revision
      INTO platform_kind,platform_mode,platform_ref,platform_state,platform_head
      FROM git_repository_bindings
     WHERE id=NEW.platform_binding_id;
    IF platform_kind IS DISTINCT FROM 'platform' OR
       platform_mode IS DISTINCT FROM 'github-app' OR
       platform_ref IS DISTINCT FROM NEW.platform_target_ref THEN
        RAISE EXCEPTION 'Argo desired state requires the exact protected GitHub App platform binding'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       platform_state NOT IN ('ready','indexing') OR
       platform_head IS DISTINCT FROM NEW.base_revision
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires the exact provider-verified planned platform head'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       NEW.write_base_revision<>'' OR NEW.write_base_observed_at IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'Argo desired-state write base may only be set by a fenced claim'
            USING ERRCODE='23514';
    END IF;

    SELECT kind,target_ref,project_id,environment_id,state,target_head_revision,
           indexed_revision,projection_generation
      INTO environment_kind,environment_ref,environment_project,bound_environment,
           environment_state,environment_head,environment_indexed,
           environment_projection_generation
      FROM git_repository_bindings
     WHERE id=NEW.environment_binding_id;
    IF environment_kind IS DISTINCT FROM 'environment' OR
       environment_ref IS DISTINCT FROM NEW.environment_target_ref OR
       environment_project IS DISTINCT FROM NEW.project_id OR
       bound_environment IS DISTINCT FROM NEW.environment_id THEN
        RAISE EXCEPTION 'Argo desired state environment binding identity does not match'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND (
       environment_state IS DISTINCT FROM 'ready' OR
       environment_head IS DISTINCT FROM environment_indexed OR
       environment_indexed IS DISTINCT FROM NEW.environment_revision OR
       environment_projection_generation IS DISTINCT FROM NEW.environment_generation
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires the exact active indexed environment generation'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND NOT EXISTS (
        SELECT 1 FROM git_projection_generations generation
         WHERE generation.binding_id=NEW.environment_binding_id
           AND generation.generation=NEW.environment_generation
           AND generation.head_revision=NEW.environment_revision
           AND generation.state='active'
    ) THEN
        RAISE EXCEPTION 'Argo desired state requires an activated projection receipt'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' AND EXISTS (
        SELECT 1 FROM git_projected_documents document
         WHERE document.binding_id=NEW.environment_binding_id
           AND document.generation=NEW.environment_generation
           AND NOT document.valid
    ) THEN
        RAISE EXCEPTION 'Argo desired state refuses an invalid projected document'
            USING ERRCODE='23514';
    END IF;

    IF NEW.destination_namespace IS DISTINCT FROM (
        SELECT namespace FROM environments WHERE id=NEW.environment_id
    ) OR NEW.argo_project IS DISTINCT FROM (
        SELECT argo_project FROM environments WHERE id=NEW.environment_id
    ) THEN
        RAISE EXCEPTION 'Argo destination identity must be server-derived'
            USING ERRCODE='23514';
    END IF;

    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.id,NEW.generation,NEW.project_id,NEW.environment_id,
               NEW.platform_binding_id,NEW.environment_binding_id,
               NEW.platform_target_ref,NEW.environment_target_ref,
               NEW.environment_revision,NEW.environment_generation,NEW.path,
               NEW.argo_namespace,NEW.destination_namespace,NEW.argo_project,
               NEW.base_revision,NEW.precondition,NEW.expected_etag,
               NEW.catalog_digest,NEW.chart_repository,NEW.chart_name,
               NEW.chart_version,NEW.chart_digest,NEW.renderer_image,
               NEW.chart_digest_enforcement,NEW.content,NEW.content_sha256,
               NEW.message,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.generation,OLD.project_id,OLD.environment_id,
               OLD.platform_binding_id,OLD.environment_binding_id,
               OLD.platform_target_ref,OLD.environment_target_ref,
               OLD.environment_revision,OLD.environment_generation,OLD.path,
               OLD.argo_namespace,OLD.destination_namespace,OLD.argo_project,
               OLD.base_revision,OLD.precondition,OLD.expected_etag,
               OLD.catalog_digest,OLD.chart_repository,OLD.chart_name,
               OLD.chart_version,OLD.chart_digest,OLD.renderer_image,
               OLD.chart_digest_enforcement,OLD.content,OLD.content_sha256,
               OLD.message,OLD.created_at) THEN
            RAISE EXCEPTION 'Argo desired-state command identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Argo desired-state command epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           NEW.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) THEN
            RAISE EXCEPTION 'Argo desired-state lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.write_base_revision<>'' AND
           ROW(NEW.write_base_revision,NEW.write_base_observed_at)
           IS DISTINCT FROM
           ROW(OLD.write_base_revision,OLD.write_base_observed_at) THEN
            RAISE EXCEPTION 'Argo desired-state write-base receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.write_base_revision='' AND NEW.write_base_revision<>'' AND (
           OLD.state<>'claimed' OR NEW.state<>'claimed' OR
           OLD.lease_owner IS NULL OR NEW.lease_owner IS NULL OR
           NEW.lease_epoch<>OLD.lease_epoch OR
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest)
        ) THEN
            RAISE EXCEPTION 'Argo desired-state write-base receipt requires the exact fenced claim'
                USING ERRCODE='23514';
        END IF;
        IF OLD.state IN ('verified','blocked-prerequisite','failed','superseded') AND
           NEW.state<>OLD.state THEN
            RAISE EXCEPTION 'Argo desired-state terminal state is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.state IN ('verified','blocked-prerequisite','failed','superseded') AND
           ROW(NEW.state,NEW.committed_revision,NEW.committed_at,NEW.verified_at,
               NEW.write_base_revision,NEW.write_base_observed_at,
               NEW.next_attempt_at,NEW.consecutive_failures,NEW.last_failure_code,
               NEW.lease_owner,NEW.lease_epoch,NEW.lease_until,
               NEW.worker_contract,NEW.worker_config_digest,NEW.updated_at,
               NEW.completed_at)
           IS DISTINCT FROM
           ROW(OLD.state,OLD.committed_revision,OLD.committed_at,OLD.verified_at,
               OLD.write_base_revision,OLD.write_base_observed_at,
               OLD.next_attempt_at,OLD.consecutive_failures,OLD.last_failure_code,
               OLD.lease_owner,OLD.lease_epoch,OLD.lease_until,
               OLD.worker_contract,OLD.worker_config_digest,OLD.updated_at,
               OLD.completed_at) THEN
            RAISE EXCEPTION 'Argo desired-state terminal result is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND
           ROW(NEW.committed_revision,NEW.committed_at)
           IS DISTINCT FROM ROW(OLD.committed_revision,OLD.committed_at) THEN
            RAISE EXCEPTION 'Argo desired-state Git receipt is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.completed_at IS NOT NULL AND NEW.completed_at<>OLD.completed_at) THEN
            RAISE EXCEPTION 'Argo desired-state command time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_argo_desired_state_materialization_receipt(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_argo_desired_state_materialization_receipt() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$
BEGIN
    IF TG_OP='DELETE' AND pg_trigger_depth()>1 THEN
        RETURN OLD;
    END IF;
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'Argo desired-state materialization receipts are immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.policy_digest IS NULL OR NOT EXISTS (
        SELECT 1
        FROM public.git_repository_bindings environment_binding
        JOIN public.git_projection_generations generation
          ON generation.binding_id=environment_binding.id
         AND generation.generation=NEW.environment_generation
        JOIN public.git_repository_bindings platform ON platform.id=NEW.platform_binding_id
        WHERE environment_binding.id=NEW.environment_binding_id
          AND environment_binding.kind='environment'
          AND environment_binding.credential_mode='github-app'
          AND environment_binding.state='ready'
          AND environment_binding.project_id=NEW.project_id
          AND environment_binding.environment_id=NEW.environment_id
          AND environment_binding.target_ref=NEW.environment_target_ref
          AND environment_binding.target_head_revision=NEW.environment_revision
          AND environment_binding.indexed_revision=NEW.environment_revision
          AND environment_binding.projection_generation=NEW.environment_generation
          AND generation.head_revision=NEW.environment_revision AND generation.state='active'
          AND platform.kind='platform' AND platform.credential_mode='github-app'
          AND platform.target_ref=NEW.platform_target_ref
          AND platform.state IN ('ready','indexing')
          AND NOT EXISTS(SELECT 1 FROM public.git_projected_documents document
            WHERE document.binding_id=NEW.environment_binding_id
              AND document.generation=NEW.environment_generation AND NOT document.valid)
    ) THEN
        RAISE EXCEPTION 'Argo materialization receipt requires exact current projection authority'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.argo_desired_state_commands command
        WHERE command.id=NEW.desired_state_command_id
          AND command.generation=NEW.desired_state_generation
          AND command.project_id=NEW.project_id AND command.environment_id=NEW.environment_id
          AND command.platform_binding_id=NEW.platform_binding_id
          AND command.environment_binding_id=NEW.environment_binding_id
          AND command.platform_target_ref=NEW.platform_target_ref
          AND command.environment_target_ref=NEW.environment_target_ref
          AND command.state='verified' AND command.committed_revision=NEW.desired_state_revision
          AND command.content_sha256=NEW.desired_state_content_sha256
          AND command.app_project_content=NEW.app_project_content
          AND command.write_base_revision<>'' AND command.verified_at IS NOT NULL
          AND command.completed_at=command.verified_at
    ) THEN
        RAISE EXCEPTION 'Argo materialization receipt requires exact verified desired state'
            USING ERRCODE='23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.argo_desired_state_commands later
        WHERE later.project_id=NEW.project_id AND later.environment_id=NEW.environment_id
          AND later.generation>NEW.desired_state_generation
          AND (later.state NOT IN ('failed','superseded') OR later.completed_at IS NULL OR
               later.completed_at>=NEW.created_at)
    ) THEN
        RAISE EXCEPTION 'Argo materialization receipt is behind newer desired-state authority'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_argo_materialization_app_project_content(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_argo_materialization_app_project_content() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'pg_temp'
    AS $$
BEGIN
    IF TG_OP='DELETE' AND pg_trigger_depth()>1 THEN
        RETURN OLD;
    END IF;
    IF TG_OP='DELETE' OR TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Argo materialization AppProject authority is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.app_project_content IS NULL OR pg_catalog.octet_length(NEW.app_project_content)=0 OR
       NOT EXISTS(SELECT 1 FROM public.argo_desired_state_commands command
         WHERE command.id=NEW.desired_state_command_id
           AND command.app_project_content=NEW.app_project_content) THEN
        RAISE EXCEPTION 'Argo materialization lacks canonical AppProject authority'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_auto_deploy_policy_revision(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_auto_deploy_policy_revision() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE policy_row auto_deploy_policies%ROWTYPE;
BEGIN
    SELECT * INTO STRICT policy_row FROM auto_deploy_policies WHERE id=NEW.policy_id;
    IF NOT EXISTS (
        SELECT 1 FROM applications a
        JOIN environments e ON e.id=policy_row.environment_id AND e.project_id=a.project_id
        JOIN deployments d ON d.id=NEW.source_deployment_id
             AND d.application_id=a.id AND d.environment_id=e.id AND d.generation=NEW.source_deployment_generation
        JOIN service_accounts sa ON sa.id=NEW.service_actor_id AND sa.project_id=a.project_id
        WHERE a.id=policy_row.application_id AND a.project_id=policy_row.project_id
          AND a.build_source_id IS NOT NULL
          AND (NOT NEW.enabled OR sa.disabled_at IS NULL)
    ) THEN
        RAISE EXCEPTION 'auto-deploy policy resource binding mismatch' USING ERRCODE='23503';
    END IF;
    IF NEW.created_at<policy_row.created_at THEN
        RAISE EXCEPTION 'auto-deploy revision predates policy' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END; $$;


--
-- Name: validate_cert_manager_issuer_reference_scope(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_cert_manager_issuer_reference_scope() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM configuration_profiles p
        JOIN applications a ON a.id=NEW.application_id
        JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id
        JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment'
        WHERE p.id=NEW.profile_id AND p.kind='certificate-issuer'
          AND NEW.git_path=b.path_prefix || '/apps/' || a.id::text || '/app.yaml'
    ) THEN
        RAISE EXCEPTION 'certificate issuer reference destination mismatch' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_certificate_issuer_profile_child(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_certificate_issuer_profile_child() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM configuration_profile_revisions
        WHERE profile_id=NEW.profile_id AND revision=NEW.revision
          AND profile_kind='certificate-issuer'
    ) THEN
        RAISE EXCEPTION 'certificate issuer child references a non-issuer profile revision' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_configuration_profile_assignment(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_configuration_profile_assignment() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.profile_kind='scheduling' THEN
        IF NEW.scope_type NOT IN ('team','project','environment') OR
           (NEW.scope_type='team' AND NOT EXISTS (SELECT 1 FROM teams WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) THEN
            RAISE EXCEPTION 'scheduling profile assignment scope does not exist' USING ERRCODE='23503';
        END IF;
    ELSIF NEW.profile_kind='middleware' THEN
        IF NEW.scope_type NOT IN ('project','environment','application') OR
           (NEW.scope_type='project' AND NOT EXISTS (SELECT 1 FROM projects WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='environment' AND NOT EXISTS (SELECT 1 FROM environments WHERE id=NEW.scope_id)) OR
           (NEW.scope_type='application' AND NOT EXISTS (SELECT 1 FROM applications WHERE id=NEW.scope_id)) THEN
            RAISE EXCEPTION 'middleware profile assignment scope does not exist' USING ERRCODE='23503';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_configuration_profile_revision(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_configuration_profile_revision() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.profile_kind='certificate-issuer' THEN
        IF NEW.solver_type NOT IN ('http01','dns01-cloudflare') OR
           NEW.assignments_digest IS NOT NULL OR NEW.cloned_from_profile_id IS NOT NULL OR
           octet_length(NEW.spec::text)>32768 THEN
            RAISE EXCEPTION 'invalid certificate issuer profile revision' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.profile_kind='scheduling' THEN
        IF NEW.solver_type IS NOT NULL OR NEW.assignments_digest IS NULL OR NEW.cloned_from_profile_id IS NOT NULL THEN
            RAISE EXCEPTION 'invalid scheduling profile revision' USING ERRCODE='23514';
        END IF;
    ELSIF NEW.profile_kind='middleware' THEN
        IF NEW.solver_type IS NOT NULL OR NEW.assignments_digest IS NULL THEN
            RAISE EXCEPTION 'invalid middleware profile revision' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_edge_runtime_target(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_edge_runtime_target() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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


--
-- Name: validate_git_write_operation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_git_write_operation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE expected_target uuid;
BEGIN
    IF NEW.command_kind<>'variable-set' THEN RETURN NEW; END IF;
    expected_target := CASE WHEN NEW.variable_scope='project' THEN NEW.project_id ELSE NEW.environment_id END;
    IF NOT EXISTS (SELECT 1 FROM operations o WHERE o.id=NEW.operation_id AND o.kind='variable-set.git-write'
        AND o.target_type=NEW.variable_scope AND o.target_id=expected_target AND o.status='queued' AND o.generation=1
        AND o.lease_owner IS NULL AND o.lease_until IS NULL) THEN
        RAISE EXCEPTION 'Git VariableSet command operation identity mismatch' USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM mutation_receipts i WHERE i.receipt_kind='resource'
        AND i.operation_id=NEW.operation_id AND i.actor_id=NEW.actor_id AND i.request_digest=NEW.request_digest) THEN
        RAISE EXCEPTION 'Git VariableSet command request authority mismatch' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_middleware_profile_reference_scope(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_middleware_profile_reference_scope() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM configuration_profiles p
        JOIN applications a ON a.id=NEW.application_id
        JOIN environments e ON e.id=NEW.environment_id AND e.project_id=a.project_id
        JOIN git_repository_bindings b ON b.project_id=e.project_id AND b.environment_id=e.id AND b.kind='environment'
        WHERE p.id=NEW.profile_id AND p.kind='middleware'
          AND NEW.git_path=b.path_prefix || '/apps/' || NEW.application_id::text || '/app.yaml'
    ) THEN
        RAISE EXCEPTION 'middleware profile reference destination does not match application/environment/Git binding' USING ERRCODE='23503';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_mutation_receipt(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_mutation_receipt() RETURNS trigger
    LANGUAGE plpgsql
    AS $_$
BEGIN
	IF TG_OP='DELETE' AND OLD.receipt_kind='auto-deploy-policy' THEN
		RETURN OLD;
	END IF;
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'mutation receipts are immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='configuration-profile' AND NEW.namespace='certificate-issuer' AND
       NOT EXISTS (SELECT 1 FROM users WHERE id=NEW.actor_id AND role='platform-admin') THEN
        RAISE EXCEPTION 'certificate issuer mutation requires platform-admin' USING ERRCODE='42501';
    END IF;
    IF NEW.receipt_kind IN ('auto-deploy-policy','configuration-profile') AND
       (NEW.request_id !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$' OR
        NEW.idempotency_key !~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$') THEN
        RAISE EXCEPTION 'mutation receipt identifier is invalid' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='build-api' AND
       (NEW.namespace NOT IN ('definition.create','definition.delete','definition.build','attempt.cancel','attempt.retry') OR
        length(NEW.idempotency_key) NOT BETWEEN 16 AND 128) THEN
        RAISE EXCEPTION 'build API mutation receipt is invalid' USING ERRCODE='23514';
    END IF;
    IF NEW.receipt_kind='secret-binding' AND
       (NEW.namespace NOT IN ('create','rotate','delete') OR NEW.idempotency_key !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$') THEN
        RAISE EXCEPTION 'secret binding mutation receipt is invalid' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$_$;


--
-- Name: validate_registry_runtime_maintenance_target(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_registry_runtime_maintenance_target() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    target_mode text;
    plan_target uuid;
BEGIN
    SELECT mode INTO target_mode FROM registry_targets WHERE id=NEW.registry_target_id;
    SELECT registry_target_id INTO plan_target FROM registry_cleanup_plans WHERE id=NEW.plan_id;
    IF target_mode IS DISTINCT FROM 'managed' OR plan_target IS DISTINCT FROM NEW.registry_target_id THEN
        RAISE EXCEPTION 'Registry maintenance is restricted to its managed target and cleanup plan'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_runtime_readiness(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_runtime_readiness() RETURNS trigger
    LANGUAGE plpgsql
    AS $_$
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
           NOT (NEW.identity ?& ARRAY['githubAppId','builderNamespace','builderAgentImage']) OR
           NEW.observation - ARRAY['builderCapacityReady'] <> '{}'::jsonb OR NOT (NEW.observation ? 'builderCapacityReady') OR
           jsonb_typeof(NEW.observation->'builderCapacityReady')<>'boolean' OR
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
        IF NEW.identity - ARRAY['githubAppId','argoNamespace','rootApplicationName','repositorySecretName',
                'chartRepository','chartName','chartVersion','chartDigest','rendererImage','chartDigestEnforcement'] <> '{}'::jsonb OR
           NOT (NEW.identity ?& ARRAY['githubAppId','argoNamespace','rootApplicationName','repositorySecretName',
                'chartRepository','chartName','chartVersion','chartDigest','rendererImage','chartDigestEnforcement']) OR
           NEW.identity->>'githubAppId' !~ '^[1-9][0-9]*$' OR
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
        WHERE id=NEW.platform_binding_id;
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
$_$;


--
-- Name: validate_runtime_registry_pull_artifact(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_runtime_registry_pull_artifact() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    durable_namespace text;
    durable_pull_ref text;
BEGIN
    SELECT namespace INTO durable_namespace
      FROM environments WHERE id=NEW.environment_id FOR KEY SHARE;
    SELECT pull_credential_ref INTO durable_pull_ref
      FROM registry_targets WHERE id=NEW.registry_target_id FOR KEY SHARE;
    IF durable_namespace IS DISTINCT FROM NEW.namespace OR
       durable_pull_ref IS NULL OR durable_pull_ref='' OR
       durable_pull_ref IS DISTINCT FROM NEW.pull_credential_ref THEN
        RAISE EXCEPTION 'Runtime registry pull artifact scope does not match durable metadata'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.environment_id,NEW.namespace,NEW.registry_target_id,
               NEW.pull_credential_ref,NEW.profile_name,NEW.profile_revision,
               NEW.secret_name,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.environment_id,OLD.namespace,OLD.registry_target_id,
               OLD.pull_credential_ref,OLD.profile_name,OLD.profile_revision,
               OLD.secret_name,OLD.created_at) THEN
            RAISE EXCEPTION 'Runtime registry pull artifact identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Runtime registry pull artifact epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) AND
           NEW.lease_owner IS NOT NULL THEN
            RAISE EXCEPTION 'Runtime registry pull artifact lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.runtime_state='failed' AND NEW.runtime_state<>'failed' THEN
            IF OLD.last_failure_code<>'profile-mismatch' OR
               OLD.lease_owner IS NULL OR OLD.worker_contract<>'registry-pull.v1' OR
               OLD.worker_config_digest IS NULL OR NEW.runtime_state<>'ready' OR
               NEW.lease_owner IS NOT NULL OR NEW.worker_contract IS NOT NULL OR
               NEW.worker_config_digest IS NOT NULL OR NEW.lease_epoch<>OLD.lease_epoch OR
               NEW.last_failure_code<>'' OR NEW.consecutive_failures<>0 OR
               NEW.last_observed_at IS NULL OR NEW.observed_uid='' OR
               NEW.observed_resource_version='' THEN
                RAISE EXCEPTION 'Failed runtime registry pull artifacts are terminal'
                    USING ERRCODE='23514';
            END IF;
        END IF;
        IF NEW.updated_at<OLD.updated_at OR
           (OLD.last_observed_at IS NOT NULL AND NEW.last_observed_at IS NOT NULL AND
            NEW.last_observed_at<OLD.last_observed_at) THEN
            RAISE EXCEPTION 'Runtime registry pull artifact time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_secret_binding_runtime_reconciliation(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_secret_binding_runtime_reconciliation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    durable_provider text;
BEGIN
    SELECT provider INTO durable_provider
      FROM secret_binding_versions
     WHERE id=NEW.version_id AND binding_id=NEW.binding_id;
    IF durable_provider IS DISTINCT FROM 'sealed-secrets' THEN
        RAISE EXCEPTION 'Runtime reconciliation requires an exact SealedSecret version'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' THEN
        IF ROW(NEW.version_id,NEW.binding_id,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.version_id,OLD.binding_id,OLD.created_at) THEN
            RAISE EXCEPTION 'Runtime reconciliation identity is immutable'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch<OLD.lease_epoch OR NEW.lease_epoch>OLD.lease_epoch+1 THEN
            RAISE EXCEPTION 'Runtime reconciliation epoch is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.lease_epoch=OLD.lease_epoch AND OLD.lease_owner IS NOT NULL AND
           ROW(NEW.lease_owner,NEW.worker_contract,NEW.worker_config_digest)
           IS DISTINCT FROM
           ROW(OLD.lease_owner,OLD.worker_contract,OLD.worker_config_digest) AND
           NEW.lease_owner IS NOT NULL THEN
            RAISE EXCEPTION 'Runtime reconciliation lease identity changed without a new epoch'
                USING ERRCODE='23514';
        END IF;
        IF OLD.runtime_state<>'awaiting' AND NEW.runtime_state<>OLD.runtime_state THEN
            RAISE EXCEPTION 'Runtime reconciliation terminal state is immutable'
                USING ERRCODE='23514';
        END IF;
        IF OLD.runtime_state='awaiting' AND NEW.runtime_state NOT IN ('awaiting','ready','failed') THEN
            RAISE EXCEPTION 'Runtime reconciliation transition is invalid'
                USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at THEN
            RAISE EXCEPTION 'Runtime reconciliation time cannot regress'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: validate_tls_certificate_runtime_readiness(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_tls_certificate_runtime_readiness() RETURNS trigger
    LANGUAGE plpgsql
    AS $_$
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
$_$;


--
-- Name: validate_tls_certificate_version(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.validate_tls_certificate_version() RETURNS trigger
    LANGUAGE plpgsql
    AS $_$
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
$_$;


--
-- Name: access_grants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.access_grants (
    id uuid NOT NULL,
    subject_user_id uuid,
    role text NOT NULL,
    scope_type text NOT NULL,
    scope_id text NOT NULL,
    permissions text[] DEFAULT ARRAY[]::text[] NOT NULL,
    source text DEFAULT 'explicit'::text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    subject_team_id uuid,
    CONSTRAINT access_grants_check CHECK (((role <> 'organization-admin'::text) OR (scope_type = 'team'::text))),
    CONSTRAINT access_grants_check1 CHECK (((role <> 'project-admin'::text) OR (scope_type = 'project'::text))),
    CONSTRAINT access_grants_check2 CHECK (((role = 'platform-admin'::text) = ((scope_type = 'platform'::text) AND (scope_id = 'platform'::text)))),
    CONSTRAINT access_grants_check3 CHECK (((scope_type <> 'platform'::text) OR ((role = 'platform-admin'::text) AND (scope_id = 'platform'::text)))),
    CONSTRAINT access_grants_check4 CHECK (((source = 'bootstrap'::text) = (role = 'platform-admin'::text))),
    CONSTRAINT access_grants_check5 CHECK ((((scope_type = 'platform'::text) AND (scope_id = 'platform'::text)) OR ((scope_type = 'namespace'::text) AND (scope_id ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'::text)) OR ((scope_type = ANY (ARRAY['team'::text, 'project'::text, 'environment'::text, 'application'::text])) AND (scope_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'::text)))),
    CONSTRAINT access_grants_exactly_one_subject_check CHECK (((subject_user_id IS NOT NULL) <> (subject_team_id IS NOT NULL))),
    CONSTRAINT access_grants_permissions_check CHECK ((permissions <@ ARRAY['logs.read'::text])),
    CONSTRAINT access_grants_permissions_check1 CHECK ((cardinality(permissions) <= 1)),
    CONSTRAINT access_grants_role_check CHECK ((role = ANY (ARRAY['viewer'::text, 'developer'::text, 'project-admin'::text, 'organization-admin'::text, 'platform-admin'::text]))),
    CONSTRAINT access_grants_scope_id_check CHECK (((length(scope_id) >= 1) AND (length(scope_id) <= 253))),
    CONSTRAINT access_grants_scope_type_check CHECK ((scope_type = ANY (ARRAY['platform'::text, 'team'::text, 'project'::text, 'environment'::text, 'namespace'::text, 'application'::text]))),
    CONSTRAINT access_grants_source_check CHECK ((source = ANY (ARRAY['explicit'::text, 'bootstrap'::text, 'service-account'::text])))
);


--
-- Name: applications; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.applications (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    source_kind text DEFAULT 'oci'::text NOT NULL,
    registry_pull_mode text,
    registry_pull_project_credential_id uuid,
    registry_pull_updated_by uuid,
    registry_pull_updated_at timestamp with time zone,
    build_generation bigint DEFAULT 0 NOT NULL,
    build_source_id uuid,
    build_source_kind text,
    build_source_installation_id uuid,
    build_source_repository_id uuid,
    build_source_git_ssh jsonb,
    build_source_registry_target_id uuid,
    build_source_trigger_ref text,
    build_source_spec jsonb,
    build_source_digest text,
    build_source_revision bigint,
    build_source_created_at timestamp with time zone,
    build_source_updated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT applications_build_generation_check CHECK ((build_generation >= 0)),
    CONSTRAINT applications_build_source_check CHECK (((build_source_id IS NULL) AND (build_source_kind IS NULL) AND (build_source_installation_id IS NULL) AND (build_source_repository_id IS NULL) AND (build_source_git_ssh IS NULL) AND (build_source_registry_target_id IS NULL) AND (build_source_trigger_ref IS NULL) AND (build_source_spec IS NULL) AND (build_source_digest IS NULL) AND (build_source_revision IS NULL) AND (build_source_created_at IS NULL) AND (build_source_updated_at IS NULL)) OR ((build_source_id IS NOT NULL) AND (build_source_kind IS NOT NULL) AND (build_source_registry_target_id IS NOT NULL) AND (build_source_trigger_ref IS NOT NULL) AND (build_source_spec IS NOT NULL) AND (build_source_digest IS NOT NULL) AND (build_source_revision IS NOT NULL) AND (build_source_created_at IS NOT NULL) AND (build_source_updated_at IS NOT NULL) AND (jsonb_typeof(build_source_spec) = 'object'::text) AND (build_source_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND (build_source_revision > 0) AND (build_source_updated_at >= build_source_created_at) AND (((build_source_kind = 'github'::text) AND (source_kind = 'github'::text) AND (build_source_installation_id IS NOT NULL) AND (build_source_repository_id IS NOT NULL) AND (build_source_git_ssh IS NULL)) OR ((build_source_kind = 'git_ssh'::text) AND (source_kind = 'git-ssh'::text) AND (build_source_installation_id IS NULL) AND (build_source_repository_id IS NULL) AND (jsonb_typeof(build_source_git_ssh) = 'object'::text))))),
    CONSTRAINT applications_build_source_kind_check CHECK ((build_source_kind = ANY (ARRAY['github'::text, 'git_ssh'::text]))),
    CONSTRAINT applications_source_kind_check CHECK ((source_kind = ANY (ARRAY['oci'::text, 'github'::text, 'git-ssh'::text, 'helm'::text]))),
    CONSTRAINT applications_registry_pull_check CHECK ((((registry_pull_mode IS NULL) AND (registry_pull_project_credential_id IS NULL) AND (registry_pull_updated_by IS NULL) AND (registry_pull_updated_at IS NULL)) OR ((registry_pull_mode = 'public'::text) AND (registry_pull_project_credential_id IS NULL) AND (registry_pull_updated_by IS NOT NULL) AND (registry_pull_updated_at IS NOT NULL)) OR ((registry_pull_mode = 'project-credential'::text) AND (registry_pull_project_credential_id IS NOT NULL) AND (registry_pull_updated_by IS NOT NULL) AND (registry_pull_updated_at IS NOT NULL)))),
    CONSTRAINT applications_registry_pull_mode_check CHECK ((registry_pull_mode = ANY (ARRAY['public'::text, 'project-credential'::text]))),
    CONSTRAINT applications_registry_pull_update_check CHECK (((registry_pull_updated_at IS NULL) = (registry_pull_updated_by IS NULL)))
);


--
-- Name: argo_application_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.argo_application_observations (
    application_id uuid NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    argo_uid uuid NOT NULL,
    argo_namespace text NOT NULL,
    argo_name text NOT NULL,
    destination_namespace text NOT NULL,
    desired_revision text NOT NULL,
    observed_revision text NOT NULL,
    sync_status text NOT NULL,
    health_status text NOT NULL,
    operation_phase text NOT NULL,
    message text DEFAULT ''::text NOT NULL,
    resources jsonb NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    deployment_id uuid NOT NULL,
    CONSTRAINT argo_application_observations_check CHECK ((updated_at >= observed_at)),
    CONSTRAINT argo_application_observations_desired_revision_check CHECK ((desired_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_application_observations_health_status_check CHECK ((health_status = ANY (ARRAY['unknown'::text, 'progressing'::text, 'healthy'::text, 'degraded'::text, 'suspended'::text, 'missing'::text]))),
    CONSTRAINT argo_application_observations_message_check CHECK ((length(message) <= 1024)),
    CONSTRAINT argo_application_observations_observed_revision_check CHECK ((observed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_application_observations_operation_phase_check CHECK ((operation_phase = ANY (ARRAY[''::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'error'::text, 'terminating'::text]))),
    CONSTRAINT argo_application_observations_resources_check CHECK (
CASE
    WHEN (jsonb_typeof(resources) = 'array'::text) THEN (jsonb_array_length(resources) <= 512)
    ELSE false
END),
    CONSTRAINT argo_application_observations_sync_status_check CHECK ((sync_status = ANY (ARRAY['unknown'::text, 'synced'::text, 'out-of-sync'::text])))
);


--
-- Name: argo_desired_state_commands; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.argo_desired_state_commands (
    id uuid NOT NULL,
    generation bigint NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    platform_target_ref text NOT NULL,
    environment_target_ref text NOT NULL,
    environment_revision text NOT NULL,
    environment_generation bigint NOT NULL,
    path text NOT NULL,
    argo_namespace text NOT NULL,
    destination_namespace text NOT NULL,
    argo_project text NOT NULL,
    base_revision text NOT NULL,
    write_base_revision text DEFAULT ''::text NOT NULL,
    write_base_observed_at timestamp with time zone,
    precondition text NOT NULL,
    expected_etag text DEFAULT ''::text NOT NULL,
    catalog_digest text NOT NULL,
    chart_repository text NOT NULL,
    chart_name text NOT NULL,
    chart_version text NOT NULL,
    chart_digest text NOT NULL,
    renderer_image text NOT NULL,
    chart_digest_enforcement text NOT NULL,
    content bytea NOT NULL,
    content_sha256 text NOT NULL,
    message text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    committed_revision text DEFAULT ''::text NOT NULL,
    committed_at timestamp with time zone,
    verified_at timestamp with time zone,
    next_attempt_at timestamp with time zone NOT NULL,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_failure_code text DEFAULT ''::text NOT NULL,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    worker_contract text,
    worker_config_digest text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    policy_digest text,
    app_project_content bytea,
    CONSTRAINT argo_desired_state_commands_argo_namespace_check CHECK ((argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'::text)),
    CONSTRAINT argo_desired_state_commands_argo_project_check CHECK ((argo_project ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'::text)),
    CONSTRAINT argo_desired_state_commands_base_revision_check CHECK ((base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_desired_state_commands_catalog_digest_check CHECK ((catalog_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_desired_state_commands_chart_digest_check CHECK ((chart_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_desired_state_commands_chart_digest_enforcement_check CHECK ((chart_digest_enforcement = ANY (ARRAY['unavailable'::text, 'native-oci-digest-v1'::text]))),
    CONSTRAINT argo_desired_state_commands_chart_name_check CHECK ((chart_name = 'kuberploy-runtime'::text)),
    CONSTRAINT argo_desired_state_commands_chart_repository_check CHECK (((length(chart_repository) >= 7) AND (length(chart_repository) <= 512) AND (chart_repository ~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$'::text))),
    CONSTRAINT argo_desired_state_commands_chart_version_check CHECK (((length(chart_version) >= 5) AND (length(chart_version) <= 64) AND (chart_version ~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'::text))),
    CONSTRAINT argo_desired_state_commands_check CHECK ((path = (('platform/argocd/environments/'::text || (environment_id)::text) || '.yaml'::text))),
    CONSTRAINT argo_desired_state_commands_check1 CHECK ((((precondition = 'match-etag'::text) AND (expected_etag ~ '^"sha256:[0-9a-f]{64}"$'::text)) OR ((precondition = 'create-if-absent'::text) AND (expected_etag = ''::text)))),
    CONSTRAINT argo_desired_state_commands_check2 CHECK (((updated_at >= created_at) AND (next_attempt_at >= created_at))),
    CONSTRAINT argo_desired_state_commands_check3 CHECK ((((write_base_revision = ''::text) AND (write_base_observed_at IS NULL)) OR ((write_base_revision <> ''::text) AND (write_base_observed_at IS NOT NULL) AND (write_base_observed_at >= created_at) AND (write_base_observed_at <= updated_at)))),
    CONSTRAINT argo_desired_state_commands_check4 CHECK (((last_failure_code = ''::text) = (consecutive_failures = 0))),
    CONSTRAINT argo_desired_state_commands_check5 CHECK ((((lease_owner IS NULL) AND (lease_until IS NULL) AND (worker_contract IS NULL) AND (worker_config_digest IS NULL)) OR ((lease_owner IS NOT NULL) AND (lease_until IS NOT NULL) AND (worker_contract IS NOT NULL) AND (worker_config_digest IS NOT NULL) AND (lease_epoch > 0) AND (lease_until > updated_at)))),
    CONSTRAINT argo_desired_state_commands_check6 CHECK ((((state = 'pending'::text) AND (committed_revision = ''::text) AND (committed_at IS NULL) AND (verified_at IS NULL) AND (completed_at IS NULL) AND (lease_owner IS NULL)) OR ((state = 'claimed'::text) AND (committed_revision = ''::text) AND (committed_at IS NULL) AND (verified_at IS NULL) AND (completed_at IS NULL) AND (lease_owner IS NOT NULL)) OR ((state = 'git-committed'::text) AND (write_base_revision <> ''::text) AND (committed_revision <> ''::text) AND (committed_at IS NOT NULL) AND (committed_at >= created_at) AND (verified_at IS NULL) AND (completed_at IS NULL)) OR ((state = 'verified'::text) AND (write_base_revision <> ''::text) AND (committed_revision <> ''::text) AND (committed_at IS NOT NULL) AND (verified_at IS NOT NULL) AND (verified_at >= committed_at) AND (completed_at = verified_at) AND (lease_owner IS NULL)) OR ((state = ANY (ARRAY['blocked-prerequisite'::text, 'superseded'::text])) AND (write_base_revision = ''::text) AND (committed_revision = ''::text) AND (committed_at IS NULL) AND (verified_at IS NULL) AND (completed_at IS NOT NULL) AND (completed_at >= created_at) AND (lease_owner IS NULL)) OR ((state = 'failed'::text) AND (committed_revision = ''::text) AND (committed_at IS NULL) AND (verified_at IS NULL) AND (completed_at IS NOT NULL) AND (completed_at >= created_at) AND (lease_owner IS NULL)))),
    CONSTRAINT argo_desired_state_commands_check7 CHECK ((((chart_digest_enforcement = 'unavailable'::text) AND (state = 'blocked-prerequisite'::text)) OR ((chart_digest_enforcement = 'native-oci-digest-v1'::text) AND (state <> 'blocked-prerequisite'::text)))),
    CONSTRAINT argo_desired_state_commands_committed_revision_check CHECK (((committed_revision = ''::text) OR (committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))),
    CONSTRAINT argo_desired_state_commands_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 30))),
    CONSTRAINT argo_desired_state_commands_content_check CHECK (((octet_length(content) >= 1) AND (octet_length(content) <= 262144))),
    CONSTRAINT argo_desired_state_commands_content_sha256_check CHECK ((content_sha256 ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_desired_state_commands_destination_namespace_check CHECK ((destination_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'::text)),
    CONSTRAINT argo_desired_state_commands_environment_generation_check CHECK ((environment_generation > 0)),
    CONSTRAINT argo_desired_state_commands_environment_revision_check CHECK ((environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_desired_state_commands_generation_check CHECK ((generation > 0)),
    CONSTRAINT argo_desired_state_commands_last_failure_code_check CHECK (((last_failure_code = ''::text) OR (last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT argo_desired_state_commands_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT argo_desired_state_commands_lease_owner_check CHECK (((lease_owner IS NULL) OR ((length(lease_owner) >= 16) AND (length(lease_owner) <= 128) AND (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'::text)))),
    CONSTRAINT argo_desired_state_commands_message_check CHECK (((length(message) >= 1) AND (length(message) <= 512) AND (message !~ '[\x00\r]'::text))),
    CONSTRAINT argo_desired_state_commands_policy_digest_check CHECK (((policy_digest IS NULL) OR (policy_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT argo_desired_state_commands_precondition_check CHECK ((precondition = ANY (ARRAY['match-etag'::text, 'create-if-absent'::text]))),
    CONSTRAINT argo_desired_state_commands_renderer_image_check CHECK (((length(renderer_image) >= 10) AND (length(renderer_image) <= 512) AND (renderer_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT argo_desired_state_commands_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'claimed'::text, 'git-committed'::text, 'verified'::text, 'blocked-prerequisite'::text, 'failed'::text, 'superseded'::text]))),
    CONSTRAINT argo_desired_state_commands_worker_config_digest_check CHECK (((worker_config_digest IS NULL) OR (worker_config_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT argo_desired_state_commands_worker_contract_check CHECK (((worker_contract IS NULL) OR ((length(worker_contract) >= 8) AND (length(worker_contract) <= 64) AND (worker_contract ~ '^[a-z][a-z0-9.-]{7,63}$'::text)))),
    CONSTRAINT argo_desired_state_commands_write_base_revision_check CHECK (((write_base_revision = ''::text) OR (write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)))
);


--
-- Name: argo_desired_state_materialization_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.argo_desired_state_materialization_receipts (
    id uuid NOT NULL,
    environment_binding_id uuid CONSTRAINT argo_desired_state_materializat_environment_binding_id_not_null NOT NULL,
    environment_revision text CONSTRAINT argo_desired_state_materializatio_environment_revision_not_null NOT NULL,
    environment_generation bigint CONSTRAINT argo_desired_state_materializat_environment_generation_not_null NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid CONSTRAINT argo_desired_state_materialization_rece_environment_id_not_null NOT NULL,
    platform_binding_id uuid CONSTRAINT argo_desired_state_materialization_platform_binding_id_not_null NOT NULL,
    platform_target_ref text CONSTRAINT argo_desired_state_materialization_platform_target_ref_not_null NOT NULL,
    environment_target_ref text CONSTRAINT argo_desired_state_materializat_environment_target_ref_not_null NOT NULL,
    desired_state_command_id uuid CONSTRAINT argo_desired_state_materializ_desired_state_command_id_not_null NOT NULL,
    desired_state_generation bigint CONSTRAINT argo_desired_state_materializ_desired_state_generation_not_null NOT NULL,
    desired_state_revision text CONSTRAINT argo_desired_state_materializat_desired_state_revision_not_null NOT NULL,
    desired_state_content_sha256 text CONSTRAINT argo_desired_state_material_desired_state_content_sha2_not_null NOT NULL,
    catalog_digest text CONSTRAINT argo_desired_state_materialization_rece_catalog_digest_not_null NOT NULL,
    policy_digest text,
    chart_repository text CONSTRAINT argo_desired_state_materialization_re_chart_repository_not_null NOT NULL,
    chart_name text NOT NULL,
    chart_version text CONSTRAINT argo_desired_state_materialization_recei_chart_version_not_null NOT NULL,
    chart_digest text CONSTRAINT argo_desired_state_materialization_receip_chart_digest_not_null NOT NULL,
    renderer_image text CONSTRAINT argo_desired_state_materialization_rece_renderer_image_not_null NOT NULL,
    chart_digest_enforcement text CONSTRAINT argo_desired_state_materializ_chart_digest_enforcement_not_null NOT NULL,
    created_at timestamp with time zone NOT NULL,
    app_project_content bytea,
    CONSTRAINT argo_desired_state_materiali_desired_state_content_sha256_check CHECK ((desired_state_content_sha256 ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_desired_state_materializati_chart_digest_enforcement_check CHECK ((chart_digest_enforcement = 'native-oci-digest-v1'::text)),
    CONSTRAINT argo_desired_state_materializati_desired_state_generation_check CHECK ((desired_state_generation > 0)),
    CONSTRAINT argo_desired_state_materialization_desired_state_revision_check CHECK ((desired_state_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_desired_state_materialization_environment_generation_check CHECK ((environment_generation > 0)),
    CONSTRAINT argo_desired_state_materialization_environment_target_ref_check CHECK ((environment_target_ref ~ '^refs/heads/[A-Za-z0-9._/-]{1,240}$'::text)),
    CONSTRAINT argo_desired_state_materialization_r_environment_revision_check CHECK ((environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_desired_state_materialization_re_platform_target_ref_check CHECK ((platform_target_ref ~ '^refs/heads/[A-Za-z0-9._/-]{1,240}$'::text)),
    CONSTRAINT argo_desired_state_materialization_recei_chart_repository_check CHECK (((length(chart_repository) >= 7) AND (length(chart_repository) <= 512) AND (chart_repository ~ '^oci://[^/?#@[:space:]]+/[^?#@[:space:]]+$'::text))),
    CONSTRAINT argo_desired_state_materialization_receipt_catalog_digest_check CHECK ((catalog_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_desired_state_materialization_receipt_renderer_image_check CHECK (((length(renderer_image) >= 10) AND (length(renderer_image) <= 512) AND (renderer_image ~ '^[^[:space:]@]+@sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT argo_desired_state_materialization_receipts_chart_digest_check CHECK ((chart_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_desired_state_materialization_receipts_chart_name_check CHECK ((chart_name = 'kuberploy-runtime'::text)),
    CONSTRAINT argo_desired_state_materialization_receipts_chart_version_check CHECK (((length(chart_version) >= 5) AND (length(chart_version) <= 64) AND (chart_version ~ '^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$'::text))),
    CONSTRAINT argo_desired_state_materialization_receipts_policy_digest_check CHECK (((policy_digest IS NULL) OR (policy_digest ~ '^sha256:[0-9a-f]{64}$'::text)))
);


--
-- Name: argo_observation_runtime; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.argo_observation_runtime (
    argo_namespace text NOT NULL,
    lease_owner text DEFAULT ''::text NOT NULL,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    snapshot_resource_version text DEFAULT ''::text NOT NULL,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_failure_code text DEFAULT ''::text NOT NULL,
    next_poll_at timestamp with time zone NOT NULL,
    last_completed_at timestamp with time zone,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT argo_observation_runtime_argo_namespace_check CHECK ((argo_namespace ~ '^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$'::text)),
    CONSTRAINT argo_observation_runtime_check CHECK ((((lease_owner = ''::text) AND (lease_until IS NULL)) OR ((lease_owner <> ''::text) AND (lease_epoch > 0) AND (lease_until IS NOT NULL)))),
    CONSTRAINT argo_observation_runtime_check1 CHECK ((((consecutive_failures = 0) AND (last_failure_code = ''::text)) OR ((consecutive_failures > 0) AND (last_failure_code <> ''::text)))),
    CONSTRAINT argo_observation_runtime_check2 CHECK (((last_completed_at IS NULL) OR (last_completed_at <= updated_at))),
    CONSTRAINT argo_observation_runtime_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 32))),
    CONSTRAINT argo_observation_runtime_last_failure_code_check CHECK (((last_failure_code = ''::text) OR (last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT argo_observation_runtime_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT argo_observation_runtime_lease_owner_check CHECK (((lease_owner = ''::text) OR (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$'::text))),
    CONSTRAINT argo_observation_runtime_snapshot_resource_version_check CHECK (((length(snapshot_resource_version) <= 128) AND (POSITION((chr(10)) IN (snapshot_resource_version)) = 0) AND (POSITION((chr(13)) IN (snapshot_resource_version)) = 0)))
);


--
-- Name: argo_rollback_commands; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.argo_rollback_commands (
    id uuid NOT NULL,
    application_id uuid NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    operation_id uuid NOT NULL,
    base_revision text NOT NULL,
    expected_etag text NOT NULL,
    release_repository text NOT NULL,
    release_digest text NOT NULL,
    path text NOT NULL,
    candidate bytea NOT NULL,
    candidate_sha256 text NOT NULL,
    commit_message text NOT NULL,
    state text NOT NULL,
    git_revision text,
    failure_code text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT argo_rollback_commands_base_revision_check CHECK ((base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_rollback_commands_candidate_check CHECK (((octet_length(candidate) >= 1) AND (octet_length(candidate) <= 262144))),
    CONSTRAINT argo_rollback_commands_candidate_sha256_check CHECK ((candidate_sha256 ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_rollback_commands_check CHECK ((((state = 'git-committed'::text) AND (git_revision IS NOT NULL) AND (failure_code = ''::text)) OR ((state = 'failed'::text) AND (git_revision IS NULL) AND (failure_code <> ''::text)) OR ((state = 'pending-git'::text) AND (git_revision IS NULL) AND (failure_code = ''::text)))),
    CONSTRAINT argo_rollback_commands_check1 CHECK ((updated_at >= created_at)),
    CONSTRAINT argo_rollback_commands_commit_message_check CHECK (((length(commit_message) >= 1) AND (length(commit_message) <= 512))),
    CONSTRAINT argo_rollback_commands_expected_etag_check CHECK ((expected_etag ~ '^"sha256:[0-9a-f]{64}"$'::text)),
    CONSTRAINT argo_rollback_commands_git_revision_check CHECK ((git_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT argo_rollback_commands_path_check CHECK (((path !~ '(^/|/\.\.?(/|$)|//|\\)'::text) AND ((length(path) >= 1) AND (length(path) <= 1024)))),
    CONSTRAINT argo_rollback_commands_release_digest_check CHECK ((release_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT argo_rollback_commands_state_check CHECK ((state = ANY (ARRAY['pending-git'::text, 'git-committed'::text, 'failed'::text])))
);


--
-- Name: audit_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_events (
    id uuid NOT NULL,
    actor_id uuid NOT NULL,
    action text NOT NULL,
    target_type text NOT NULL,
    target_id uuid NOT NULL,
    request_id text NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT audit_events_detail_check CHECK (((jsonb_typeof(detail) = 'object'::text) AND (octet_length((detail)::text) <= 65536)))
);


--
-- Name: auto_deploy_policies; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auto_deploy_policies (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    application_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    current_revision bigint NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT auto_deploy_policies_current_revision_check CHECK ((current_revision > 0))
);


--
-- Name: auto_deploy_policy_revisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auto_deploy_policy_revisions (
    policy_id uuid NOT NULL,
    revision bigint NOT NULL,
    enabled boolean NOT NULL,
    source_deployment_id uuid NOT NULL,
    source_deployment_generation bigint CONSTRAINT auto_deploy_policy_revision_source_deployment_generati_not_null NOT NULL,
    source_config_etag text NOT NULL,
    config_intent bytea NOT NULL,
    template_digest text NOT NULL,
    service_actor_id uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT auto_deploy_policy_revisions_config_intent_check CHECK (((octet_length(config_intent) >= 2) AND (octet_length(config_intent) <= 262144))),
    CONSTRAINT auto_deploy_policy_revisions_config_intent_check1 CHECK ((jsonb_typeof((convert_from(config_intent, 'UTF8'::name))::jsonb) = 'object'::text)),
    CONSTRAINT auto_deploy_policy_revisions_revision_check CHECK ((revision > 0)),
    CONSTRAINT auto_deploy_policy_revisions_source_config_etag_check CHECK ((source_config_etag ~ '^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$'::text)),
    CONSTRAINT auto_deploy_policy_revisions_source_deployment_generation_check CHECK ((source_deployment_generation > 0)),
    CONSTRAINT auto_deploy_policy_revisions_template_digest_check CHECK ((template_digest ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: auto_deploy_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auto_deploy_runs (
    attempt_id uuid NOT NULL,
    policy_id uuid NOT NULL,
    policy_revision bigint NOT NULL,
    definition_id uuid NOT NULL,
    definition_digest text NOT NULL,
    release_id uuid NOT NULL,
    template_digest text NOT NULL,
    source_deployment_id uuid NOT NULL,
    source_deployment_generation bigint NOT NULL,
    source_config_etag text NOT NULL,
    idempotency_key text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempts bigint DEFAULT 0 NOT NULL,
    available_at timestamp with time zone NOT NULL,
    lease_owner text,
    lease_until timestamp with time zone,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    operation_id uuid,
    deployment_id uuid,
    failure_code text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT auto_deploy_runs_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT auto_deploy_runs_check CHECK (((lease_owner IS NULL) = (lease_until IS NULL))),
    CONSTRAINT auto_deploy_runs_check1 CHECK (((state = 'processing'::text) = (lease_owner IS NOT NULL))),
    CONSTRAINT auto_deploy_runs_check2 CHECK (((state = ANY (ARRAY['submitted'::text, 'failed'::text])) = (completed_at IS NOT NULL))),
    CONSTRAINT auto_deploy_runs_check3 CHECK ((((state = 'submitted'::text) AND (operation_id IS NOT NULL) AND (deployment_id IS NOT NULL) AND (failure_code = ''::text)) OR ((state = 'failed'::text) AND (operation_id IS NULL) AND (deployment_id IS NULL) AND (failure_code <> ''::text)) OR ((state = ANY (ARRAY['pending'::text, 'processing'::text])) AND (operation_id IS NULL) AND (deployment_id IS NULL)))),
    CONSTRAINT auto_deploy_runs_check4 CHECK (((completed_at IS NULL) OR (completed_at >= created_at))),
    CONSTRAINT auto_deploy_runs_check5 CHECK ((updated_at >= created_at)),
    CONSTRAINT auto_deploy_runs_check6 CHECK (((lease_until IS NULL) OR (lease_until > updated_at))),
    CONSTRAINT auto_deploy_runs_definition_digest_check CHECK ((definition_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT auto_deploy_runs_failure_code_check CHECK (((failure_code = ''::text) OR (failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT auto_deploy_runs_idempotency_key_check CHECK ((idempotency_key ~ '^auto-deploy/[0-9a-f-]{36}/[1-9][0-9]*/[0-9a-f-]{36}$'::text)),
    CONSTRAINT auto_deploy_runs_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT auto_deploy_runs_source_config_etag_check CHECK ((source_config_etag ~ '^"(?:sha256:|cfg-sha256-)[0-9a-f]{64}"$'::text)),
    CONSTRAINT auto_deploy_runs_source_deployment_generation_check CHECK ((source_deployment_generation > 0)),
    CONSTRAINT auto_deploy_runs_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'processing'::text, 'submitted'::text, 'failed'::text]))),
    CONSTRAINT auto_deploy_runs_template_digest_check CHECK ((template_digest ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: build_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.build_attempts (
    id uuid NOT NULL,
    definition_id uuid NOT NULL,
    delivery_claim_key text,
    trigger_kind text DEFAULT 'github_push'::text NOT NULL,
    trigger_key text NOT NULL,
    project_id uuid NOT NULL,
    service_id uuid NOT NULL,
    commit_sha text NOT NULL,
    git_ref text NOT NULL,
    generation bigint NOT NULL,
    definition_digest text NOT NULL,
    source_snapshot jsonb NOT NULL,
    plan_request jsonb NOT NULL,
    checkout_request jsonb NOT NULL,
    input_digest text NOT NULL,
    registry_mode text NOT NULL,
    state text NOT NULL,
    execution_attempts integer DEFAULT 0 NOT NULL,
    max_attempts integer NOT NULL,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    lease_owner text,
    lease_until timestamp with time zone,
    job_namespace text NOT NULL,
    job_name text NOT NULL,
    cache_candidate text NOT NULL,
    cache_reference text DEFAULT ''::text NOT NULL,
    result jsonb,
    log_reference text DEFAULT ''::text NOT NULL,
    failure_code text DEFAULT ''::text NOT NULL,
    cancel_requested_at timestamp with time zone,
    started_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT build_attempts_check CHECK (((jsonb_typeof(source_snapshot) = 'object'::text) AND (jsonb_typeof(plan_request) = 'object'::text) AND (jsonb_typeof(checkout_request) = 'object'::text))),
    CONSTRAINT build_attempts_check1 CHECK (((lease_owner IS NULL) = (lease_until IS NULL))),
    CONSTRAINT build_attempts_check2 CHECK ((((state = ANY (ARRAY['succeeded'::text, 'failed'::text, 'cancelled'::text])) AND (completed_at IS NOT NULL) AND (lease_owner IS NULL) AND (lease_until IS NULL)) OR ((state <> ALL (ARRAY['succeeded'::text, 'failed'::text, 'cancelled'::text])) AND (completed_at IS NULL)))),
    CONSTRAINT build_attempts_check3 CHECK ((((state = 'succeeded'::text) AND (result IS NOT NULL) AND (failure_code = ''::text)) OR (state <> 'succeeded'::text))),
    CONSTRAINT build_attempts_commit_sha_check CHECK ((commit_sha ~ '^[0-9a-f]{40}$'::text)),
    CONSTRAINT build_attempts_definition_digest_check CHECK ((definition_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT build_attempts_execution_attempts_check CHECK ((execution_attempts >= 0)),
    CONSTRAINT build_attempts_generation_check CHECK ((generation > 0)),
    CONSTRAINT build_attempts_input_digest_check CHECK ((input_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT build_attempts_max_attempts_check CHECK (((max_attempts >= 1) AND (max_attempts <= 5))),
    CONSTRAINT build_attempts_registry_mode_check CHECK ((registry_mode = ANY (ARRAY['managed'::text, 'external'::text]))),
    CONSTRAINT build_attempts_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'preparing'::text, 'running'::text, 'cancelling'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text]))),
    CONSTRAINT build_attempts_trigger_check CHECK ((((trigger_kind = 'github_push'::text) AND (delivery_claim_key IS NOT NULL) AND (trigger_key = delivery_claim_key)) OR ((trigger_kind = ANY (ARRAY['manual'::text, 'retry'::text])) AND (trigger_key <> ''::text)))),
    CONSTRAINT build_attempts_trigger_kind_check CHECK ((trigger_kind = ANY (ARRAY['github_push'::text, 'manual'::text, 'retry'::text])))
);


--
-- Name: build_release_projections; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.build_release_projections (
    attempt_id uuid NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    lease_owner text,
    lease_until timestamp with time zone,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    failure_code text DEFAULT ''::text NOT NULL,
    release_id uuid,
    cache_generation_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT build_release_projections_attempts_check CHECK (((attempts >= 0) AND (attempts <= 20))),
    CONSTRAINT build_release_projections_check CHECK (((lease_owner IS NULL) = (lease_until IS NULL))),
    CONSTRAINT build_release_projections_check1 CHECK (((state = 'processing'::text) = (lease_owner IS NOT NULL))),
    CONSTRAINT build_release_projections_check2 CHECK (((state = ANY (ARRAY['succeeded'::text, 'failed'::text])) = (completed_at IS NOT NULL))),
    CONSTRAINT build_release_projections_check3 CHECK (((state = 'succeeded'::text) OR (release_id IS NULL))),
    CONSTRAINT build_release_projections_check4 CHECK (((state <> 'succeeded'::text) OR (failure_code = ''::text))),
    CONSTRAINT build_release_projections_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT build_release_projections_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'processing'::text, 'succeeded'::text, 'failed'::text])))
);


--
-- Name: builder_platform_setting_mutations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.builder_platform_setting_mutations (
    actor_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    request_fingerprint text NOT NULL,
    revision bigint NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT builder_platform_setting_mutations_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 128) AND (btrim(idempotency_key) = idempotency_key))),
    CONSTRAINT builder_platform_setting_mutations_request_fingerprint_check CHECK ((request_fingerprint ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT builder_platform_setting_mutations_revision_check CHECK ((revision > 0))
);


--
-- Name: builder_platform_settings_revisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.builder_platform_settings_revisions (
    revision bigint NOT NULL,
    node_isolation boolean DEFAULT false NOT NULL,
    max_concurrent_builders integer DEFAULT 1 NOT NULL,
    checkout_resources jsonb NOT NULL,
    dind_resources jsonb NOT NULL,
    agent_resources jsonb NOT NULL,
    updated_by uuid NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT builder_platform_settings_revisions_revision_check CHECK ((revision > 0)),
    CONSTRAINT builder_platform_settings_revisions_max_concurrent_builders_check CHECK (((max_concurrent_builders >= 1) AND (max_concurrent_builders <= 20))),
    CONSTRAINT builder_platform_settings_revisions_resources_check CHECK (((jsonb_typeof(checkout_resources) = 'object'::text) AND (jsonb_typeof(dind_resources) = 'object'::text) AND (jsonb_typeof(agent_resources) = 'object'::text)))
);


--
-- Name: cert_manager_issuer_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cert_manager_issuer_observations (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    state text NOT NULL,
    observed_spec_digest text,
    observed_generation bigint,
    reason text DEFAULT ''::text NOT NULL,
    observed_at timestamp with time zone,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT cert_manager_issuer_observations_check CHECK ((((state = 'pending'::text) AND (observed_spec_digest IS NULL) AND (observed_generation IS NULL) AND (observed_at IS NULL)) OR ((state = ANY (ARRAY['ready'::text, 'degraded'::text])) AND (observed_spec_digest IS NOT NULL) AND (observed_generation IS NOT NULL) AND (observed_at IS NOT NULL)))),
    CONSTRAINT cert_manager_issuer_observations_observed_generation_check CHECK (((observed_generation IS NULL) OR (observed_generation > 0))),
    CONSTRAINT cert_manager_issuer_observations_observed_spec_digest_check CHECK (((observed_spec_digest IS NULL) OR (observed_spec_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT cert_manager_issuer_observations_reason_check CHECK ((octet_length(reason) <= 1024)),
    CONSTRAINT cert_manager_issuer_observations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'ready'::text, 'degraded'::text])))
);


--
-- Name: cert_manager_issuer_references; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cert_manager_issuer_references (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    application_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    git_path text NOT NULL,
    hostname text NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT cert_manager_issuer_references_git_path_check CHECK (((git_path <> ''::text) AND (octet_length(git_path) <= 1024))),
    CONSTRAINT cert_manager_issuer_references_hostname_check CHECK (((hostname <> ''::text) AND (octet_length(hostname) <= 253)))
);


--
-- Name: configuration_profile_assignments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.configuration_profile_assignments (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    profile_kind text NOT NULL,
    ordinal integer NOT NULL,
    scope_type text NOT NULL,
    scope_id uuid NOT NULL,
    CONSTRAINT configuration_profile_assignments_ordinal_check CHECK ((ordinal >= 0)),
    CONSTRAINT configuration_profile_assignments_profile_kind_check CHECK ((profile_kind = ANY (ARRAY['scheduling'::text, 'middleware'::text]))),
    CONSTRAINT configuration_profile_assignments_scope_type_check CHECK ((scope_type = ANY (ARRAY['team'::text, 'project'::text, 'environment'::text, 'application'::text])))
);


--
-- Name: configuration_profile_revisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.configuration_profile_revisions (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    profile_kind text NOT NULL,
    solver_type text,
    spec jsonb NOT NULL,
    spec_digest text NOT NULL,
    assignments_digest text,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    cloned_from_profile_id uuid,
    cloned_from_revision bigint,
    CONSTRAINT configuration_profile_revisions_assignments_digest_check CHECK (((assignments_digest IS NULL) OR (assignments_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT configuration_profile_revisions_check CHECK (((cloned_from_profile_id IS NULL) = (cloned_from_revision IS NULL))),
    CONSTRAINT configuration_profile_revisions_profile_kind_check CHECK ((profile_kind = ANY (ARRAY['scheduling'::text, 'middleware'::text, 'certificate-issuer'::text]))),
    CONSTRAINT configuration_profile_revisions_revision_check CHECK ((revision > 0)),
    CONSTRAINT configuration_profile_revisions_spec_check CHECK (((jsonb_typeof(spec) = 'object'::text) AND (octet_length((spec)::text) <= 65536))),
    CONSTRAINT configuration_profile_revisions_spec_digest_check CHECK ((spec_digest ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: configuration_profiles; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.configuration_profiles (
    id uuid NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    lifecycle text NOT NULL,
    current_revision bigint NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    deactivated_by uuid,
    deactivated_at timestamp with time zone,
    CONSTRAINT configuration_profiles_check CHECK ((((lifecycle = 'active'::text) AND (deactivated_by IS NULL) AND (deactivated_at IS NULL)) OR ((lifecycle = 'deactivated'::text) AND (deactivated_by IS NOT NULL) AND (deactivated_at IS NOT NULL)))),
    CONSTRAINT configuration_profiles_current_revision_check CHECK ((current_revision > 0)),
    CONSTRAINT configuration_profiles_kind_check CHECK ((kind = ANY (ARRAY['scheduling'::text, 'middleware'::text, 'certificate-issuer'::text]))),
    CONSTRAINT configuration_profiles_lifecycle_check CHECK ((lifecycle = ANY (ARRAY['active'::text, 'deactivated'::text]))),
    CONSTRAINT configuration_profiles_name_check CHECK ((name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text))
);


--
-- Name: deployment_operation_inputs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deployment_operation_inputs (
    operation_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    image text NOT NULL,
    replicas integer NOT NULL,
    port integer NOT NULL,
    environment jsonb DEFAULT '{}'::jsonb NOT NULL,
    route jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    runtime jsonb NOT NULL,
    config_raw bytea,
    CONSTRAINT deployment_operation_inputs_config_size CHECK (((config_raw IS NULL) OR ((octet_length(config_raw) >= 1) AND (octet_length(config_raw) <= 262144)))),
    CONSTRAINT deployment_operation_inputs_port_check CHECK (((port >= 1) AND (port <= 65535))),
    CONSTRAINT deployment_operation_inputs_replicas_check CHECK (((replicas >= 1) AND (replicas <= 100))),
    CONSTRAINT deployment_operation_inputs_runtime_object CHECK ((jsonb_typeof(runtime) = 'object'::text))
);


--
-- Name: deployments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deployments (
    id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    image text NOT NULL,
    replicas integer NOT NULL,
    port integer NOT NULL,
    environment jsonb DEFAULT '{}'::jsonb NOT NULL,
    route jsonb,
    state text NOT NULL,
    operation_id uuid NOT NULL,
    desired_revision text DEFAULT ''::text NOT NULL,
    observed_revision text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    runtime jsonb NOT NULL,
    config_raw bytea,
    config_etag text DEFAULT ''::text NOT NULL,
    config_version bigint DEFAULT 0 NOT NULL,
    CONSTRAINT deployments_config_projection_complete CHECK ((((config_raw IS NULL) AND (config_etag = ''::text) AND (config_version = 0)) OR ((octet_length(config_raw) >= 1) AND (octet_length(config_raw) <= 262144) AND (config_etag <> ''::text) AND (config_version > 0)))),
    CONSTRAINT deployments_port_check CHECK (((port >= 1) AND (port <= 65535))),
    CONSTRAINT deployments_replicas_check CHECK (((replicas >= 1) AND (replicas <= 100))),
    CONSTRAINT deployments_runtime_object CHECK ((jsonb_typeof(runtime) = 'object'::text))
);


--
-- Name: edge_runtime_targets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.edge_runtime_targets (
    target_key text NOT NULL,
    profile_revision bigint NOT NULL,
    kind text NOT NULL,
    integration_id uuid,
    management_mode text NOT NULL,
    namespace text NOT NULL,
    profile_config_map text NOT NULL,
    external_txt_owner_id text DEFAULT ''::text NOT NULL,
    external_policy text DEFAULT ''::text NOT NULL,
    external_domains text DEFAULT ''::text NOT NULL,
    desired_digest text NOT NULL,
    runtime_config_digest text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    runtime_state text DEFAULT 'awaiting'::text NOT NULL,
    next_observation_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone,
    observed_identity_digest text DEFAULT ''::text NOT NULL,
    observed_resource_versions text DEFAULT ''::text NOT NULL,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_failure_code text DEFAULT ''::text NOT NULL,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    worker_contract text,
    worker_config_digest text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    external_provider_kind text DEFAULT ''::text NOT NULL,
    external_credential_secret_ref text DEFAULT ''::text NOT NULL,
    external_provider_config_ref text DEFAULT ''::text NOT NULL,
    external_egress_config_ref text DEFAULT ''::text NOT NULL,
    CONSTRAINT edge_runtime_external_dns_identity_v2 CHECK ((((kind <> 'external-dns'::text) AND (external_provider_kind = ''::text) AND (external_credential_secret_ref = ''::text) AND (external_provider_config_ref = ''::text) AND (external_egress_config_ref = ''::text)) OR ((kind = 'external-dns'::text) AND (external_provider_kind = ANY (ARRAY['aws'::text, 'azure'::text, 'cloudflare'::text, 'google'::text, 'rfc2136'::text])) AND (((management_mode = 'managed'::text) AND (external_credential_secret_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text) AND (external_provider_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text) AND (external_egress_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text)) OR ((management_mode = 'adopted'::text) AND (external_credential_secret_ref = ''::text) AND (external_provider_config_ref = ''::text) AND (external_egress_config_ref = ''::text)))))),
    CONSTRAINT edge_runtime_targets_check CHECK (((updated_at >= created_at) AND (next_observation_at >= created_at))),
    CONSTRAINT edge_runtime_targets_check1 CHECK ((((kind = 'traefik'::text) AND (target_key = 'traefik'::text) AND (integration_id IS NULL) AND (external_txt_owner_id = ''::text) AND (external_policy = ''::text) AND (external_domains = ''::text)) OR ((kind = 'cert-manager'::text) AND (target_key = 'cert-manager'::text) AND (integration_id IS NULL) AND (external_txt_owner_id = ''::text) AND (external_policy = ''::text) AND (external_domains = ''::text)) OR ((kind = 'external-dns'::text) AND (integration_id IS NOT NULL) AND (target_key = ('external-dns/'::text || (integration_id)::text)) AND (external_txt_owner_id ~ '^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$'::text) AND (external_policy = ANY (ARRAY['upsert-only'::text, 'sync'::text])) AND ((length(external_domains) >= 1) AND (length(external_domains) <= 16384)) AND (external_domains !~ '[[:space:][:cntrl:]]'::text)))),
    CONSTRAINT edge_runtime_targets_check2 CHECK ((((last_observed_at IS NULL) AND (observed_identity_digest = ''::text) AND (observed_resource_versions = ''::text)) OR ((last_observed_at IS NOT NULL) AND (observed_identity_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND (observed_resource_versions ~ '^sha256:[0-9a-f]{64}$'::text)))),
    CONSTRAINT edge_runtime_targets_check3 CHECK (((runtime_state <> 'ready'::text) OR (last_observed_at IS NOT NULL))),
    CONSTRAINT edge_runtime_targets_check4 CHECK (((last_failure_code = ''::text) = (consecutive_failures = 0))),
    CONSTRAINT edge_runtime_targets_check5 CHECK ((((lease_owner IS NULL) AND (lease_until IS NULL) AND (worker_contract IS NULL) AND (worker_config_digest IS NULL)) OR ((lease_owner IS NOT NULL) AND (lease_until IS NOT NULL) AND (worker_contract = 'edge-observer.v1'::text) AND (worker_config_digest = runtime_config_digest) AND (lease_epoch > 0) AND (lease_until > updated_at)))),
    CONSTRAINT edge_runtime_targets_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 30))),
    CONSTRAINT edge_runtime_targets_desired_digest_check CHECK ((desired_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT edge_runtime_targets_kind_check CHECK ((kind = ANY (ARRAY['traefik'::text, 'cert-manager'::text, 'external-dns'::text]))),
    CONSTRAINT edge_runtime_targets_last_failure_code_check CHECK (((last_failure_code = ''::text) OR (last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT edge_runtime_targets_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT edge_runtime_targets_lease_owner_check CHECK (((lease_owner IS NULL) OR ((length(lease_owner) >= 16) AND (length(lease_owner) <= 128) AND (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'::text)))),
    CONSTRAINT edge_runtime_targets_management_mode_check CHECK ((management_mode = ANY (ARRAY['managed'::text, 'adopted'::text]))),
    CONSTRAINT edge_runtime_targets_namespace_check CHECK (((length(namespace) >= 1) AND (length(namespace) <= 63) AND (namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text))),
    CONSTRAINT edge_runtime_targets_profile_config_map_check CHECK (((length(profile_config_map) >= 1) AND (length(profile_config_map) <= 253) AND (profile_config_map ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text))),
    CONSTRAINT edge_runtime_targets_profile_revision_check CHECK ((profile_revision > 0)),
    CONSTRAINT edge_runtime_targets_runtime_config_digest_check CHECK ((runtime_config_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT edge_runtime_targets_runtime_state_check CHECK ((runtime_state = ANY (ARRAY['awaiting'::text, 'ready'::text, 'failed'::text]))),
    CONSTRAINT edge_runtime_targets_target_key_check CHECK (((length(target_key) >= 7) AND (length(target_key) <= 64) AND (target_key ~ '^(traefik|cert-manager|external-dns/[0-9a-f-]{36})$'::text))),
    CONSTRAINT edge_runtime_targets_worker_config_digest_check CHECK (((worker_config_digest IS NULL) OR (worker_config_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT edge_runtime_targets_worker_contract_check CHECK (((worker_contract IS NULL) OR (worker_contract = 'edge-observer.v1'::text)))
);


--
-- Name: edge_sslip_ingress_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.edge_sslip_ingress_observations (
    target_key text NOT NULL,
    profile_revision bigint NOT NULL,
    desired_digest text NOT NULL,
    runtime_config_digest text NOT NULL,
    public_ipv4 inet NOT NULL,
    source_kind text NOT NULL,
    service_uid uuid NOT NULL,
    service_resource_version text CONSTRAINT edge_sslip_ingress_observatio_service_resource_version_not_null NOT NULL,
    worker_id text NOT NULL,
    lease_epoch bigint NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT edge_sslip_ingress_observations_check CHECK (((target_key = 'traefik'::text) AND (updated_at = observed_at) AND (observed_at >= created_at))),
    CONSTRAINT edge_sslip_ingress_observations_desired_digest_check CHECK ((desired_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT edge_sslip_ingress_observations_lease_epoch_check CHECK ((lease_epoch > 0)),
    CONSTRAINT edge_sslip_ingress_observations_profile_revision_check CHECK ((profile_revision > 0)),
    CONSTRAINT edge_sslip_ingress_observations_public_ipv4_check CHECK (((family(public_ipv4) = 4) AND (masklen(public_ipv4) = 32) AND (NOT (public_ipv4 <<= '0.0.0.0/8'::inet)) AND (NOT (public_ipv4 <<= '10.0.0.0/8'::inet)) AND (NOT (public_ipv4 <<= '100.64.0.0/10'::inet)) AND (NOT (public_ipv4 <<= '127.0.0.0/8'::inet)) AND (NOT (public_ipv4 <<= '169.254.0.0/16'::inet)) AND (NOT (public_ipv4 <<= '172.16.0.0/12'::inet)) AND (NOT (public_ipv4 <<= '192.0.0.0/24'::inet)) AND (NOT (public_ipv4 <<= '192.0.2.0/24'::inet)) AND (NOT (public_ipv4 <<= '192.88.99.0/24'::inet)) AND (NOT (public_ipv4 <<= '192.168.0.0/16'::inet)) AND (NOT (public_ipv4 <<= '198.18.0.0/15'::inet)) AND (NOT (public_ipv4 <<= '198.51.100.0/24'::inet)) AND (NOT (public_ipv4 <<= '203.0.113.0/24'::inet)) AND (NOT (public_ipv4 <<= '224.0.0.0/4'::inet)) AND (NOT (public_ipv4 <<= '240.0.0.0/4'::inet)))),
    CONSTRAINT edge_sslip_ingress_observations_runtime_config_digest_check CHECK ((runtime_config_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT edge_sslip_ingress_observations_service_resource_version_check CHECK (((length(service_resource_version) >= 1) AND (length(service_resource_version) <= 128) AND (service_resource_version ~ '^[A-Za-z0-9._:/+\-]+$'::text))),
    CONSTRAINT edge_sslip_ingress_observations_source_kind_check CHECK ((source_kind = ANY (ARRAY['service-ip'::text, 'verified-static-ip'::text]))),
    CONSTRAINT edge_sslip_ingress_observations_worker_id_check CHECK (((length(worker_id) >= 16) AND (length(worker_id) <= 128) AND (worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'::text)))
);


--
-- Name: environment_app_placements; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.environment_app_placements (
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    state text DEFAULT 'draft'::text NOT NULL,
    desired_state text DEFAULT 'stopped'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT environment_app_placements_state_check CHECK ((((state = 'draft'::text) AND (desired_state = 'stopped'::text)) OR ((state = 'active'::text) AND (desired_state = 'running'::text))))
);


--
-- Name: environment_foundation_intents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.environment_foundation_intents (
    id uuid NOT NULL,
    environment_id uuid NOT NULL,
    project_id uuid NOT NULL,
    namespace text NOT NULL,
    argo_project text NOT NULL,
    platform_binding_id uuid NOT NULL,
    target_ref text NOT NULL,
    planned_head_revision text NOT NULL,
    binding_generation bigint NOT NULL,
    profile_digest text NOT NULL,
    publisher_config_digest text NOT NULL,
    publisher_contract text NOT NULL,
    publisher_policy text NOT NULL,
    manifest_path text NOT NULL,
    manifest bytea NOT NULL,
    manifest_digest text NOT NULL,
    intent_digest text NOT NULL,
    commit_trailer text NOT NULL,
    state text NOT NULL,
    active boolean NOT NULL,
    next_attempt_at timestamp with time zone NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_failure_code text DEFAULT ''::text NOT NULL,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    write_base_revision text DEFAULT ''::text NOT NULL,
    write_base_observed_at timestamp with time zone,
    committed_revision text DEFAULT ''::text NOT NULL,
    committed_parent_revision text DEFAULT ''::text CONSTRAINT environment_foundation_inten_committed_parent_revision_not_null NOT NULL,
    provider_request text DEFAULT ''::text NOT NULL,
    published_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT environment_foundation_intents_argo_project_check CHECK ((argo_project ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text)),
    CONSTRAINT environment_foundation_intents_attempts_check CHECK (((attempts >= 0) AND (attempts <= 30))),
    CONSTRAINT environment_foundation_intents_binding_generation_check CHECK ((binding_generation > 0)),
    CONSTRAINT environment_foundation_intents_check CHECK ((manifest_path = (('platform/argocd/foundations/'::text || (environment_id)::text) || '.yaml'::text))),
    CONSTRAINT environment_foundation_intents_check1 CHECK ((commit_trailer = ('Kuberploy-Environment-Foundation-Intent: '::text || (id)::text))),
    CONSTRAINT environment_foundation_intents_check2 CHECK (((updated_at >= created_at) AND (next_attempt_at >= created_at))),
    CONSTRAINT environment_foundation_intents_check3 CHECK (((last_failure_code = ''::text) = (consecutive_failures = 0))),
    CONSTRAINT environment_foundation_intents_check4 CHECK (((write_base_revision = ''::text) = (write_base_observed_at IS NULL))),
    CONSTRAINT environment_foundation_intents_check5 CHECK (((write_base_observed_at IS NULL) OR ((write_base_observed_at >= created_at) AND (write_base_observed_at <= updated_at)))),
    CONSTRAINT environment_foundation_intents_check6 CHECK (((state = ANY (ARRAY['pending'::text, 'claimed'::text, 'ready'::text])) = active)),
    CONSTRAINT environment_foundation_intents_check7 CHECK ((((state = 'claimed'::text) AND (lease_owner IS NOT NULL) AND (lease_epoch > 0) AND (lease_until > updated_at) AND (attempts > 0)) OR ((state <> 'claimed'::text) AND (lease_owner IS NULL) AND (lease_until IS NULL)))),
    CONSTRAINT environment_foundation_intents_check8 CHECK ((((committed_revision = ''::text) AND (committed_parent_revision = ''::text) AND (provider_request = ''::text) AND (published_at IS NULL)) OR ((committed_revision <> ''::text) AND (committed_parent_revision = write_base_revision) AND (provider_request <> ''::text) AND (published_at IS NOT NULL)))),
    CONSTRAINT environment_foundation_intents_check9 CHECK ((((state = 'ready'::text) AND (committed_revision <> ''::text) AND (completed_at = published_at)) OR ((state = ANY (ARRAY['pending'::text, 'claimed'::text])) AND (committed_revision = ''::text) AND (completed_at IS NULL)) OR ((state = 'failed'::text) AND (committed_revision = ''::text) AND (completed_at IS NOT NULL) AND (consecutive_failures > 0)) OR ((state = 'superseded'::text) AND (completed_at IS NOT NULL)))),
    CONSTRAINT environment_foundation_intents_committed_parent_revision_check CHECK (((committed_parent_revision = ''::text) OR (committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))),
    CONSTRAINT environment_foundation_intents_committed_revision_check CHECK (((committed_revision = ''::text) OR (committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))),
    CONSTRAINT environment_foundation_intents_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 30))),
    CONSTRAINT environment_foundation_intents_intent_digest_check CHECK ((intent_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_foundation_intents_last_failure_code_check CHECK (((last_failure_code = ''::text) OR (last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT environment_foundation_intents_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT environment_foundation_intents_lease_owner_check CHECK (((lease_owner IS NULL) OR ((length(lease_owner) >= 16) AND (length(lease_owner) <= 128) AND (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'::text)))),
    CONSTRAINT environment_foundation_intents_manifest_check CHECK (((octet_length(manifest) >= 1) AND (octet_length(manifest) <= 262144))),
    CONSTRAINT environment_foundation_intents_manifest_digest_check CHECK ((manifest_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_foundation_intents_namespace_check CHECK ((namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text)),
    CONSTRAINT environment_foundation_intents_planned_head_revision_check CHECK ((planned_head_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT environment_foundation_intents_profile_digest_check CHECK ((profile_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_foundation_intents_provider_request_check CHECK (((length(provider_request) <= 256) AND (provider_request !~ '[[:cntrl:]]'::text))),
    CONSTRAINT environment_foundation_intents_publisher_config_digest_check CHECK ((publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT environment_foundation_intents_publisher_contract_check CHECK ((publisher_contract = 'environment-foundation-protected-git.v1'::text)),
    CONSTRAINT environment_foundation_intents_publisher_policy_check CHECK ((publisher_policy = 'platform-protected-git.v1'::text)),
    CONSTRAINT environment_foundation_intents_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'claimed'::text, 'ready'::text, 'failed'::text, 'superseded'::text]))),
    CONSTRAINT environment_foundation_intents_target_ref_check CHECK (((target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$'::text) AND (target_ref !~ '(\.\.|//)'::text))),
    CONSTRAINT environment_foundation_intents_write_base_revision_check CHECK (((write_base_revision = ''::text) OR (write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)))
);


--
-- Name: environments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.environments (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    namespace text NOT NULL,
    argo_project text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    protection_policy text DEFAULT 'protected'::text NOT NULL,
    CONSTRAINT environments_argo_project_check CHECK ((argo_project = namespace)),
    CONSTRAINT environments_protection_policy_check CHECK ((protection_policy = ANY (ARRAY['development'::text, 'protected'::text])))
);


--
-- Name: external_dns_integration_environments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.external_dns_integration_environments (
    integration_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL
);


--
-- Name: external_dns_integrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.external_dns_integrations (
    id uuid NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    mode text NOT NULL,
    provider_kind text NOT NULL,
    txt_owner_id text NOT NULL,
    allowed_domain_suffixes jsonb NOT NULL,
    sync_policy text DEFAULT 'upsert-only'::text NOT NULL,
    destructive_sync_confirmed boolean DEFAULT false NOT NULL,
    credential_secret_ref text,
    provider_config_ref text,
    egress_config_ref text,
    operator_profile_ref text,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    runtime_revision bigint DEFAULT 1 NOT NULL,
    lifecycle text DEFAULT 'active'::text NOT NULL,
    deactivated_by uuid,
    deactivated_at timestamp with time zone,
    protected_git_state text DEFAULT 'pending'::text NOT NULL,
    protected_git_revision bigint,
    protected_git_content_digest text DEFAULT ''::text NOT NULL,
    protected_git_commit text DEFAULT ''::text NOT NULL,
    protected_git_observed_at timestamp with time zone,
    CONSTRAINT external_dns_integrations_allowed_domain_suffixes_check CHECK (public.external_dns_domain_suffixes_valid(allowed_domain_suffixes)),
    CONSTRAINT external_dns_integrations_check CHECK ((updated_at >= created_at)),
    CONSTRAINT external_dns_integrations_check1 CHECK ((((sync_policy = 'upsert-only'::text) AND (NOT destructive_sync_confirmed)) OR ((sync_policy = 'sync'::text) AND destructive_sync_confirmed))),
    CONSTRAINT external_dns_integrations_check2 CHECK ((((mode = 'managed'::text) AND (credential_secret_ref IS NOT NULL) AND (provider_config_ref IS NOT NULL) AND (egress_config_ref IS NOT NULL) AND (credential_secret_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text) AND (provider_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text) AND (egress_config_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text) AND (operator_profile_ref IS NULL)) OR ((mode = 'adopted'::text) AND (operator_profile_ref IS NOT NULL) AND (credential_secret_ref IS NULL) AND (provider_config_ref IS NULL) AND (egress_config_ref IS NULL) AND (operator_profile_ref ~ '^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$'::text)))),
    CONSTRAINT external_dns_integrations_lifecycle_check CHECK ((lifecycle = ANY (ARRAY['active'::text, 'deactivated'::text]))),
    CONSTRAINT external_dns_integrations_mode_check CHECK ((mode = ANY (ARRAY['managed'::text, 'adopted'::text]))),
    CONSTRAINT external_dns_integrations_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100) AND (name = btrim(name)) AND (name !~ '[[:cntrl:]]'::text))),
    CONSTRAINT external_dns_integrations_protected_git_state_check CHECK ((protected_git_state = ANY (ARRAY['pending'::text, 'materialized'::text, 'dematerialized'::text]))),
    CONSTRAINT external_dns_integrations_provider_kind_check CHECK ((provider_kind = ANY (ARRAY['aws'::text, 'azure'::text, 'cloudflare'::text, 'google'::text, 'rfc2136'::text]))),
    CONSTRAINT external_dns_integrations_runtime_revision_check CHECK ((runtime_revision > 0)),
    CONSTRAINT external_dns_integrations_slug_check CHECK (((length(slug) >= 1) AND (length(slug) <= 63) AND (slug ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text))),
    CONSTRAINT external_dns_integrations_sync_policy_check CHECK ((sync_policy = ANY (ARRAY['upsert-only'::text, 'sync'::text]))),
    CONSTRAINT external_dns_integrations_txt_owner_id_check CHECK (((length(txt_owner_id) >= 1) AND (length(txt_owner_id) <= 128) AND (txt_owner_id ~ '^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$'::text))),
    CONSTRAINT external_dns_lifecycle_consistent CHECK ((((lifecycle = 'active'::text) AND (deactivated_by IS NULL) AND (deactivated_at IS NULL)) OR ((lifecycle = 'deactivated'::text) AND (deactivated_by IS NOT NULL) AND (deactivated_at IS NOT NULL)))),
    CONSTRAINT external_dns_protected_git_receipt CHECK ((((protected_git_state = 'pending'::text) AND (protected_git_revision IS NULL) AND (protected_git_content_digest = ''::text) AND (protected_git_commit = ''::text) AND (protected_git_observed_at IS NULL)) OR ((protected_git_state = 'materialized'::text) AND (protected_git_revision = runtime_revision) AND (protected_git_content_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND (protected_git_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text) AND (protected_git_observed_at IS NOT NULL)) OR ((protected_git_state = 'dematerialized'::text) AND (lifecycle = 'deactivated'::text) AND (protected_git_revision = runtime_revision) AND (protected_git_content_digest = ''::text) AND (protected_git_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text) AND (protected_git_observed_at IS NOT NULL))))
);


--
-- Name: git_path_reservations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_path_reservations (
    binding_id uuid NOT NULL,
    target_ref text NOT NULL,
    path text NOT NULL,
    operation_id uuid NOT NULL,
    owner text NOT NULL,
    base_revision text NOT NULL,
    committed_revision text,
    state text NOT NULL,
    lease_until timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT git_path_reservations_base_revision_check CHECK ((base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_path_reservations_check CHECK ((((state = 'candidate'::text) AND (lease_until IS NOT NULL) AND (committed_revision IS NULL)) OR ((state = 'committed-pending-index'::text) AND (lease_until IS NULL) AND (committed_revision IS NOT NULL)))),
    CONSTRAINT git_path_reservations_committed_revision_check CHECK ((committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_path_reservations_owner_check CHECK (((length(owner) >= 1) AND (length(owner) <= 128))),
    CONSTRAINT git_path_reservations_path_check CHECK (((path !~ '(^/|/\.\.?(/|$)|//|\\)'::text) AND ((length(path) >= 1) AND (length(path) <= 1024)))),
    CONSTRAINT git_path_reservations_state_check CHECK ((state = ANY (ARRAY['candidate'::text, 'committed-pending-index'::text])))
);


--
-- Name: git_projected_documents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_projected_documents (
    binding_id uuid NOT NULL,
    generation bigint NOT NULL,
    path text NOT NULL,
    application_id uuid,
    source_revision text NOT NULL,
    config_revision text NOT NULL,
    blob_id text NOT NULL,
    content_sha256 text NOT NULL,
    raw bytea NOT NULL,
    parsed jsonb,
    valid boolean NOT NULL,
    diagnostics jsonb NOT NULL,
    schema_version text NOT NULL,
    parser_version text NOT NULL,
    indexed_at timestamp with time zone NOT NULL,
    CONSTRAINT git_projected_documents_blob_id_check CHECK ((blob_id ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_projected_documents_check CHECK (
CASE
    WHEN (jsonb_typeof(diagnostics) = 'array'::text) THEN ((valid AND (jsonb_array_length(diagnostics) = 0)) OR ((NOT valid) AND (jsonb_array_length(diagnostics) > 0)))
    ELSE false
END),
    CONSTRAINT git_projected_documents_config_revision_check CHECK ((config_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_projected_documents_content_sha256_check CHECK ((content_sha256 ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT git_projected_documents_diagnostics_check CHECK (
CASE
    WHEN (jsonb_typeof(diagnostics) = 'array'::text) THEN (jsonb_array_length(diagnostics) <= 64)
    ELSE false
END),
    CONSTRAINT git_projected_documents_path_check CHECK (((path !~ '(^/|/\.\.?(/|$)|//|\\)'::text) AND ((length(path) >= 1) AND (length(path) <= 1024)))),
    CONSTRAINT git_projected_documents_raw_check CHECK (((octet_length(raw) >= 1) AND (octet_length(raw) <= 262144))),
    CONSTRAINT git_projected_documents_source_revision_check CHECK ((source_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))
);


--
-- Name: git_projection_generations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_projection_generations (
    binding_id uuid NOT NULL,
    generation bigint NOT NULL,
    head_revision text NOT NULL,
    parser_version text NOT NULL,
    state text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    activated_at timestamp with time zone,
    CONSTRAINT git_projection_generations_check CHECK ((((state = 'active'::text) AND (activated_at IS NOT NULL) AND (activated_at >= started_at)) OR ((state <> 'active'::text) AND (activated_at IS NULL)))),
    CONSTRAINT git_projection_generations_generation_check CHECK ((generation > 0)),
    CONSTRAINT git_projection_generations_head_revision_check CHECK ((head_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_projection_generations_parser_version_check CHECK (((length(parser_version) >= 1) AND (length(parser_version) <= 64))),
    CONSTRAINT git_projection_generations_state_check CHECK ((state = ANY (ARRAY['staging'::text, 'active'::text, 'failed'::text])))
);


--
-- Name: git_projection_push_wake_targets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_projection_push_wake_targets (
    delivery_hash text NOT NULL,
    binding_id uuid NOT NULL,
    wake_generation bigint NOT NULL,
    CONSTRAINT git_projection_push_wake_targets_wake_generation_check CHECK ((wake_generation > 0))
);


--
-- Name: git_projection_push_wakes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_projection_push_wakes (
    delivery_hash text NOT NULL,
    github_app_id bigint NOT NULL,
    installation_id bigint NOT NULL,
    repository_id bigint NOT NULL,
    target_ref text NOT NULL,
    after_commit text NOT NULL,
    received_at timestamp with time zone NOT NULL,
    CONSTRAINT git_projection_push_wakes_after_commit_check CHECK ((after_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_projection_push_wakes_delivery_hash_check CHECK ((delivery_hash ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT git_projection_push_wakes_github_app_id_check CHECK ((github_app_id > 0)),
    CONSTRAINT git_projection_push_wakes_installation_id_check CHECK ((installation_id > 0)),
    CONSTRAINT git_projection_push_wakes_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT git_projection_push_wakes_target_ref_check CHECK ((target_ref ~ '^refs/heads/'::text))
);


--
-- Name: git_pull_request_publications; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_pull_request_publications (
    operation_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    provider text NOT NULL,
    installation_id bigint NOT NULL,
    repository_id bigint NOT NULL,
    repository_owner text NOT NULL,
    repository_name text NOT NULL,
    target_ref text NOT NULL,
    base_revision text NOT NULL,
    write_base_revision text DEFAULT ''::text NOT NULL,
    candidate_ref text NOT NULL,
    candidate_revision text DEFAULT ''::text NOT NULL,
    pull_request_number bigint DEFAULT 0 NOT NULL,
    pull_request_url text DEFAULT ''::text NOT NULL,
    pull_request_state text DEFAULT ''::text NOT NULL,
    merge_revision text DEFAULT ''::text NOT NULL,
    target_revision text DEFAULT ''::text NOT NULL,
    state text NOT NULL,
    provider_observed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT git_pull_request_publications_base_revision_check CHECK ((base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_pull_request_publications_candidate_revision_check CHECK (((candidate_revision = ''::text) OR (candidate_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))),
    CONSTRAINT git_pull_request_publications_check CHECK ((candidate_ref = ('refs/heads/kuberploy/operations/'::text || (operation_id)::text))),
    CONSTRAINT git_pull_request_publications_check1 CHECK ((candidate_ref <> target_ref)),
    CONSTRAINT git_pull_request_publications_check2 CHECK ((updated_at >= created_at)),
    CONSTRAINT git_pull_request_publications_check3 CHECK (((provider_observed_at IS NULL) OR ((provider_observed_at >= created_at) AND (provider_observed_at <= updated_at)))),
    CONSTRAINT git_pull_request_publications_check4 CHECK ((((state = 'pending-candidate'::text) AND (write_base_revision = ''::text) AND (candidate_revision = ''::text) AND (pull_request_number = 0) AND (pull_request_url = ''::text) AND (pull_request_state = ''::text) AND (merge_revision = ''::text) AND (target_revision = ''::text) AND (provider_observed_at IS NULL)) OR ((state = 'write-base-ready'::text) AND (write_base_revision <> ''::text) AND (candidate_revision = ''::text) AND (pull_request_number = 0) AND (pull_request_url = ''::text) AND (pull_request_state = ''::text) AND (merge_revision = ''::text) AND (target_revision = ''::text) AND (provider_observed_at IS NULL)) OR ((state = 'candidate-ready'::text) AND (write_base_revision <> ''::text) AND (candidate_revision <> ''::text) AND (pull_request_number = 0) AND (pull_request_url = ''::text) AND (pull_request_state = ''::text) AND (merge_revision = ''::text) AND (target_revision = ''::text) AND (provider_observed_at IS NULL)) OR ((state = 'pull-request-open'::text) AND (write_base_revision <> ''::text) AND (candidate_revision <> ''::text) AND (pull_request_number > 0) AND (pull_request_state = 'open'::text) AND (pull_request_url = ((((('https://github.com/'::text || repository_owner) || '/'::text) || repository_name) || '/pull/'::text) || (pull_request_number)::text)) AND (merge_revision = ''::text) AND (target_revision = ''::text) AND (provider_observed_at IS NOT NULL)) OR ((state = 'pull-request-closed'::text) AND (write_base_revision <> ''::text) AND (candidate_revision <> ''::text) AND (pull_request_number > 0) AND (pull_request_state = 'closed'::text) AND (pull_request_url = ((((('https://github.com/'::text || repository_owner) || '/'::text) || repository_name) || '/pull/'::text) || (pull_request_number)::text)) AND (merge_revision = ''::text) AND (target_revision = ''::text) AND (provider_observed_at IS NOT NULL)) OR ((state = 'merge-pending'::text) AND (write_base_revision <> ''::text) AND (candidate_revision <> ''::text) AND (pull_request_number > 0) AND (pull_request_state = 'closed'::text) AND (pull_request_url = ((((('https://github.com/'::text || repository_owner) || '/'::text) || repository_name) || '/pull/'::text) || (pull_request_number)::text)) AND (merge_revision <> ''::text) AND (target_revision = ''::text) AND (provider_observed_at IS NOT NULL)) OR ((state = 'merge-verified'::text) AND (write_base_revision <> ''::text) AND (candidate_revision <> ''::text) AND (pull_request_number > 0) AND (pull_request_state = 'closed'::text) AND (pull_request_url = ((((('https://github.com/'::text || repository_owner) || '/'::text) || repository_name) || '/pull/'::text) || (pull_request_number)::text)) AND (merge_revision <> ''::text) AND (target_revision <> ''::text) AND (provider_observed_at IS NOT NULL)))),
    CONSTRAINT git_pull_request_publications_installation_id_check CHECK ((installation_id > 0)),
    CONSTRAINT git_pull_request_publications_merge_revision_check CHECK (((merge_revision = ''::text) OR (merge_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))),
    CONSTRAINT git_pull_request_publications_provider_check CHECK ((provider = 'github'::text)),
    CONSTRAINT git_pull_request_publications_pull_request_number_check CHECK ((pull_request_number >= 0)),
    CONSTRAINT git_pull_request_publications_pull_request_state_check CHECK ((pull_request_state = ANY (ARRAY[''::text, 'open'::text, 'closed'::text]))),
    CONSTRAINT git_pull_request_publications_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT git_pull_request_publications_repository_name_check CHECK (((repository_name ~ '^[A-Za-z0-9_.-]{1,100}$'::text) AND (repository_name <> ALL (ARRAY['.'::text, '..'::text])) AND (lower(repository_name) <> '.git'::text) AND (lower(repository_name) !~ '\.git$'::text))),
    CONSTRAINT git_pull_request_publications_repository_owner_check CHECK ((repository_owner ~ '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$'::text)),
    CONSTRAINT git_pull_request_publications_state_check CHECK ((state = ANY (ARRAY['pending-candidate'::text, 'write-base-ready'::text, 'candidate-ready'::text, 'pull-request-open'::text, 'pull-request-closed'::text, 'merge-pending'::text, 'merge-verified'::text]))),
    CONSTRAINT git_pull_request_publications_target_ref_check CHECK (((target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$'::text) AND (target_ref !~ '\.\.'::text) AND (target_ref !~ '//'::text) AND (target_ref !~ '/$'::text))),
    CONSTRAINT git_pull_request_publications_target_revision_check CHECK (((target_revision = ''::text) OR (target_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))),
    CONSTRAINT git_pull_request_publications_version_check CHECK ((version > 0)),
    CONSTRAINT git_pull_request_publications_write_base_revision_check CHECK (((write_base_revision = ''::text) OR (write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)))
);


--
-- Name: git_repository_bindings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_repository_bindings (
    id uuid NOT NULL,
    kind text NOT NULL,
    scope_id uuid NOT NULL,
    project_id uuid,
    environment_id uuid,
    provider text NOT NULL,
    installation_id bigint NOT NULL,
    repository_id bigint NOT NULL,
    repository_owner text NOT NULL,
    repository_name text NOT NULL,
    target_ref text NOT NULL,
    path_prefix text NOT NULL,
    credential_secret_name text NOT NULL,
    state text NOT NULL,
    target_head_revision text,
    indexed_revision text,
    projection_generation bigint DEFAULT 0 NOT NULL,
    parser_version text NOT NULL,
    target_head_observed_at timestamp with time zone,
    indexed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    credential_mode text DEFAULT 'legacy-secret'::text NOT NULL,
    CONSTRAINT git_repository_bindings_check CHECK ((((kind = 'environment'::text) AND (scope_id = environment_id) AND (project_id IS NOT NULL) AND (path_prefix = ((('tenants/'::text || (project_id)::text) || '/environments/'::text) || (environment_id)::text))) OR ((kind = 'platform'::text) AND (scope_id = id) AND (project_id IS NULL) AND (environment_id IS NULL) AND (path_prefix = 'platform'::text)))),
    CONSTRAINT git_repository_bindings_check1 CHECK ((((indexed_revision IS NULL) AND (projection_generation = 0) AND (indexed_at IS NULL)) OR ((indexed_revision IS NOT NULL) AND (projection_generation > 0) AND (indexed_at IS NOT NULL)))),
    CONSTRAINT git_repository_bindings_check2 CHECK (((target_head_revision IS NULL) = (target_head_observed_at IS NULL))),
    CONSTRAINT git_repository_bindings_check3 CHECK (((target_head_observed_at IS NULL) OR ((target_head_observed_at >= created_at) AND (target_head_observed_at <= updated_at)))),
    CONSTRAINT git_repository_bindings_check4 CHECK (((indexed_at IS NULL) OR ((indexed_at >= created_at) AND (indexed_at <= updated_at)))),
    CONSTRAINT git_repository_bindings_check5 CHECK (((state <> 'ready'::text) OR ((target_head_revision IS NOT NULL) AND (target_head_revision = indexed_revision)))),
    CONSTRAINT git_repository_bindings_check6 CHECK (((state <> ALL (ARRAY['indexing'::text, 'diverged'::text])) OR (target_head_revision IS NOT NULL))),
    CONSTRAINT git_repository_bindings_check7 CHECK ((updated_at >= created_at)),
    CONSTRAINT git_repository_bindings_credential_mode_check CHECK ((((credential_mode = 'legacy-secret'::text) AND (credential_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$'::text)) OR ((credential_mode = 'github-app'::text) AND (credential_secret_name = ''::text)))),
    CONSTRAINT git_repository_bindings_indexed_revision_check CHECK ((indexed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_repository_bindings_installation_id_check CHECK ((installation_id > 0)),
    CONSTRAINT git_repository_bindings_kind_check CHECK ((kind = ANY (ARRAY['platform'::text, 'environment'::text]))),
    CONSTRAINT git_repository_bindings_parser_version_check CHECK (((length(parser_version) >= 1) AND (length(parser_version) <= 64))),
    CONSTRAINT git_repository_bindings_path_prefix_check CHECK (((path_prefix !~ '(^/|/\.\.?(/|$)|//|\\)'::text) AND ((length(path_prefix) >= 1) AND (length(path_prefix) <= 1024)))),
    CONSTRAINT git_repository_bindings_projection_generation_check CHECK ((projection_generation >= 0)),
    CONSTRAINT git_repository_bindings_provider_check CHECK ((provider = 'github'::text)),
    CONSTRAINT git_repository_bindings_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT git_repository_bindings_repository_name_check CHECK (((repository_name ~ '^[A-Za-z0-9_.-]{1,100}$'::text) AND (repository_name <> ALL (ARRAY['.'::text, '..'::text])))),
    CONSTRAINT git_repository_bindings_repository_owner_check CHECK ((repository_owner ~ '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$'::text)),
    CONSTRAINT git_repository_bindings_state_check CHECK ((state = ANY (ARRAY['ready'::text, 'indexing'::text, 'waiting-for-git'::text, 'diverged'::text, 'missing-ref'::text]))),
    CONSTRAINT git_repository_bindings_target_head_revision_check CHECK ((target_head_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_repository_bindings_target_ref_check CHECK (((target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$'::text) AND (target_ref !~ '\.\.'::text) AND (target_ref !~ '//'::text)))
);


--
-- Name: git_safety_poll_cursors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_safety_poll_cursors (
    binding_id uuid NOT NULL,
    last_commit text,
    provider_cursor text DEFAULT ''::text NOT NULL,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    next_poll_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    reconciled_binding_updated_at timestamp with time zone,
    last_error_code text DEFAULT ''::text NOT NULL,
    wake_generation bigint DEFAULT 0 NOT NULL,
    reconciled_wake_generation bigint DEFAULT 0 NOT NULL,
    CONSTRAINT git_safety_poll_cursors_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 32))),
    CONSTRAINT git_safety_poll_cursors_last_commit_check CHECK ((last_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_safety_poll_cursors_provider_cursor_check CHECK ((length(provider_cursor) <= 512)),
    CONSTRAINT git_safety_poll_cursors_reconciled_wake_generation_check CHECK ((reconciled_wake_generation >= 0)),
    CONSTRAINT git_safety_poll_cursors_wake_generation_check CHECK ((wake_generation >= 0)),
    CONSTRAINT git_safety_poll_error_code CHECK (((last_error_code = ''::text) OR (last_error_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT git_safety_poll_lease_epoch_valid CHECK ((lease_epoch >= 0)),
    CONSTRAINT git_safety_poll_lease_shape CHECK ((((lease_owner IS NULL) AND (lease_until IS NULL)) OR ((lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$'::text) AND (lease_epoch > 0) AND (lease_until > updated_at)))),
    CONSTRAINT git_safety_poll_reconciled_time CHECK (((reconciled_binding_updated_at IS NULL) OR (reconciled_binding_updated_at <= updated_at))),
    CONSTRAINT git_safety_poll_wake_order CHECK ((reconciled_wake_generation <= wake_generation))
);


--
-- Name: git_ssh_key_mutation_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_ssh_key_mutation_receipts (
    actor_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    operation text NOT NULL,
    request_fingerprint text NOT NULL,
    scope text NOT NULL,
    owner_id uuid NOT NULL,
    key_revision bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT git_ssh_key_mutation_receipts_fingerprint_check CHECK ((request_fingerprint ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT git_ssh_key_mutation_receipts_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 128))),
    CONSTRAINT git_ssh_key_mutation_receipts_operation_check CHECK ((operation = ANY (ARRAY['create'::text, 'rotate'::text, 'revoke'::text]))),
    CONSTRAINT git_ssh_key_mutation_receipts_revision_check CHECK ((key_revision > 0)),
    CONSTRAINT git_ssh_key_mutation_receipts_scope_check CHECK ((scope = ANY (ARRAY['app'::text, 'project'::text])))
);


--
-- Name: git_ssh_key_revisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_ssh_key_revisions (
    id uuid NOT NULL,
    scope text NOT NULL,
    owner_id uuid NOT NULL,
    revision bigint NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    public_key text NOT NULL,
    fingerprint text NOT NULL,
    encryption_key_version text NOT NULL,
    private_key_ciphertext bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT git_ssh_key_revisions_ciphertext_check CHECK (((octet_length(private_key_ciphertext) >= 32) AND (octet_length(private_key_ciphertext) <= 65536))),
    CONSTRAINT git_ssh_key_revisions_fingerprint_check CHECK (((length(fingerprint) >= 50) AND (length(fingerprint) <= 80) AND (fingerprint ~ '^SHA256:[A-Za-z0-9+/]+$'::text))),
    CONSTRAINT git_ssh_key_revisions_key_version_check CHECK (((length(encryption_key_version) >= 1) AND (length(encryption_key_version) <= 64))),
    CONSTRAINT git_ssh_key_revisions_public_key_check CHECK (((length(public_key) >= 80) AND (length(public_key) <= 1024) AND (public_key ~ '^ssh-ed25519 [A-Za-z0-9+/=]+$'::text))),
    CONSTRAINT git_ssh_key_revisions_revision_check CHECK ((revision > 0)),
    CONSTRAINT git_ssh_key_revisions_revoked_at_check CHECK ((((status = 'active'::text) AND (revoked_at IS NULL)) OR ((status = 'revoked'::text) AND (revoked_at IS NOT NULL)))),
    CONSTRAINT git_ssh_key_revisions_scope_check CHECK ((scope = ANY (ARRAY['app'::text, 'project'::text]))),
    CONSTRAINT git_ssh_key_revisions_status_check CHECK ((status = ANY (ARRAY['active'::text, 'revoked'::text])))
);


--
-- Name: git_verified_head_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_verified_head_observations (
    binding_id uuid NOT NULL,
    provider text NOT NULL,
    installation_id bigint NOT NULL,
    repository_id bigint NOT NULL,
    repository_owner text NOT NULL,
    repository_name text NOT NULL,
    target_ref text NOT NULL,
    commit_revision text NOT NULL,
    source text NOT NULL,
    provider_request text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    CONSTRAINT git_verified_head_observations_commit_revision_check CHECK ((commit_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_verified_head_observations_installation_id_check CHECK ((installation_id > 0)),
    CONSTRAINT git_verified_head_observations_provider_check CHECK ((provider = 'github'::text)),
    CONSTRAINT git_verified_head_observations_provider_request_check CHECK (((length(provider_request) >= 1) AND (length(provider_request) <= 256))),
    CONSTRAINT git_verified_head_observations_repository_id_check CHECK ((repository_id > 0)),
    CONSTRAINT git_verified_head_observations_source_check CHECK ((source = ANY (ARRAY['verified-webhook'::text, 'safety-poll'::text, 'write-finalization'::text])))
);


--
-- Name: git_webhook_tombstones; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_webhook_tombstones (
    provider text NOT NULL,
    repository_id bigint NOT NULL,
    target_ref text NOT NULL,
    after_commit text NOT NULL,
    delivery_hash text NOT NULL,
    received_at timestamp with time zone NOT NULL,
    CONSTRAINT git_webhook_tombstones_after_commit_check CHECK ((after_commit ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_webhook_tombstones_delivery_hash_check CHECK ((delivery_hash ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT git_webhook_tombstones_provider_check CHECK ((provider = 'github'::text)),
    CONSTRAINT git_webhook_tombstones_repository_id_check CHECK ((repository_id > 0))
);


--
-- Name: git_write_commands; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_write_commands (
    operation_id uuid NOT NULL,
    command_kind text NOT NULL,
    deployment_id uuid,
    actor_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid,
    variable_scope text,
    target_ref text NOT NULL,
    path text NOT NULL,
    base_revision text NOT NULL,
    precondition text NOT NULL,
    expected_etag text DEFAULT ''::text NOT NULL,
    chart_identity text,
    policy_version text NOT NULL,
    content bytea NOT NULL,
    content_sha256 text NOT NULL,
    message text NOT NULL,
    action text DEFAULT 'upsert'::text NOT NULL,
    publication_mode text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    committed_revision text DEFAULT ''::text NOT NULL,
    committed_at timestamp with time zone,
    indexed_generation bigint DEFAULT 0 NOT NULL,
    indexed_at timestamp with time zone,
    request_digest text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT git_write_commands_base_revision_check CHECK ((base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text)),
    CONSTRAINT git_write_commands_check CHECK ((((precondition = 'match-etag'::text) AND (expected_etag ~ '^"sha256:[0-9a-f]{64}"$'::text)) OR ((precondition = 'create-if-absent'::text) AND (expected_etag = ''::text)))),
    CONSTRAINT git_write_commands_check1 CHECK ((updated_at >= created_at)),
    CONSTRAINT git_write_commands_check2 CHECK ((((state = 'pending'::text) AND (committed_revision = ''::text) AND (committed_at IS NULL) AND (indexed_generation = 0) AND (indexed_at IS NULL)) OR ((state = 'git-committed'::text) AND (committed_revision <> ''::text) AND (committed_at IS NOT NULL) AND (committed_at >= created_at) AND (indexed_generation = 0) AND (indexed_at IS NULL)) OR ((state = 'indexed'::text) AND (committed_revision <> ''::text) AND (committed_at IS NOT NULL) AND (committed_at >= created_at) AND (indexed_generation > 0) AND (indexed_at IS NOT NULL) AND (indexed_at >= committed_at)))),
    CONSTRAINT git_write_commands_check3 CHECK ((((command_kind = 'deployment'::text) AND (deployment_id IS NOT NULL) AND (application_id IS NOT NULL) AND (variable_scope IS NULL) AND (request_digest IS NULL) AND (path = (((((('tenants/'::text || (project_id)::text) || '/environments/'::text) || (environment_id)::text) || '/apps/'::text) || (application_id)::text) || '/app.yaml'::text)) AND (chart_identity ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'::text) AND ((octet_length(content) >= 1) AND (octet_length(content) <= 262144))) OR ((command_kind = 'variable-set'::text) AND (deployment_id IS NULL) AND (application_id IS NULL) AND (variable_scope = ANY (ARRAY['project'::text, 'environment'::text])) AND (chart_identity IS NULL) AND (request_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND ((octet_length(content) >= 1) AND (octet_length(content) <= 131072)) AND (((variable_scope = 'project'::text) AND (path = (('tenants/'::text || (project_id)::text) || '/variables.yaml'::text))) OR ((variable_scope = 'environment'::text) AND (path = (((('tenants/'::text || (project_id)::text) || '/environments/'::text) || (environment_id)::text) || '/variables.yaml'::text))))))),
    CONSTRAINT git_write_commands_command_kind_check CHECK ((command_kind = ANY (ARRAY['deployment'::text, 'variable-set'::text]))),
    CONSTRAINT git_write_commands_committed_revision_check CHECK (((committed_revision = ''::text) OR (committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text))),
    CONSTRAINT git_write_commands_content_sha256_check CHECK ((content_sha256 ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT git_write_commands_indexed_generation_check CHECK ((indexed_generation >= 0)),
    CONSTRAINT git_write_commands_message_check CHECK (((length(message) >= 1) AND (length(message) <= 512) AND (message !~ '[\x00\r]'::text))),
    CONSTRAINT git_write_commands_action_check CHECK ((action = 'upsert'::text OR (action = 'delete'::text AND command_kind = 'deployment'::text AND precondition = 'match-etag'::text))),
    CONSTRAINT git_write_commands_policy_version_check CHECK (((length(policy_version) >= 1) AND (length(policy_version) <= 128) AND (policy_version !~ '[\x00\r\n]'::text))),
    CONSTRAINT git_write_commands_precondition_check CHECK ((precondition = ANY (ARRAY['match-etag'::text, 'create-if-absent'::text]))),
    CONSTRAINT git_write_commands_publication_mode_check CHECK ((publication_mode = ANY (ARRAY['direct'::text, 'pull-request'::text]))),
    CONSTRAINT git_write_commands_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'git-committed'::text, 'indexed'::text])))
);


--
-- Name: github_installations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.github_installations (
    id uuid NOT NULL,
    github_installation_id bigint NOT NULL,
    account_login text NOT NULL,
    account_type text NOT NULL,
    owner_user_id uuid NOT NULL,
    visibility text DEFAULT 'private'::text NOT NULL,
    team_id uuid,
    repository_selection text NOT NULL,
    repository_count integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    github_app_id bigint,
    github_account_id bigint,
    lifecycle text DEFAULT 'pending-verification'::text NOT NULL,
    permissions jsonb DEFAULT '{}'::jsonb NOT NULL,
    suspended_at timestamp with time zone,
    deleted_at timestamp with time zone,
    last_verified_at timestamp with time zone,
    CONSTRAINT github_installations_account_id_check CHECK (((github_account_id IS NULL) OR (github_account_id > 0))),
    CONSTRAINT github_installations_account_type_check CHECK ((account_type = ANY (ARRAY['User'::text, 'Organization'::text]))),
    CONSTRAINT github_installations_app_id_check CHECK (((github_app_id IS NULL) OR (github_app_id > 0))),
    CONSTRAINT github_installations_check CHECK ((((visibility = 'private'::text) AND (team_id IS NULL)) OR ((visibility = 'team'::text) AND (team_id IS NOT NULL)))),
    CONSTRAINT github_installations_github_installation_id_check CHECK ((github_installation_id > 0)),
    CONSTRAINT github_installations_lifecycle_check CHECK ((lifecycle = ANY (ARRAY['pending-verification'::text, 'active'::text, 'suspended'::text, 'deleted'::text]))),
    CONSTRAINT github_installations_lifecycle_timestamp_check CHECK ((((lifecycle = 'suspended'::text) AND (suspended_at IS NOT NULL) AND (deleted_at IS NULL)) OR ((lifecycle = 'deleted'::text) AND (deleted_at IS NOT NULL)) OR ((lifecycle = ANY (ARRAY['pending-verification'::text, 'active'::text])) AND (suspended_at IS NULL) AND (deleted_at IS NULL)))),
    CONSTRAINT github_installations_permissions_object_check CHECK ((jsonb_typeof(permissions) = 'object'::text)),
    CONSTRAINT github_installations_repository_count_check CHECK ((repository_count >= 0)),
    CONSTRAINT github_installations_repository_selection_check CHECK ((repository_selection = ANY (ARRAY['all'::text, 'selected'::text]))),
    CONSTRAINT github_installations_verified_identity_check CHECK (((lifecycle = 'pending-verification'::text) OR ((github_app_id IS NOT NULL) AND (github_account_id IS NOT NULL) AND (last_verified_at IS NOT NULL)))),
    CONSTRAINT github_installations_visibility_check CHECK ((visibility = ANY (ARRAY['private'::text, 'team'::text])))
);


--
-- Name: github_one_time_claims; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.github_one_time_claims (
    kind text NOT NULL,
    claim_key text NOT NULL,
    retain_until timestamp with time zone NOT NULL,
    permanent boolean NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT github_one_time_claims_claim_key_check CHECK ((claim_key ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT github_one_time_claims_kind_check CHECK ((kind = ANY (ARRAY['github-state'::text, 'github-delivery'::text])))
);


--
-- Name: github_repositories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.github_repositories (
    id uuid NOT NULL,
    installation_id uuid NOT NULL,
    github_repository_id bigint NOT NULL,
    github_owner_id bigint NOT NULL,
    owner_login text NOT NULL,
    name text NOT NULL,
    lifecycle text NOT NULL,
    last_verified_at timestamp with time zone NOT NULL,
    removed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT github_repositories_check CHECK ((((lifecycle = 'active'::text) AND (removed_at IS NULL)) OR ((lifecycle = 'removed'::text) AND (removed_at IS NOT NULL)))),
    CONSTRAINT github_repositories_github_owner_id_check CHECK ((github_owner_id > 0)),
    CONSTRAINT github_repositories_github_repository_id_check CHECK ((github_repository_id > 0)),
    CONSTRAINT github_repositories_lifecycle_check CHECK ((lifecycle = ANY (ARRAY['active'::text, 'removed'::text])))
);


--
-- Name: github_setup_authorizations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.github_setup_authorizations (
    actor_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    request_fingerprint text NOT NULL,
    state_value text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT github_setup_authorizations_idempotency_key_check CHECK (((length(idempotency_key) >= 16) AND (length(idempotency_key) <= 128))),
    CONSTRAINT github_setup_authorizations_request_fingerprint_check CHECK ((request_fingerprint ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT github_setup_authorizations_state_value_check CHECK (((length(state_value) >= 64) AND (length(state_value) <= 4096)))
);


--
-- Name: github_setup_handoffs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.github_setup_handoffs (
    digest bytea NOT NULL,
    actor_id uuid NOT NULL,
    team_id uuid,
    github_user_id bigint NOT NULL,
    github_user_login text NOT NULL,
    installation jsonb NOT NULL,
    repositories jsonb NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    link_idempotency_key text,
    link_request_fingerprint text,
    linked_installation_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT github_setup_handoffs_check CHECK ((((consumed_at IS NULL) AND (link_idempotency_key IS NULL) AND (link_request_fingerprint IS NULL) AND (linked_installation_id IS NULL)) OR ((consumed_at IS NOT NULL) AND ((length(link_idempotency_key) >= 16) AND (length(link_idempotency_key) <= 128)) AND (link_request_fingerprint ~ '^sha256:[0-9a-f]{64}$'::text)))),
    CONSTRAINT github_setup_handoffs_digest_check CHECK ((octet_length(digest) = 32)),
    CONSTRAINT github_setup_handoffs_github_user_id_check CHECK ((github_user_id > 0)),
    CONSTRAINT github_setup_handoffs_installation_check CHECK ((jsonb_typeof(installation) = 'object'::text)),
    CONSTRAINT github_setup_handoffs_repositories_check CHECK ((jsonb_typeof(repositories) = 'array'::text))
);


--
-- Name: github_user_bindings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.github_user_bindings (
    user_id uuid NOT NULL,
    github_user_id bigint NOT NULL,
    github_login text NOT NULL,
    bound_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT github_user_bindings_github_login_check CHECK ((github_login ~ '^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$'::text)),
    CONSTRAINT github_user_bindings_github_user_id_check CHECK ((github_user_id > 0))
);


--
-- Name: github_webhook_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.github_webhook_receipts (
    claim_key text NOT NULL,
    claim_kind text DEFAULT 'github-delivery'::text NOT NULL,
    github_app_id bigint NOT NULL,
    github_installation_id bigint NOT NULL,
    delivery_id uuid NOT NULL,
    event text NOT NULL,
    body_sha256 text NOT NULL,
    typed_event jsonb,
    repository_id bigint,
    git_ref text DEFAULT ''::text NOT NULL,
    state text NOT NULL,
    failure_code text DEFAULT ''::text NOT NULL,
    lease_owner text,
    lease_until timestamp with time zone,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    received_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT github_webhook_receipts_body_sha256_check CHECK ((body_sha256 ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT github_webhook_receipts_check CHECK (((typed_event IS NOT NULL) OR (state = ANY (ARRAY['enqueued'::text, 'ignored'::text, 'failed'::text])))),
    CONSTRAINT github_webhook_receipts_check1 CHECK ((((event = 'push'::text) AND (repository_id IS NOT NULL) AND (git_ref <> ''::text)) OR ((event <> 'push'::text) AND (repository_id IS NULL) AND (git_ref = ''::text)))),
    CONSTRAINT github_webhook_receipts_check2 CHECK ((((state = ANY (ARRAY['enqueued'::text, 'ignored'::text, 'failed'::text])) AND (completed_at IS NOT NULL) AND (lease_owner IS NULL) AND (lease_until IS NULL)) OR ((state = ANY (ARRAY['claimed'::text, 'processing'::text])) AND (completed_at IS NULL)))),
    CONSTRAINT github_webhook_receipts_check3 CHECK (((lease_owner IS NULL) = (lease_until IS NULL))),
    CONSTRAINT github_webhook_receipts_claim_kind_check CHECK ((claim_kind = 'github-delivery'::text)),
    CONSTRAINT github_webhook_receipts_event_check CHECK ((event = ANY (ARRAY['push'::text, 'installation'::text, 'installation_repositories'::text]))),
    CONSTRAINT github_webhook_receipts_github_app_id_check CHECK ((github_app_id > 0)),
    CONSTRAINT github_webhook_receipts_github_installation_id_check CHECK ((github_installation_id > 0)),
    CONSTRAINT github_webhook_receipts_state_check CHECK ((state = ANY (ARRAY['claimed'::text, 'processing'::text, 'enqueued'::text, 'ignored'::text, 'failed'::text]))),
    CONSTRAINT github_webhook_receipts_typed_event_check CHECK (((typed_event IS NULL) OR (jsonb_typeof(typed_event) = 'object'::text)))
);


--
-- Name: middleware_profile_references; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.middleware_profile_references (
    profile_id uuid NOT NULL,
    revision bigint NOT NULL,
    application_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    git_path text NOT NULL,
    logical_name text NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT middleware_profile_references_git_path_check CHECK (((git_path <> ''::text) AND (octet_length(git_path) <= 1024))),
    CONSTRAINT middleware_profile_references_logical_name_check CHECK ((logical_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text))
);


--
-- Name: mutation_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mutation_receipts (
    actor_id uuid NOT NULL,
    receipt_kind text NOT NULL,
    namespace text NOT NULL,
    scope_key text NOT NULL,
    idempotency_key text NOT NULL,
    request_digest text,
    request_fingerprint bytea,
    resource_type text,
    resource_id uuid,
    operation_id uuid,
    profile_id uuid,
    auto_deploy_policy_id uuid,
    secret_binding_id uuid,
    secret_version_id uuid,
    action text,
    result_revision bigint,
    request_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT mutation_receipts_check CHECK ((((receipt_kind = 'resource'::text) AND (request_digest IS NOT NULL) AND (request_fingerprint IS NULL) AND (resource_type IS NOT NULL) AND (resource_id IS NOT NULL) AND (profile_id IS NULL) AND (auto_deploy_policy_id IS NULL) AND (secret_binding_id IS NULL) AND (secret_version_id IS NULL) AND (action IS NULL) AND (result_revision IS NULL) AND (request_id IS NULL) AND (scope_key = 'global'::text)) OR ((receipt_kind = 'build-api'::text) AND (request_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND (request_fingerprint IS NULL) AND (resource_type IS NULL) AND (resource_id IS NOT NULL) AND (operation_id IS NULL) AND (profile_id IS NULL) AND (auto_deploy_policy_id IS NULL) AND (secret_binding_id IS NULL) AND (secret_version_id IS NULL) AND (action IS NULL) AND (result_revision IS NULL) AND (request_id IS NULL) AND (scope_key <> 'global'::text)) OR ((receipt_kind = 'secret-binding'::text) AND (request_digest IS NULL) AND (octet_length(request_fingerprint) = 32) AND (resource_type IS NULL) AND (resource_id IS NULL) AND (operation_id IS NULL) AND (profile_id IS NULL) AND (auto_deploy_policy_id IS NULL) AND (secret_binding_id IS NOT NULL) AND (secret_version_id IS NOT NULL) AND (action IS NULL) AND (result_revision IS NULL) AND (request_id IS NULL) AND (scope_key <> 'global'::text)) OR ((receipt_kind = 'auto-deploy-policy'::text) AND (request_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND (request_fingerprint IS NULL) AND (resource_type IS NULL) AND (resource_id IS NULL) AND (operation_id IS NULL) AND (profile_id IS NULL) AND (auto_deploy_policy_id IS NOT NULL) AND (secret_binding_id IS NULL) AND (secret_version_id IS NULL) AND (action = ANY (ARRAY['create'::text, 'revise'::text])) AND (result_revision > 0) AND (request_id IS NOT NULL) AND (namespace = 'auto-deploy-policy'::text) AND (scope_key = 'global'::text)) OR ((receipt_kind = 'configuration-profile'::text) AND (request_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND (request_fingerprint IS NULL) AND (resource_type IS NULL) AND (resource_id IS NULL) AND (operation_id IS NULL) AND (profile_id IS NOT NULL) AND (auto_deploy_policy_id IS NULL) AND (secret_binding_id IS NULL) AND (secret_version_id IS NULL) AND (action = ANY (ARRAY['create'::text, 'revise'::text, 'clone'::text, 'deactivate'::text])) AND (result_revision > 0) AND (request_id IS NOT NULL) AND (namespace = ANY (ARRAY['scheduling'::text, 'middleware'::text, 'certificate-issuer'::text])) AND ((namespace = 'middleware'::text) OR (action <> 'clone'::text)) AND (scope_key = 'global'::text)))),
    CONSTRAINT mutation_receipts_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 256) AND (idempotency_key !~ '[[:cntrl:]]'::text))),
    CONSTRAINT mutation_receipts_namespace_check CHECK (((length(namespace) >= 1) AND (length(namespace) <= 128) AND (namespace !~ '[[:cntrl:]]'::text))),
    CONSTRAINT mutation_receipts_receipt_kind_check CHECK ((receipt_kind = ANY (ARRAY['resource'::text, 'build-api'::text, 'secret-binding'::text, 'auto-deploy-policy'::text, 'configuration-profile'::text]))),
    CONSTRAINT mutation_receipts_scope_key_check CHECK (((scope_key = 'global'::text) OR (scope_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'::text)))
);


--
-- Name: operations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.operations (
    id uuid NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    target_type text NOT NULL,
    target_id uuid NOT NULL,
    request_id text NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    progress jsonb DEFAULT '[]'::jsonb NOT NULL,
    git_revision text DEFAULT ''::text NOT NULL,
    problem jsonb,
    lease_owner text,
    lease_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    CONSTRAINT operations_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text, 'superseded'::text])))
);


--
-- Name: outbox; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outbox (
    operation_id uuid NOT NULL,
    kind text NOT NULL,
    scope_id uuid NOT NULL,
    generation bigint NOT NULL,
    trace_id text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    last_error text,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    published_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: outbox_valkey_dataset; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.outbox_valkey_dataset (
    singleton boolean DEFAULT true NOT NULL,
    dataset_id uuid NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT outbox_valkey_dataset_singleton_check CHECK (singleton)
);


--
-- Name: preview_authorities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.preview_authorities (
    token_hash bytea NOT NULL,
    preview_kind text NOT NULL,
    actor_id uuid NOT NULL,
    deployment_id uuid,
    binding_id uuid,
    project_id uuid,
    environment_id uuid,
    variable_scope text,
    path text,
    base_revision text,
    base_etag text NOT NULL,
    expected_etag text,
    policy_version text,
    chart_identity text,
    candidate_hash bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT preview_authorities_candidate_hash_check CHECK ((octet_length(candidate_hash) = 32)),
    CONSTRAINT preview_authorities_check CHECK (((expires_at > created_at) OR (preview_kind = 'deployment-config'::text))),
    CONSTRAINT preview_authorities_check1 CHECK (((consumed_at IS NULL) OR (consumed_at >= created_at))),
    CONSTRAINT preview_authorities_check2 CHECK ((((preview_kind = 'deployment-config'::text) AND (deployment_id IS NOT NULL) AND (project_id IS NULL) AND (environment_id IS NULL) AND (variable_scope IS NULL) AND (((binding_id IS NULL) AND (path IS NULL) AND (base_revision IS NULL) AND (expected_etag IS NULL) AND (policy_version IS NULL) AND (chart_identity IS NULL)) OR ((binding_id IS NOT NULL) AND (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text) AND (path IS NOT NULL) AND ((length(path) >= 1) AND (length(path) <= 1024)) AND (path !~ '(^/|/\.\.?(/|$)|//|\\)'::text) AND (expected_etag ~ '^"sha256:[0-9a-f]{64}"$'::text) AND (chart_identity ~ '^(?:sha256:[0-9a-f]{64}|[0-9]+\.[0-9]+\.[0-9]+(?:-rc\.[0-9]+)?)$'::text) AND ((length(policy_version) >= 1) AND (length(policy_version) <= 128)) AND (policy_version !~ '[\x00\r\n]'::text)))) OR ((preview_kind = 'variable-set'::text) AND (deployment_id IS NULL) AND (binding_id IS NOT NULL) AND (project_id IS NOT NULL) AND (environment_id IS NOT NULL) AND (variable_scope = ANY (ARRAY['project'::text, 'environment'::text])) AND (base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'::text) AND ((base_etag = ''::text) OR (base_etag ~ '^"sha256:[0-9a-f]{64}"$'::text)) AND (expected_etag IS NULL) AND (chart_identity IS NULL) AND ((length(policy_version) >= 1) AND (length(policy_version) <= 128)) AND (policy_version !~ '[\x00\r\n]'::text) AND (((variable_scope = 'project'::text) AND (path = (('tenants/'::text || (project_id)::text) || '/variables.yaml'::text))) OR ((variable_scope = 'environment'::text) AND (path = (((('tenants/'::text || (project_id)::text) || '/environments/'::text) || (environment_id)::text) || '/variables.yaml'::text))))))),
    CONSTRAINT preview_authorities_preview_kind_check CHECK ((preview_kind = ANY (ARRAY['deployment-config'::text, 'variable-set'::text]))),
    CONSTRAINT preview_authorities_token_hash_check CHECK ((octet_length(token_hash) = 32))
);


--
-- Name: project_registry_pull_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.project_registry_pull_credentials (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    registry_target_id uuid NOT NULL,
    name text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT project_registry_pull_credentials_name_check CHECK (((name ~ '^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}[A-Za-z0-9]$'::text) OR (name ~ '^[A-Za-z0-9]$'::text)))
);


--
-- Name: projects; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.projects (
    id uuid NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    team_id uuid
);


--
-- Name: registry_artifact_references; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_artifact_references (
    registry_target_id uuid NOT NULL,
    service_id text NOT NULL,
    repository text NOT NULL,
    digest text NOT NULL,
    kind text NOT NULL,
    reference_key text NOT NULL,
    source_revision text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    CONSTRAINT registry_artifact_references_digest_check CHECK ((digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT registry_artifact_references_kind_check CHECK ((kind = ANY (ARRAY['current-git-intent'::text, 'observed-running'::text, 'pin'::text, 'active-operation'::text]))),
    CONSTRAINT registry_artifact_references_reference_key_check CHECK ((reference_key <> ''::text)),
    CONSTRAINT registry_artifact_references_repository_check CHECK ((repository <> ''::text)),
    CONSTRAINT registry_artifact_references_service_id_check CHECK ((service_id <> ''::text))
);


--
-- Name: registry_authority_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_authority_observations (
    registry_target_id uuid NOT NULL,
    service_id text NOT NULL,
    authority text NOT NULL,
    revision text NOT NULL,
    complete boolean NOT NULL,
    snapshot_digest text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    CONSTRAINT registry_authority_observations_authority_check CHECK ((authority = ANY (ARRAY['git-intent'::text, 'runtime'::text, 'operations'::text]))),
    CONSTRAINT registry_authority_observations_revision_check CHECK ((revision <> ''::text)),
    CONSTRAINT registry_authority_observations_service_id_check CHECK ((service_id <> ''::text)),
    CONSTRAINT registry_authority_observations_snapshot_digest_check CHECK ((snapshot_digest ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: registry_blobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_blobs (
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    digest text NOT NULL,
    media_type text DEFAULT ''::text NOT NULL,
    size_bytes bigint NOT NULL,
    present boolean DEFAULT true NOT NULL,
    first_observed_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone NOT NULL,
    last_observation_revision bigint NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT registry_blobs_digest_check CHECK ((digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT registry_blobs_last_observation_revision_check CHECK ((last_observation_revision > 0)),
    CONSTRAINT registry_blobs_size_bytes_check CHECK ((size_bytes >= 0))
);


--
-- Name: registry_cache_generations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_cache_generations (
    id uuid NOT NULL,
    registry_target_id uuid NOT NULL,
    service_id text NOT NULL,
    repository text NOT NULL,
    platform_set text NOT NULL,
    trust_lane text NOT NULL,
    cache_schema text NOT NULL,
    build_definition_hash text NOT NULL,
    generation bigint NOT NULL,
    root_digest text NOT NULL,
    size_bytes bigint NOT NULL,
    state text NOT NULL,
    active_imports integer DEFAULT 0 NOT NULL,
    active_exports integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    last_used_at timestamp with time zone NOT NULL,
    CONSTRAINT registry_cache_generations_active_exports_check CHECK ((active_exports >= 0)),
    CONSTRAINT registry_cache_generations_active_imports_check CHECK ((active_imports >= 0)),
    CONSTRAINT registry_cache_generations_build_definition_hash_check CHECK ((build_definition_hash <> ''::text)),
    CONSTRAINT registry_cache_generations_cache_schema_check CHECK ((cache_schema <> ''::text)),
    CONSTRAINT registry_cache_generations_generation_check CHECK ((generation > 0)),
    CONSTRAINT registry_cache_generations_platform_set_check CHECK ((platform_set <> ''::text)),
    CONSTRAINT registry_cache_generations_repository_check CHECK ((repository <> ''::text)),
    CONSTRAINT registry_cache_generations_root_digest_check CHECK ((root_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT registry_cache_generations_service_id_check CHECK ((service_id <> ''::text)),
    CONSTRAINT registry_cache_generations_size_bytes_check CHECK ((size_bytes >= 0)),
    CONSTRAINT registry_cache_generations_state_check CHECK ((state = ANY (ARRAY['exporting'::text, 'succeeded'::text, 'failed'::text, 'deleted'::text, 'missing'::text]))),
    CONSTRAINT registry_cache_generations_trust_lane_check CHECK ((trust_lane <> ''::text))
);


--
-- Name: registry_catalog_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_catalog_observations (
    id uuid NOT NULL,
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    revision bigint NOT NULL,
    complete boolean NOT NULL,
    snapshot_digest text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    manifest_count integer NOT NULL,
    blob_count integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT registry_catalog_observations_blob_count_check CHECK ((blob_count >= 0)),
    CONSTRAINT registry_catalog_observations_manifest_count_check CHECK ((manifest_count >= 0)),
    CONSTRAINT registry_catalog_observations_revision_check CHECK ((revision > 0)),
    CONSTRAINT registry_catalog_observations_snapshot_digest_check CHECK ((snapshot_digest ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: registry_cleanup_items; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_cleanup_items (
    plan_id uuid NOT NULL,
    ordinal integer NOT NULL,
    repository text NOT NULL,
    resource_kind text NOT NULL,
    digest text NOT NULL,
    disposition text NOT NULL,
    action text NOT NULL,
    estimated_bytes bigint NOT NULL,
    reasons jsonb DEFAULT '[]'::jsonb NOT NULL,
    state text DEFAULT 'planned'::text NOT NULL,
    provider_message text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT registry_cleanup_items_action_check CHECK ((action = ANY (ARRAY['none'::text, 'delete-manifest'::text, 'garbage-collect-blob'::text]))),
    CONSTRAINT registry_cleanup_items_check CHECK ((((disposition = 'protect'::text) AND (action = 'none'::text) AND (state = ANY (ARRAY['planned'::text, 'protected'::text]))) OR ((disposition = 'delete'::text) AND (action <> 'none'::text) AND (state <> 'protected'::text)))),
    CONSTRAINT registry_cleanup_items_digest_check CHECK ((digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT registry_cleanup_items_disposition_check CHECK ((disposition = ANY (ARRAY['protect'::text, 'delete'::text]))),
    CONSTRAINT registry_cleanup_items_estimated_bytes_check CHECK ((estimated_bytes >= 0)),
    CONSTRAINT registry_cleanup_items_ordinal_check CHECK ((ordinal >= 0)),
    CONSTRAINT registry_cleanup_items_repository_check CHECK ((repository <> ''::text)),
    CONSTRAINT registry_cleanup_items_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['release-manifest'::text, 'cache-manifest'::text, 'blob'::text]))),
    CONSTRAINT registry_cleanup_items_state_check CHECK ((state = ANY (ARRAY['planned'::text, 'protected'::text, 'deleting'::text, 'deleted'::text, 'skipped'::text, 'failed'::text])))
);


--
-- Name: registry_cleanup_leases; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_cleanup_leases (
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    plan_id uuid NOT NULL,
    owner text NOT NULL,
    lease_until timestamp with time zone NOT NULL,
    CONSTRAINT registry_cleanup_leases_owner_check CHECK ((owner <> ''::text)),
    CONSTRAINT registry_cleanup_leases_repository_check CHECK ((repository <> ''::text))
);


--
-- Name: registry_cleanup_plans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_cleanup_plans (
    id uuid NOT NULL,
    registry_target_id uuid NOT NULL,
    service_id text NOT NULL,
    snapshot_token text NOT NULL,
    authority_token text NOT NULL,
    plan_digest text NOT NULL,
    state text DEFAULT 'preview'::text NOT NULL,
    policy jsonb NOT NULL,
    observations jsonb NOT NULL,
    summary jsonb NOT NULL,
    created_at timestamp with time zone NOT NULL,
    claimed_at timestamp with time zone,
    completed_at timestamp with time zone,
    failure text DEFAULT ''::text NOT NULL,
    CONSTRAINT registry_cleanup_plans_authority_token_check CHECK ((authority_token <> ''::text)),
    CONSTRAINT registry_cleanup_plans_plan_digest_check CHECK ((plan_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT registry_cleanup_plans_service_id_check CHECK ((service_id <> ''::text)),
    CONSTRAINT registry_cleanup_plans_snapshot_token_check CHECK ((snapshot_token <> ''::text)),
    CONSTRAINT registry_cleanup_plans_state_check CHECK ((state = ANY (ARRAY['preview'::text, 'executing'::text, 'succeeded'::text, 'failed'::text, 'superseded'::text])))
);


--
-- Name: registry_inventory_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_inventory_observations (
    registry_target_id uuid NOT NULL,
    revision text NOT NULL,
    complete boolean NOT NULL,
    repositories jsonb NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    CONSTRAINT registry_inventory_observations_repositories_check CHECK ((jsonb_typeof(repositories) = 'array'::text)),
    CONSTRAINT registry_inventory_observations_revision_check CHECK ((revision <> ''::text))
);


--
-- Name: registry_manifest_blobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_manifest_blobs (
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    manifest_digest text NOT NULL,
    blob_digest text NOT NULL
);


--
-- Name: registry_manifest_children; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_manifest_children (
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    parent_digest text NOT NULL,
    child_digest text NOT NULL,
    CONSTRAINT registry_manifest_children_check CHECK ((parent_digest <> child_digest))
);


--
-- Name: registry_manifests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_manifests (
    registry_target_id uuid NOT NULL,
    repository text NOT NULL,
    digest text NOT NULL,
    kind text NOT NULL,
    media_type text NOT NULL,
    size_bytes bigint NOT NULL,
    platform_os text DEFAULT ''::text NOT NULL,
    platform_architecture text DEFAULT ''::text NOT NULL,
    platform_variant text DEFAULT ''::text NOT NULL,
    present boolean DEFAULT true NOT NULL,
    first_observed_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone NOT NULL,
    last_observation_revision bigint NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT registry_manifests_digest_check CHECK ((digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT registry_manifests_kind_check CHECK ((kind = ANY (ARRAY['index'::text, 'manifest'::text]))),
    CONSTRAINT registry_manifests_last_observation_revision_check CHECK ((last_observation_revision > 0)),
    CONSTRAINT registry_manifests_size_bytes_check CHECK ((size_bytes >= 0))
);


--
-- Name: registry_releases; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_releases (
    id uuid NOT NULL,
    registry_target_id uuid NOT NULL,
    service_id text NOT NULL,
    repository text NOT NULL,
    root_digest text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    succeeded_at timestamp with time zone,
    availability text DEFAULT 'present'::text NOT NULL,
    availability_observed_at timestamp with time zone,
    CONSTRAINT registry_releases_availability_check CHECK ((availability = ANY (ARRAY['present'::text, 'expired'::text, 'missing'::text]))),
    CONSTRAINT registry_releases_check CHECK ((((availability = 'present'::text) AND (availability_observed_at IS NULL)) OR ((availability <> 'present'::text) AND (availability_observed_at IS NOT NULL)))),
    CONSTRAINT registry_releases_repository_check CHECK ((repository <> ''::text)),
    CONSTRAINT registry_releases_root_digest_check CHECK ((root_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT registry_releases_service_id_check CHECK ((service_id <> ''::text))
);


--
-- Name: registry_runtime_gc_sweep_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_runtime_gc_sweep_receipts (
    registry_target_id uuid NOT NULL,
    execution_key text NOT NULL,
    plan_id uuid NOT NULL,
    candidate_set_digest text CONSTRAINT registry_runtime_gc_sweep_receipt_candidate_set_digest_not_null NOT NULL,
    checkpoint_revision text NOT NULL,
    provider_sweep_id text NOT NULL,
    helper_job_uid text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT registry_runtime_gc_receipt_digest_shape CHECK (((execution_key ~ '^sha256:[0-9a-f]{64}$'::text) AND (candidate_set_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT registry_runtime_gc_receipt_identity_shape CHECK ((((plan_id)::text ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$'::text) AND (checkpoint_revision ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$'::text) AND (provider_sweep_id ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$'::text) AND (helper_job_uid !~ '[[:space:][:cntrl:]]'::text))),
    CONSTRAINT registry_runtime_gc_receipt_time_shape CHECK (((completed_at >= started_at) AND (created_at >= completed_at)))
);


--
-- Name: registry_runtime_maintenance_executions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_runtime_maintenance_executions (
    registry_target_id uuid CONSTRAINT registry_runtime_maintenance_execut_registry_target_id_not_null NOT NULL,
    execution_key text NOT NULL,
    plan_id uuid NOT NULL,
    candidate_set_digest text CONSTRAINT registry_runtime_maintenance_exec_candidate_set_digest_not_null NOT NULL,
    state text DEFAULT 'acquired'::text NOT NULL,
    maintenance_mode text,
    deployment_uid text DEFAULT ''::text NOT NULL,
    original_replicas integer,
    checkpoint_revision text DEFAULT ''::text CONSTRAINT registry_runtime_maintenance_execu_checkpoint_revision_not_null NOT NULL,
    checkpoint_digest text DEFAULT ''::text CONSTRAINT registry_runtime_maintenance_executi_checkpoint_digest_not_null NOT NULL,
    checkpoint_observed_at timestamp with time zone,
    sweep_job_uid text DEFAULT ''::text NOT NULL,
    lease_owner text NOT NULL,
    lease_epoch bigint NOT NULL,
    lease_until timestamp with time zone NOT NULL,
    restored_at timestamp with time zone,
    released_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT registry_runtime_maintenance_checkpoint_shape CHECK ((((checkpoint_revision = ''::text) AND (checkpoint_digest = ''::text) AND (checkpoint_observed_at IS NULL)) OR ((checkpoint_revision <> ''::text) AND (checkpoint_digest <> ''::text) AND (checkpoint_observed_at IS NOT NULL)))),
    CONSTRAINT registry_runtime_maintenance_digest_shape CHECK (((execution_key ~ '^sha256:[0-9a-f]{64}$'::text) AND (candidate_set_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND ((checkpoint_digest = ''::text) OR (checkpoint_digest ~ '^sha256:[0-9a-f]{64}$'::text)))),
    CONSTRAINT registry_runtime_maintenance_executions_lease_epoch_check CHECK ((lease_epoch > 0)),
    CONSTRAINT registry_runtime_maintenance_executions_maintenance_mode_check CHECK (((maintenance_mode IS NULL) OR (maintenance_mode = ANY (ARRAY['read_only'::text, 'stopped'::text])))),
    CONSTRAINT registry_runtime_maintenance_executions_original_replicas_check CHECK (((original_replicas IS NULL) OR (original_replicas >= 0))),
    CONSTRAINT registry_runtime_maintenance_executions_state_check CHECK ((state = ANY (ARRAY['acquired'::text, 'entered'::text, 'checkpointed'::text, 'sweeping'::text, 'swept'::text, 'restored'::text, 'released'::text, 'failed'::text]))),
    CONSTRAINT registry_runtime_maintenance_identity_shape CHECK ((((plan_id)::text ~ '^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$'::text) AND (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$'::text) AND (deployment_uid !~ '[[:space:][:cntrl:]]'::text) AND (sweep_job_uid !~ '[[:space:][:cntrl:]]'::text))),
    CONSTRAINT registry_runtime_maintenance_lease_shape CHECK ((lease_until > updated_at)),
    CONSTRAINT registry_runtime_maintenance_restore_shape CHECK (((released_at IS NULL) OR ((restored_at IS NOT NULL) AND (released_at >= restored_at))))
);


--
-- Name: registry_runtime_observation_cursors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_runtime_observation_cursors (
    registry_target_id uuid CONSTRAINT registry_runtime_observation_cursor_registry_target_id_not_null NOT NULL,
    completed_revision bigint DEFAULT 0 CONSTRAINT registry_runtime_observation_cursor_completed_revision_not_null NOT NULL,
    completed_at timestamp with time zone,
    next_observe_at timestamp with time zone NOT NULL,
    consecutive_failures integer DEFAULT 0 CONSTRAINT registry_runtime_observation_curs_consecutive_failures_not_null NOT NULL,
    last_error_code text DEFAULT ''::text NOT NULL,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT registry_runtime_observation_completed_shape CHECK ((((completed_revision = 0) AND (completed_at IS NULL)) OR ((completed_revision > 0) AND (completed_at IS NOT NULL)))),
    CONSTRAINT registry_runtime_observation_cursors_completed_revision_check CHECK ((completed_revision >= 0)),
    CONSTRAINT registry_runtime_observation_cursors_consecutive_failures_check CHECK ((consecutive_failures >= 0)),
    CONSTRAINT registry_runtime_observation_cursors_last_error_code_check CHECK (((last_error_code = ''::text) OR (last_error_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT registry_runtime_observation_cursors_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT registry_runtime_observation_lease_shape CHECK ((((lease_owner IS NULL) AND (lease_until IS NULL)) OR ((lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$'::text) AND (lease_epoch > 0) AND (lease_until > updated_at))))
);


--
-- Name: registry_targets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.registry_targets (
    id uuid NOT NULL,
    name text NOT NULL,
    mode text NOT NULL,
    endpoint text NOT NULL,
    repository_prefix text NOT NULL,
    pull_credential_ref text DEFAULT ''::text NOT NULL,
    push_credential_ref text DEFAULT ''::text NOT NULL,
    cache_credential_ref text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT registry_targets_endpoint_check CHECK ((endpoint <> ''::text)),
    CONSTRAINT registry_targets_mode_check CHECK ((mode = ANY (ARRAY['managed'::text, 'external'::text]))),
    CONSTRAINT registry_targets_repository_prefix_check CHECK ((repository_prefix <> ''::text))
);


--
-- Name: runtime_readiness; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.runtime_readiness (
    runtime_kind text NOT NULL,
    scope_key text NOT NULL,
    worker_id text NOT NULL,
    worker_epoch bigint NOT NULL,
    contract_version text NOT NULL,
    config_digest text NOT NULL,
    identity jsonb NOT NULL,
    observation jsonb NOT NULL,
    registry_target_id uuid,
    platform_binding_id uuid,
    started_at timestamp with time zone NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    lease_until timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT runtime_readiness_check CHECK (((observed_at >= started_at) AND (updated_at >= observed_at) AND (lease_until > observed_at))),
    CONSTRAINT runtime_readiness_check1 CHECK ((((runtime_kind = 'managed-registry'::text) AND (registry_target_id IS NOT NULL) AND (platform_binding_id IS NULL) AND (scope_key = (registry_target_id)::text)) OR ((runtime_kind = 'argo-desired-state'::text) AND (registry_target_id IS NULL) AND (platform_binding_id IS NOT NULL) AND (scope_key = 'global'::text)) OR ((runtime_kind <> ALL (ARRAY['managed-registry'::text, 'argo-desired-state'::text])) AND (registry_target_id IS NULL) AND (platform_binding_id IS NULL) AND (scope_key = 'global'::text)))),
    CONSTRAINT runtime_readiness_config_digest_check CHECK ((config_digest ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT runtime_readiness_contract_version_check CHECK (((length(contract_version) >= 8) AND (length(contract_version) <= 64) AND (contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'::text))),
    CONSTRAINT runtime_readiness_identity_check CHECK (((jsonb_typeof(identity) = 'object'::text) AND (octet_length((identity)::text) <= 8192))),
    CONSTRAINT runtime_readiness_observation_check CHECK (((jsonb_typeof(observation) = 'object'::text) AND (octet_length((observation)::text) <= 2048))),
    CONSTRAINT runtime_readiness_runtime_kind_check CHECK ((runtime_kind = ANY (ARRAY['source-build'::text, 'managed-registry'::text, 'git-projection'::text, 'runtime-secret'::text, 'argo-desired-state'::text, 'runtime-registry-pull'::text, 'edge'::text, 'environment-foundation'::text, 'auto-deploy'::text, 'certificate-issuer-observer'::text, 'tls-certificate-observer'::text]))),
    CONSTRAINT runtime_readiness_scope_key_check CHECK (((scope_key = 'global'::text) OR (scope_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'::text))),
    CONSTRAINT runtime_readiness_worker_epoch_check CHECK ((worker_epoch > 0)),
    CONSTRAINT runtime_readiness_worker_id_check CHECK (((length(worker_id) >= 1) AND (length(worker_id) <= 256) AND (worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'::text)))
);


--
-- Name: runtime_registry_pull_artifacts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.runtime_registry_pull_artifacts (
    environment_id uuid NOT NULL,
    namespace text NOT NULL,
    registry_target_id uuid NOT NULL,
    pull_credential_ref text NOT NULL,
    profile_name text NOT NULL,
    profile_revision bigint NOT NULL,
    secret_name text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    runtime_state text DEFAULT 'awaiting'::text NOT NULL,
    next_observation_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_failure_code text DEFAULT ''::text NOT NULL,
    observed_uid text DEFAULT ''::text NOT NULL,
    observed_resource_version text DEFAULT ''::text CONSTRAINT runtime_registry_pull_artifa_observed_resource_version_not_null NOT NULL,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    worker_contract text,
    worker_config_digest text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT runtime_registry_pull_artifacts_check CHECK (((updated_at >= created_at) AND (next_observation_at >= created_at))),
    CONSTRAINT runtime_registry_pull_artifacts_check1 CHECK (((last_observed_at IS NULL) = (observed_uid = ''::text))),
    CONSTRAINT runtime_registry_pull_artifacts_check2 CHECK (((observed_uid = ''::text) = (observed_resource_version = ''::text))),
    CONSTRAINT runtime_registry_pull_artifacts_check3 CHECK (((last_failure_code = ''::text) = (consecutive_failures = 0))),
    CONSTRAINT runtime_registry_pull_artifacts_check4 CHECK ((((runtime_state = 'awaiting'::text) AND (last_observed_at IS NULL)) OR ((runtime_state = 'ready'::text) AND (last_observed_at IS NOT NULL)) OR (runtime_state = 'failed'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_check5 CHECK ((((lease_owner IS NULL) AND (lease_until IS NULL) AND (worker_contract IS NULL) AND (worker_config_digest IS NULL)) OR ((lease_owner IS NOT NULL) AND (lease_until IS NOT NULL) AND (worker_contract IS NOT NULL) AND (worker_config_digest IS NOT NULL) AND (lease_epoch > 0) AND (lease_until > updated_at)))),
    CONSTRAINT runtime_registry_pull_artifacts_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 30))),
    CONSTRAINT runtime_registry_pull_artifacts_last_failure_code_check CHECK (((last_failure_code = ''::text) OR (last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT runtime_registry_pull_artifacts_lease_owner_check CHECK (((lease_owner IS NULL) OR ((length(lease_owner) >= 16) AND (length(lease_owner) <= 128) AND (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'::text)))),
    CONSTRAINT runtime_registry_pull_artifacts_namespace_check CHECK (((length(namespace) >= 1) AND (length(namespace) <= 63) AND (namespace ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_observed_resource_version_check CHECK (((length(observed_resource_version) <= 128) AND (observed_resource_version !~ '[\x00\r\n]'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_observed_uid_check CHECK (((observed_uid = ''::text) OR (observed_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_profile_name_check CHECK (((length(profile_name) >= 1) AND (length(profile_name) <= 63) AND (profile_name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_profile_revision_check CHECK ((profile_revision > 0)),
    CONSTRAINT runtime_registry_pull_artifacts_pull_credential_ref_check CHECK (((length(pull_credential_ref) >= 1) AND (length(pull_credential_ref) <= 256) AND (pull_credential_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_runtime_state_check CHECK ((runtime_state = ANY (ARRAY['awaiting'::text, 'ready'::text, 'failed'::text]))),
    CONSTRAINT runtime_registry_pull_artifacts_secret_name_check CHECK (((length(secret_name) >= 1) AND (length(secret_name) <= 63) AND (secret_name ~ '^kuberploy-pull-[a-f0-9]{24}$'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_worker_config_digest_check CHECK (((worker_config_digest IS NULL) OR (worker_config_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT runtime_registry_pull_artifacts_worker_contract_check CHECK (((worker_contract IS NULL) OR ((length(worker_contract) >= 8) AND (length(worker_contract) <= 64) AND (worker_contract ~ '^[a-z][a-z0-9.-]{7,63}$'::text))))
);


--
-- Name: secret_binding_deliveries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.secret_binding_deliveries (
    version_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    ordinal integer NOT NULL,
    source_key text NOT NULL,
    kind text NOT NULL,
    environment_name text,
    file_path text,
    file_mode integer,
    CONSTRAINT secret_binding_deliveries_check CHECK ((((kind = 'environment'::text) AND (environment_name ~ '^[A-Za-z_][A-Za-z0-9_]{0,252}$'::text) AND (file_path IS NULL) AND (file_mode IS NULL)) OR ((kind = 'file'::text) AND (environment_name IS NULL) AND (file_path ~ '^/var/run/secrets/kuberploy/[A-Za-z0-9._/-]+$'::text) AND (file_path !~ '/\\.?\\.?(/|$)'::text) AND (file_path !~ '//'::text) AND (file_mode = ANY (ARRAY[256, 288]))))),
    CONSTRAINT secret_binding_deliveries_kind_check CHECK ((kind = ANY (ARRAY['environment'::text, 'file'::text]))),
    CONSTRAINT secret_binding_deliveries_ordinal_check CHECK (((ordinal >= 0) AND (ordinal <= 127))),
    CONSTRAINT secret_binding_deliveries_source_key_check CHECK (((length(source_key) >= 1) AND (length(source_key) <= 253) AND (source_key ~ '^[A-Za-z0-9._-]+$'::text)))
);


--
-- Name: secret_binding_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.secret_binding_events (
    id uuid NOT NULL,
    binding_id uuid NOT NULL,
    version_id uuid,
    actor_id uuid,
    kind text NOT NULL,
    request_id text NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    published_at timestamp with time zone,
    CONSTRAINT secret_binding_events_check CHECK (((published_at IS NULL) OR (published_at >= occurred_at))),
    CONSTRAINT secret_binding_events_kind_check CHECK ((kind = ANY (ARRAY['version-staging'::text, 'version-awaiting-readiness'::text, 'version-active'::text, 'version-failed'::text, 'reference-added'::text, 'reference-removed'::text, 'binding-deleting'::text, 'binding-deleted'::text]))),
    CONSTRAINT secret_binding_events_request_id_check CHECK ((request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'::text))
);


--
-- Name: secret_binding_references; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.secret_binding_references (
    binding_id uuid NOT NULL,
    version_id uuid NOT NULL,
    kind text NOT NULL,
    reference_id text NOT NULL,
    revision text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT secret_binding_references_kind_check CHECK ((kind = ANY (ARRAY['git-current'::text, 'current-release'::text, 'retained-release'::text]))),
    CONSTRAINT secret_binding_references_reference_id_check CHECK (((length(reference_id) >= 1) AND (length(reference_id) <= 256) AND (reference_id !~ '[[:cntrl:]]'::text))),
    CONSTRAINT secret_binding_references_revision_check CHECK ((revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64}|sha256:[0-9a-f]{64})$'::text))
);


--
-- Name: secret_binding_runtime_reconciliations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.secret_binding_runtime_reconciliations (
    version_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    runtime_state text DEFAULT 'awaiting'::text NOT NULL,
    next_attempt_at timestamp with time zone NOT NULL,
    consecutive_failures integer DEFAULT 0 CONSTRAINT secret_binding_runtime_reconcilia_consecutive_failures_not_null NOT NULL,
    last_failure_code text DEFAULT ''::text CONSTRAINT secret_binding_runtime_reconciliatio_last_failure_code_not_null NOT NULL,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_until timestamp with time zone,
    worker_contract text,
    worker_config_digest text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT secret_binding_runtime_reconciliatio_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 30))),
    CONSTRAINT secret_binding_runtime_reconciliatio_worker_config_digest_check CHECK (((worker_config_digest IS NULL) OR (worker_config_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT secret_binding_runtime_reconciliations_check CHECK (((updated_at >= created_at) AND (next_attempt_at >= created_at))),
    CONSTRAINT secret_binding_runtime_reconciliations_check1 CHECK ((((runtime_state = 'awaiting'::text) AND (completed_at IS NULL)) OR ((runtime_state = ANY (ARRAY['ready'::text, 'failed'::text])) AND (completed_at IS NOT NULL) AND (completed_at >= created_at)))),
    CONSTRAINT secret_binding_runtime_reconciliations_check2 CHECK ((((lease_owner IS NULL) AND (lease_until IS NULL) AND (worker_contract IS NULL) AND (worker_config_digest IS NULL)) OR ((lease_owner IS NOT NULL) AND (lease_until IS NOT NULL) AND (worker_contract IS NOT NULL) AND (worker_config_digest IS NOT NULL) AND (lease_epoch > 0) AND (lease_until > updated_at)))),
    CONSTRAINT secret_binding_runtime_reconciliations_check3 CHECK (((last_failure_code = ''::text) = (consecutive_failures = 0))),
    CONSTRAINT secret_binding_runtime_reconciliations_last_failure_code_check CHECK (((last_failure_code = ''::text) OR (last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT secret_binding_runtime_reconciliations_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT secret_binding_runtime_reconciliations_lease_owner_check CHECK (((lease_owner IS NULL) OR ((length(lease_owner) >= 16) AND (length(lease_owner) <= 128) AND (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'::text)))),
    CONSTRAINT secret_binding_runtime_reconciliations_runtime_state_check CHECK ((runtime_state = ANY (ARRAY['awaiting'::text, 'ready'::text, 'failed'::text]))),
    CONSTRAINT secret_binding_runtime_reconciliations_worker_contract_check CHECK (((worker_contract IS NULL) OR ((length(worker_contract) >= 8) AND (length(worker_contract) <= 64) AND (worker_contract ~ '^[a-z][a-z0-9.-]{7,63}$'::text))))
);


--
-- Name: secret_binding_versions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.secret_binding_versions (
    id uuid NOT NULL,
    binding_id uuid NOT NULL,
    version_number bigint NOT NULL,
    provider text NOT NULL,
    state text NOT NULL,
    fingerprint_key_id text NOT NULL,
    content_fingerprint bytea NOT NULL,
    provider_object_name text,
    target_secret_name text,
    provider_revision text,
    manifest_digest text,
    sealed_key_fingerprint text,
    ciphertext_digest text,
    failure_code text DEFAULT ''::text NOT NULL,
    staged_at timestamp with time zone,
    readiness_observed_at timestamp with time zone,
    activated_at timestamp with time zone,
    retained_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    target_secret_type text DEFAULT 'Opaque'::text NOT NULL,
    CONSTRAINT secret_binding_versions_check CHECK ((updated_at >= created_at)),
    CONSTRAINT secret_binding_versions_check1 CHECK ((((provider_object_name IS NULL) AND (target_secret_name IS NULL) AND (provider_revision IS NULL) AND (manifest_digest IS NULL) AND (sealed_key_fingerprint IS NULL) AND (ciphertext_digest IS NULL)) OR ((provider_object_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$'::text) AND (target_secret_name ~ '^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$'::text) AND ((length(provider_revision) >= 1) AND (length(provider_revision) <= 256)) AND (manifest_digest ~ '^sha256:[0-9a-f]{64}$'::text) AND (((provider = 'external-secrets'::text) AND (sealed_key_fingerprint IS NULL) AND (ciphertext_digest IS NULL)) OR ((provider = 'sealed-secrets'::text) AND (sealed_key_fingerprint ~ '^sha256:[0-9a-f]{64}$'::text) AND (ciphertext_digest ~ '^sha256:[0-9a-f]{64}$'::text)))))),
    CONSTRAINT secret_binding_versions_check2 CHECK (((state = ANY (ARRAY['staging'::text, 'failed'::text, 'deleted'::text])) OR (provider_object_name IS NOT NULL))),
    CONSTRAINT secret_binding_versions_check3 CHECK ((((state = 'staging'::text) AND (staged_at IS NULL)) OR ((state <> 'staging'::text) AND ((staged_at IS NOT NULL) OR (state = 'failed'::text))))),
    CONSTRAINT secret_binding_versions_check4 CHECK ((((state = ANY (ARRAY['active'::text, 'retained'::text])) AND (activated_at IS NOT NULL)) OR (state <> ALL (ARRAY['active'::text, 'retained'::text])))),
    CONSTRAINT secret_binding_versions_check5 CHECK ((((state = 'retained'::text) AND (retained_at IS NOT NULL)) OR (state <> 'retained'::text))),
    CONSTRAINT secret_binding_versions_check6 CHECK ((((state = 'failed'::text) AND (failure_code <> ''::text)) OR ((state <> ALL (ARRAY['failed'::text, 'deleted'::text])) AND (failure_code = ''::text)) OR (state = 'deleted'::text))),
    CONSTRAINT secret_binding_versions_content_fingerprint_check CHECK ((octet_length(content_fingerprint) = 32)),
    CONSTRAINT secret_binding_versions_failure_code_check CHECK (((failure_code = ''::text) OR (failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT secret_binding_versions_fingerprint_key_id_check CHECK (((length(fingerprint_key_id) >= 1) AND (length(fingerprint_key_id) <= 128) AND (fingerprint_key_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'::text))),
    CONSTRAINT secret_binding_versions_provider_check CHECK ((provider = ANY (ARRAY['external-secrets'::text, 'sealed-secrets'::text]))),
    CONSTRAINT secret_binding_versions_provider_revision_check CHECK (((provider_revision IS NULL) OR ((length(provider_revision) >= 1) AND (length(provider_revision) <= 256) AND (provider_revision = btrim(provider_revision)) AND (provider_revision !~ '[[:cntrl:]]'::text)))),
    CONSTRAINT secret_binding_versions_state_check CHECK ((state = ANY (ARRAY['staging'::text, 'awaiting-readiness'::text, 'active'::text, 'retained'::text, 'failed'::text, 'deleted'::text]))),
    CONSTRAINT secret_binding_versions_target_type_check CHECK ((target_secret_type = ANY (ARRAY['Opaque'::text, 'kubernetes.io/tls'::text]))),
    CONSTRAINT secret_binding_versions_version_number_check CHECK ((version_number > 0))
);


--
-- Name: secret_bindings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.secret_bindings (
    id uuid NOT NULL,
    organization_id uuid,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    target_namespace text NOT NULL,
    name text NOT NULL,
    provider text NOT NULL,
    state text NOT NULL,
    active_version bigint DEFAULT 0 NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    delete_started_at timestamp with time zone,
    deleted_at timestamp with time zone,
    purpose text DEFAULT 'runtime-secret'::text NOT NULL,
    CONSTRAINT secret_bindings_active_version_check CHECK ((active_version >= 0)),
    CONSTRAINT secret_bindings_check CHECK ((updated_at >= created_at)),
    CONSTRAINT secret_bindings_check1 CHECK ((((state = 'ready'::text) AND (active_version > 0)) OR ((state = ANY (ARRAY['provisioning'::text, 'failed'::text, 'deleted'::text])) AND (active_version = 0)) OR (state = 'deleting'::text))),
    CONSTRAINT secret_bindings_check2 CHECK ((((state = 'deleting'::text) AND (delete_started_at IS NOT NULL) AND (deleted_at IS NULL)) OR ((state = 'deleted'::text) AND (delete_started_at IS NOT NULL) AND (deleted_at IS NOT NULL) AND (deleted_at >= delete_started_at)) OR ((state <> ALL (ARRAY['deleting'::text, 'deleted'::text])) AND (delete_started_at IS NULL) AND (deleted_at IS NULL)))),
    CONSTRAINT secret_bindings_name_check CHECK (((length(name) >= 1) AND (length(name) <= 63) AND (name ~ '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$'::text))),
    CONSTRAINT secret_bindings_provider_check CHECK ((provider = ANY (ARRAY['external-secrets'::text, 'sealed-secrets'::text]))),
    CONSTRAINT secret_bindings_purpose_check CHECK (((purpose = 'runtime-secret'::text) OR ((purpose = 'tls-certificate'::text) AND (provider = 'sealed-secrets'::text)))),
    CONSTRAINT secret_bindings_state_check CHECK ((state = ANY (ARRAY['provisioning'::text, 'ready'::text, 'deleting'::text, 'deleted'::text, 'failed'::text])))
);


--
-- Name: service_account_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.service_account_tokens (
    id uuid NOT NULL,
    service_account_id uuid NOT NULL,
    name text NOT NULL,
    token_prefix text NOT NULL,
    token_hash bytea NOT NULL,
    scopes text[] NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT service_account_tokens_check CHECK (((expires_at > created_at) AND (expires_at <= (created_at + '90 days'::interval)))),
    CONSTRAINT service_account_tokens_check1 CHECK (((last_used_at IS NULL) OR (last_used_at >= created_at))),
    CONSTRAINT service_account_tokens_check2 CHECK (((revoked_at IS NULL) OR (revoked_at >= created_at))),
    CONSTRAINT service_account_tokens_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100) AND (name = btrim(name)) AND (name !~ '[[:cntrl:]]'::text))),
    CONSTRAINT service_account_tokens_scopes_check CHECK (((cardinality(scopes) >= 1) AND (cardinality(scopes) <= 4))),
    CONSTRAINT service_account_tokens_scopes_check1 CHECK ((scopes <@ ARRAY['app.read'::text, 'app.edit'::text, 'build.create'::text, 'logs.read'::text])),
    CONSTRAINT service_account_tokens_scopes_check2 CHECK ((array_position(scopes, NULL::text) IS NULL)),
    CONSTRAINT service_account_tokens_token_hash_check CHECK ((octet_length(token_hash) = 32)),
    CONSTRAINT service_account_tokens_token_prefix_check CHECK ((token_prefix ~ '^kp_sa_[A-Za-z0-9_-]{8}$'::text))
);


--
-- Name: service_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.service_accounts (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    role text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
    CONSTRAINT service_accounts_check CHECK (((disabled_at IS NULL) OR (disabled_at >= created_at))),
    CONSTRAINT service_accounts_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100) AND (name = btrim(name)) AND (name !~ '[[:cntrl:]]'::text))),
    CONSTRAINT service_accounts_role_check CHECK ((role = ANY (ARRAY['viewer'::text, 'developer'::text, 'project-admin'::text])))
);


--
-- Name: service_registry_policies; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.service_registry_policies (
    registry_target_id uuid NOT NULL,
    service_id text NOT NULL,
    repository text NOT NULL,
    keep_last_successful integer DEFAULT 10 NOT NULL,
    minimum_safety_age_seconds bigint DEFAULT 86400 NOT NULL,
    cache_unused_expiry_seconds bigint DEFAULT 604800 NOT NULL,
    cache_byte_quota bigint DEFAULT '10737418240'::bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT service_registry_policies_cache_byte_quota_check CHECK ((cache_byte_quota > 0)),
    CONSTRAINT service_registry_policies_cache_unused_expiry_seconds_check CHECK ((cache_unused_expiry_seconds >= 60)),
    CONSTRAINT service_registry_policies_keep_last_successful_check CHECK (((keep_last_successful >= 1) AND (keep_last_successful <= 100))),
    CONSTRAINT service_registry_policies_minimum_safety_age_seconds_check CHECK ((minimum_safety_age_seconds >= 60)),
    CONSTRAINT service_registry_policies_repository_check CHECK ((repository <> ''::text)),
    CONSTRAINT service_registry_policies_service_id_check CHECK ((service_id <> ''::text))
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    token_hash bytea NOT NULL,
    user_id uuid NOT NULL,
    grant_revision bigint NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: team_memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.team_memberships (
    team_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT team_memberships_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'member'::text])))
);


--
-- Name: teams; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.teams (
    id uuid NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tls_certificate_observations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tls_certificate_observations (
    version_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    target_digest text NOT NULL,
    observation_contract_version text DEFAULT ''::text CONSTRAINT tls_certificate_observation_observation_contract_versi_not_null NOT NULL,
    observation_config_digest text DEFAULT ''::text NOT NULL,
    state text NOT NULL,
    next_observation_at timestamp with time zone NOT NULL,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    failure_code text DEFAULT ''::text NOT NULL,
    last_observed_at timestamp with time zone,
    last_ready_at timestamp with time zone,
    lease_owner text,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_claimed_at timestamp with time zone,
    lease_until timestamp with time zone,
    lease_contract_version text,
    lease_config_digest text,
    lease_target_digest text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT tls_certificate_observations_check CHECK ((updated_at >= created_at)),
    CONSTRAINT tls_certificate_observations_check1 CHECK (((observation_contract_version = ''::text) = (observation_config_digest = ''::text))),
    CONSTRAINT tls_certificate_observations_check2 CHECK ((((state = 'awaiting'::text) AND (failure_code = ''::text) AND (last_observed_at IS NULL) AND (last_ready_at IS NULL)) OR ((state = 'ready'::text) AND (failure_code = ''::text) AND (last_observed_at IS NOT NULL) AND (last_ready_at = last_observed_at) AND (observation_contract_version <> ''::text)) OR ((state = 'degraded'::text) AND (failure_code <> ''::text) AND (last_observed_at IS NOT NULL) AND (observation_contract_version <> ''::text)) OR ((state = 'requeue'::text) AND (failure_code <> ''::text) AND (observation_contract_version <> ''::text)))),
    CONSTRAINT tls_certificate_observations_check3 CHECK (((last_ready_at IS NULL) OR ((last_observed_at IS NOT NULL) AND (last_ready_at <= last_observed_at)))),
    CONSTRAINT tls_certificate_observations_check4 CHECK ((((lease_owner IS NULL) AND (lease_claimed_at IS NULL) AND (lease_until IS NULL) AND (lease_contract_version IS NULL) AND (lease_config_digest IS NULL) AND (lease_target_digest IS NULL)) OR ((lease_owner IS NOT NULL) AND (lease_claimed_at IS NOT NULL) AND (lease_until > lease_claimed_at) AND (lease_contract_version IS NOT NULL) AND (lease_config_digest IS NOT NULL) AND (lease_target_digest = target_digest)))),
    CONSTRAINT tls_certificate_observations_consecutive_failures_check CHECK (((consecutive_failures >= 0) AND (consecutive_failures <= 30))),
    CONSTRAINT tls_certificate_observations_failure_code_check CHECK (((failure_code = ''::text) OR (failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'::text))),
    CONSTRAINT tls_certificate_observations_lease_config_digest_check CHECK (((lease_config_digest IS NULL) OR (lease_config_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT tls_certificate_observations_lease_contract_version_check CHECK (((lease_contract_version IS NULL) OR (lease_contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'::text))),
    CONSTRAINT tls_certificate_observations_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT tls_certificate_observations_lease_owner_check CHECK (((lease_owner IS NULL) OR (lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'::text))),
    CONSTRAINT tls_certificate_observations_lease_target_digest_check CHECK (((lease_target_digest IS NULL) OR (lease_target_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT tls_certificate_observations_observation_config_digest_check CHECK (((observation_config_digest = ''::text) OR (observation_config_digest ~ '^sha256:[0-9a-f]{64}$'::text))),
    CONSTRAINT tls_certificate_observations_observation_contract_version_check CHECK (((observation_contract_version = ''::text) OR (observation_contract_version ~ '^[a-z][a-z0-9.-]{7,63}$'::text))),
    CONSTRAINT tls_certificate_observations_state_check CHECK ((state = ANY (ARRAY['awaiting'::text, 'ready'::text, 'degraded'::text, 'requeue'::text]))),
    CONSTRAINT tls_certificate_observations_target_digest_check CHECK ((target_digest ~ '^sha256:[0-9a-f]{64}$'::text))
);


--
-- Name: tls_certificate_versions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tls_certificate_versions (
    version_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    version_number bigint NOT NULL,
    secret_content_fingerprint bytea NOT NULL,
    leaf_fingerprint text NOT NULL,
    public_key_fingerprint text NOT NULL,
    dns_names jsonb NOT NULL,
    ip_addresses jsonb NOT NULL,
    not_before timestamp with time zone NOT NULL,
    not_after timestamp with time zone NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT tls_certificate_versions_check CHECK (((jsonb_typeof(ip_addresses) = 'array'::text) AND ((jsonb_array_length(ip_addresses) >= 0) AND (jsonb_array_length(ip_addresses) <= 128)) AND ((jsonb_array_length(dns_names) + jsonb_array_length(ip_addresses)) <= 128))),
    CONSTRAINT tls_certificate_versions_check1 CHECK ((not_after > not_before)),
    CONSTRAINT tls_certificate_versions_dns_names_check CHECK (((jsonb_typeof(dns_names) = 'array'::text) AND ((jsonb_array_length(dns_names) >= 1) AND (jsonb_array_length(dns_names) <= 128)))),
    CONSTRAINT tls_certificate_versions_leaf_fingerprint_check CHECK ((leaf_fingerprint ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT tls_certificate_versions_public_key_fingerprint_check CHECK ((public_key_fingerprint ~ '^sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT tls_certificate_versions_secret_content_fingerprint_check CHECK ((octet_length(secret_content_fingerprint) = 32)),
    CONSTRAINT tls_certificate_versions_version_number_check CHECK ((version_number > 0))
);


--
-- Name: user_invitations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_invitations (
    id uuid NOT NULL,
    token_hash bytea NOT NULL,
    email text CONSTRAINT user_invitations_display_name_not_null NOT NULL,
    created_by uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    accepted_user_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_invitations_check CHECK (((accepted_at IS NULL) = (accepted_user_id IS NULL)))
);


--
-- Name: user_password_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_password_credentials (
    user_id uuid NOT NULL,
    email_normalized text CONSTRAINT user_password_credentials_login_normalized_not_null NOT NULL,
    password_hash text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT user_password_credentials_email_normalized_check CHECK ((email_normalized = lower(btrim(email_normalized)))),
    CONSTRAINT user_password_credentials_email_normalized_check1 CHECK (((length(email_normalized) >= 3) AND (length(email_normalized) <= 254))),
    CONSTRAINT user_password_credentials_password_hash_check CHECK (((length(password_hash) >= 64) AND (length(password_hash) <= 512)))
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id uuid NOT NULL,
    display_name text CONSTRAINT users_login_not_null NOT NULL,
    role text NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    grant_revision bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    email text,
    CONSTRAINT users_email_check CHECK (((email IS NULL) OR ((email = lower(btrim(email))) AND ((length(email) >= 3) AND (length(email) <= 254))))),
    CONSTRAINT users_role_check CHECK ((role = ANY (ARRAY['platform-admin'::text, 'developer'::text])))
);


--
-- Name: access_grants access_grants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_grants
    ADD CONSTRAINT access_grants_pkey PRIMARY KEY (id);


--
-- Name: applications applications_id_project_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_id_project_unique UNIQUE (id, project_id);


--
-- Name: applications applications_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_pkey PRIMARY KEY (id);


--
-- Name: applications applications_project_id_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_project_id_slug_key UNIQUE (project_id, slug);


--
-- Name: argo_application_observations argo_application_observations_application_environment_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_application_environment_unique UNIQUE (application_id, environment_id);


--
-- Name: argo_application_observations argo_application_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_pkey PRIMARY KEY (deployment_id);


--
-- Name: argo_desired_state_commands argo_desired_state_commands_environment_id_generation_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_environment_id_generation_key UNIQUE (environment_id, generation);


--
-- Name: argo_desired_state_commands argo_desired_state_commands_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_pkey PRIMARY KEY (id);


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materialization_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_materialization_receipts
    ADD CONSTRAINT argo_desired_state_materialization_receipts_pkey PRIMARY KEY (id);


--
-- Name: argo_observation_runtime argo_observation_runtime_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_observation_runtime
    ADD CONSTRAINT argo_observation_runtime_pkey PRIMARY KEY (argo_namespace);


--
-- Name: argo_rollback_commands argo_rollback_commands_operation_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_operation_id_key UNIQUE (operation_id);


--
-- Name: argo_rollback_commands argo_rollback_commands_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_pkey PRIMARY KEY (id);


--
-- Name: audit_events audit_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);


--
-- Name: auto_deploy_policies auto_deploy_policies_application_id_environment_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policies
    ADD CONSTRAINT auto_deploy_policies_application_id_environment_id_key UNIQUE (application_id, environment_id);


--
-- Name: auto_deploy_policies auto_deploy_policies_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policies
    ADD CONSTRAINT auto_deploy_policies_pkey PRIMARY KEY (id);


--
-- Name: auto_deploy_policy_revisions auto_deploy_policy_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policy_revisions
    ADD CONSTRAINT auto_deploy_policy_revisions_pkey PRIMARY KEY (policy_id, revision);


--
-- Name: auto_deploy_runs auto_deploy_runs_idempotency_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_idempotency_key_key UNIQUE (idempotency_key);


--
-- Name: auto_deploy_runs auto_deploy_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_pkey PRIMARY KEY (attempt_id, policy_id);


--
-- Name: build_attempts build_attempts_delivery_claim_key_definition_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_attempts
    ADD CONSTRAINT build_attempts_delivery_claim_key_definition_id_key UNIQUE (delivery_claim_key, definition_id);


--
-- Name: build_attempts build_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_attempts
    ADD CONSTRAINT build_attempts_pkey PRIMARY KEY (id);


--
-- Name: build_attempts build_attempts_project_id_service_id_generation_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_attempts
    ADD CONSTRAINT build_attempts_project_id_service_id_generation_key UNIQUE (project_id, service_id, generation);


--
-- Name: build_attempts build_attempts_trigger_kind_trigger_key_definition_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_attempts
    ADD CONSTRAINT build_attempts_trigger_kind_trigger_key_definition_id_key UNIQUE (trigger_kind, trigger_key, definition_id);


--
-- Name: build_release_projections build_release_projections_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_release_projections
    ADD CONSTRAINT build_release_projections_pkey PRIMARY KEY (attempt_id);


--
-- Name: builder_platform_setting_mutations builder_platform_setting_mutations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.builder_platform_setting_mutations
    ADD CONSTRAINT builder_platform_setting_mutations_pkey PRIMARY KEY (actor_id, idempotency_key);


--
-- Name: builder_platform_settings_revisions builder_platform_settings_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.builder_platform_settings_revisions
    ADD CONSTRAINT builder_platform_settings_revisions_pkey PRIMARY KEY (revision);


--
-- Name: cert_manager_issuer_observations cert_manager_issuer_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cert_manager_issuer_observations
    ADD CONSTRAINT cert_manager_issuer_observations_pkey PRIMARY KEY (profile_id, revision);


--
-- Name: cert_manager_issuer_references cert_manager_issuer_references_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cert_manager_issuer_references
    ADD CONSTRAINT cert_manager_issuer_references_pkey PRIMARY KEY (git_path, hostname);


--
-- Name: configuration_profile_assignments configuration_profile_assignm_profile_id_revision_scope_typ_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_assignments
    ADD CONSTRAINT configuration_profile_assignm_profile_id_revision_scope_typ_key UNIQUE (profile_id, revision, scope_type, scope_id);


--
-- Name: configuration_profile_assignments configuration_profile_assignments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_assignments
    ADD CONSTRAINT configuration_profile_assignments_pkey PRIMARY KEY (profile_id, revision, ordinal);


--
-- Name: configuration_profile_revisions configuration_profile_revisio_profile_id_revision_profile_k_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_revisions
    ADD CONSTRAINT configuration_profile_revisio_profile_id_revision_profile_k_key UNIQUE (profile_id, revision, profile_kind);


--
-- Name: configuration_profile_revisions configuration_profile_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_revisions
    ADD CONSTRAINT configuration_profile_revisions_pkey PRIMARY KEY (profile_id, revision);


--
-- Name: configuration_profiles configuration_profiles_id_kind_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profiles
    ADD CONSTRAINT configuration_profiles_id_kind_key UNIQUE (id, kind);


--
-- Name: configuration_profiles configuration_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profiles
    ADD CONSTRAINT configuration_profiles_pkey PRIMARY KEY (id);


--
-- Name: deployment_operation_inputs deployment_operation_inputs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_operation_inputs
    ADD CONSTRAINT deployment_operation_inputs_pkey PRIMARY KEY (operation_id);


--
-- Name: deployments deployments_id_application_environment_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_id_application_environment_unique UNIQUE (id, application_id, environment_id);


--
-- Name: deployments deployments_operation_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_operation_id_key UNIQUE (operation_id);


--
-- Name: deployments deployments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_pkey PRIMARY KEY (id);


--
-- Name: edge_runtime_targets edge_runtime_targets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.edge_runtime_targets
    ADD CONSTRAINT edge_runtime_targets_pkey PRIMARY KEY (target_key, profile_revision);


--
-- Name: edge_sslip_ingress_observations edge_sslip_ingress_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.edge_sslip_ingress_observations
    ADD CONSTRAINT edge_sslip_ingress_observations_pkey PRIMARY KEY (target_key, profile_revision);


--
-- Name: environment_app_placements environment_app_placements_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_app_placements
    ADD CONSTRAINT environment_app_placements_pkey PRIMARY KEY (environment_id, application_id);


--
-- Name: environment_foundation_intents environment_foundation_intents_intent_digest_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_foundation_intents
    ADD CONSTRAINT environment_foundation_intents_intent_digest_key UNIQUE (intent_digest);


--
-- Name: environment_foundation_intents environment_foundation_intents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_foundation_intents
    ADD CONSTRAINT environment_foundation_intents_pkey PRIMARY KEY (id);


--
-- Name: environments environments_id_project_namespace_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_id_project_namespace_unique UNIQUE (id, project_id, namespace);


--
-- Name: environments environments_id_project_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_id_project_unique UNIQUE (id, project_id);


--
-- Name: environments environments_namespace_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_namespace_key UNIQUE (namespace);


--
-- Name: environments environments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_pkey PRIMARY KEY (id);


--
-- Name: environments environments_project_id_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_project_id_slug_key UNIQUE (project_id, slug);


--
-- Name: external_dns_integration_environments external_dns_integration_environments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integration_environments
    ADD CONSTRAINT external_dns_integration_environments_pkey PRIMARY KEY (integration_id, environment_id);


--
-- Name: external_dns_integrations external_dns_integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integrations
    ADD CONSTRAINT external_dns_integrations_pkey PRIMARY KEY (id);


--
-- Name: external_dns_integrations external_dns_integrations_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integrations
    ADD CONSTRAINT external_dns_integrations_slug_key UNIQUE (slug);


--
-- Name: external_dns_integrations external_dns_integrations_txt_owner_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integrations
    ADD CONSTRAINT external_dns_integrations_txt_owner_id_key UNIQUE (txt_owner_id);


--
-- Name: git_path_reservations git_path_reservations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_path_reservations
    ADD CONSTRAINT git_path_reservations_pkey PRIMARY KEY (binding_id, target_ref, path);


--
-- Name: git_projected_documents git_projected_documents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projected_documents
    ADD CONSTRAINT git_projected_documents_pkey PRIMARY KEY (binding_id, generation, path);


--
-- Name: git_projection_generations git_projection_generations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_generations
    ADD CONSTRAINT git_projection_generations_pkey PRIMARY KEY (binding_id, generation);


--
-- Name: git_projection_push_wake_targets git_projection_push_wake_targets_binding_id_wake_generation_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_push_wake_targets
    ADD CONSTRAINT git_projection_push_wake_targets_binding_id_wake_generation_key UNIQUE (binding_id, wake_generation);


--
-- Name: git_projection_push_wake_targets git_projection_push_wake_targets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_push_wake_targets
    ADD CONSTRAINT git_projection_push_wake_targets_pkey PRIMARY KEY (delivery_hash, binding_id);


--
-- Name: git_projection_push_wakes git_projection_push_wakes_github_app_id_installation_id_rep_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_push_wakes
    ADD CONSTRAINT git_projection_push_wakes_github_app_id_installation_id_rep_key UNIQUE (github_app_id, installation_id, repository_id, target_ref, after_commit);


--
-- Name: git_projection_push_wakes git_projection_push_wakes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_push_wakes
    ADD CONSTRAINT git_projection_push_wakes_pkey PRIMARY KEY (delivery_hash);


--
-- Name: git_pull_request_publications git_pull_request_publications_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_pull_request_publications
    ADD CONSTRAINT git_pull_request_publications_pkey PRIMARY KEY (operation_id);


--
-- Name: git_pull_request_publications git_pull_request_publications_repository_id_candidate_ref_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_pull_request_publications
    ADD CONSTRAINT git_pull_request_publications_repository_id_candidate_ref_key UNIQUE (repository_id, candidate_ref);


--
-- Name: git_repository_bindings git_repository_bindings_id_project_id_environment_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_id_project_id_environment_id_key UNIQUE (id, project_id, environment_id);


--
-- Name: git_repository_bindings git_repository_bindings_id_target_ref_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_id_target_ref_key UNIQUE (id, target_ref);


--
-- Name: git_repository_bindings git_repository_bindings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_pkey PRIMARY KEY (id);


--
-- Name: git_repository_bindings git_repository_bindings_provider_installation_id_repository_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_provider_installation_id_repository_key UNIQUE (provider, installation_id, repository_id, target_ref, scope_id);


--
-- Name: git_safety_poll_cursors git_safety_poll_cursors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_safety_poll_cursors
    ADD CONSTRAINT git_safety_poll_cursors_pkey PRIMARY KEY (binding_id);


--
-- Name: git_ssh_key_mutation_receipts git_ssh_key_mutation_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_ssh_key_mutation_receipts
    ADD CONSTRAINT git_ssh_key_mutation_receipts_pkey PRIMARY KEY (actor_id, idempotency_key);


--
-- Name: git_ssh_key_revisions git_ssh_key_revisions_fingerprint_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_ssh_key_revisions
    ADD CONSTRAINT git_ssh_key_revisions_fingerprint_key UNIQUE (fingerprint);


--
-- Name: git_ssh_key_revisions git_ssh_key_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_ssh_key_revisions
    ADD CONSTRAINT git_ssh_key_revisions_pkey PRIMARY KEY (id);


--
-- Name: git_ssh_key_revisions git_ssh_key_revisions_scope_owner_id_revision_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_ssh_key_revisions
    ADD CONSTRAINT git_ssh_key_revisions_scope_owner_id_revision_key UNIQUE (scope, owner_id, revision);


--
-- Name: git_verified_head_observations git_verified_head_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_verified_head_observations
    ADD CONSTRAINT git_verified_head_observations_pkey PRIMARY KEY (binding_id, commit_revision, source, provider_request);


--
-- Name: git_webhook_tombstones git_webhook_tombstones_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_webhook_tombstones
    ADD CONSTRAINT git_webhook_tombstones_pkey PRIMARY KEY (provider, delivery_hash);


--
-- Name: git_webhook_tombstones git_webhook_tombstones_provider_repository_id_target_ref_af_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_webhook_tombstones
    ADD CONSTRAINT git_webhook_tombstones_provider_repository_id_target_ref_af_key UNIQUE (provider, repository_id, target_ref, after_commit);


--
-- Name: git_write_commands git_write_commands_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_pkey PRIMARY KEY (operation_id);


--
-- Name: github_installations github_installations_github_installation_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_installations
    ADD CONSTRAINT github_installations_github_installation_id_key UNIQUE (github_installation_id);


--
-- Name: github_installations github_installations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_installations
    ADD CONSTRAINT github_installations_pkey PRIMARY KEY (id);


--
-- Name: github_one_time_claims github_one_time_claims_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_one_time_claims
    ADD CONSTRAINT github_one_time_claims_pkey PRIMARY KEY (kind, claim_key);


--
-- Name: github_repositories github_repositories_id_installation_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_repositories
    ADD CONSTRAINT github_repositories_id_installation_id_key UNIQUE (id, installation_id);


--
-- Name: github_repositories github_repositories_installation_id_github_repository_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_repositories
    ADD CONSTRAINT github_repositories_installation_id_github_repository_id_key UNIQUE (installation_id, github_repository_id);


--
-- Name: github_repositories github_repositories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_repositories
    ADD CONSTRAINT github_repositories_pkey PRIMARY KEY (id);


--
-- Name: github_setup_authorizations github_setup_authorizations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_setup_authorizations
    ADD CONSTRAINT github_setup_authorizations_pkey PRIMARY KEY (actor_id, idempotency_key);


--
-- Name: github_setup_handoffs github_setup_handoffs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_setup_handoffs
    ADD CONSTRAINT github_setup_handoffs_pkey PRIMARY KEY (digest);


--
-- Name: github_user_bindings github_user_bindings_github_user_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_user_bindings
    ADD CONSTRAINT github_user_bindings_github_user_id_key UNIQUE (github_user_id);


--
-- Name: github_user_bindings github_user_bindings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_user_bindings
    ADD CONSTRAINT github_user_bindings_pkey PRIMARY KEY (user_id);


--
-- Name: github_webhook_receipts github_webhook_receipts_github_app_id_github_installation_i_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_webhook_receipts
    ADD CONSTRAINT github_webhook_receipts_github_app_id_github_installation_i_key UNIQUE (github_app_id, github_installation_id, delivery_id);


--
-- Name: github_webhook_receipts github_webhook_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_webhook_receipts
    ADD CONSTRAINT github_webhook_receipts_pkey PRIMARY KEY (claim_key);


--
-- Name: middleware_profile_references middleware_profile_references_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.middleware_profile_references
    ADD CONSTRAINT middleware_profile_references_pkey PRIMARY KEY (git_path, profile_id, logical_name);


--
-- Name: mutation_receipts mutation_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mutation_receipts
    ADD CONSTRAINT mutation_receipts_pkey PRIMARY KEY (actor_id, receipt_kind, namespace, scope_key, idempotency_key);


--
-- Name: operations operations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.operations
    ADD CONSTRAINT operations_pkey PRIMARY KEY (id);


--
-- Name: outbox outbox_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outbox
    ADD CONSTRAINT outbox_pkey PRIMARY KEY (operation_id);


--
-- Name: outbox_valkey_dataset outbox_valkey_dataset_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outbox_valkey_dataset
    ADD CONSTRAINT outbox_valkey_dataset_pkey PRIMARY KEY (singleton);


--
-- Name: preview_authorities preview_authorities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preview_authorities
    ADD CONSTRAINT preview_authorities_pkey PRIMARY KEY (token_hash);


--
-- Name: project_registry_pull_credentials project_registry_pull_credent_project_id_registry_target_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_registry_pull_credentials
    ADD CONSTRAINT project_registry_pull_credent_project_id_registry_target_id_key UNIQUE (project_id, registry_target_id);


--
-- Name: project_registry_pull_credentials project_registry_pull_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_registry_pull_credentials
    ADD CONSTRAINT project_registry_pull_credentials_pkey PRIMARY KEY (id);


--
-- Name: project_registry_pull_credentials project_registry_pull_credentials_project_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_registry_pull_credentials
    ADD CONSTRAINT project_registry_pull_credentials_project_id_name_key UNIQUE (project_id, name);


--
-- Name: projects projects_id_team_unique; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_id_team_unique UNIQUE (id, team_id);


--
-- Name: projects projects_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_pkey PRIMARY KEY (id);


--
-- Name: projects projects_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_slug_key UNIQUE (slug);


--
-- Name: registry_artifact_references registry_artifact_references_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_artifact_references
    ADD CONSTRAINT registry_artifact_references_pkey PRIMARY KEY (registry_target_id, service_id, kind, reference_key);


--
-- Name: registry_authority_observations registry_authority_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_authority_observations
    ADD CONSTRAINT registry_authority_observations_pkey PRIMARY KEY (registry_target_id, service_id, authority);


--
-- Name: registry_blobs registry_blobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_blobs
    ADD CONSTRAINT registry_blobs_pkey PRIMARY KEY (registry_target_id, repository, digest);


--
-- Name: registry_cache_generations registry_cache_generations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cache_generations
    ADD CONSTRAINT registry_cache_generations_pkey PRIMARY KEY (id);


--
-- Name: registry_cache_generations registry_cache_generations_registry_target_id_service_id_pl_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cache_generations
    ADD CONSTRAINT registry_cache_generations_registry_target_id_service_id_pl_key UNIQUE (registry_target_id, service_id, platform_set, trust_lane, cache_schema, build_definition_hash, generation);


--
-- Name: registry_catalog_observations registry_catalog_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_catalog_observations
    ADD CONSTRAINT registry_catalog_observations_pkey PRIMARY KEY (id);


--
-- Name: registry_catalog_observations registry_catalog_observations_registry_target_id_repository_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_catalog_observations
    ADD CONSTRAINT registry_catalog_observations_registry_target_id_repository_key UNIQUE (registry_target_id, repository, revision);


--
-- Name: registry_cleanup_items registry_cleanup_items_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_items
    ADD CONSTRAINT registry_cleanup_items_pkey PRIMARY KEY (plan_id, ordinal);


--
-- Name: registry_cleanup_items registry_cleanup_items_plan_id_repository_resource_kind_dig_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_items
    ADD CONSTRAINT registry_cleanup_items_plan_id_repository_resource_kind_dig_key UNIQUE (plan_id, repository, resource_kind, digest);


--
-- Name: registry_cleanup_leases registry_cleanup_leases_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_leases
    ADD CONSTRAINT registry_cleanup_leases_pkey PRIMARY KEY (registry_target_id, repository);


--
-- Name: registry_cleanup_plans registry_cleanup_plans_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_plans
    ADD CONSTRAINT registry_cleanup_plans_pkey PRIMARY KEY (id);


--
-- Name: registry_cleanup_plans registry_cleanup_plans_registry_target_id_service_id_plan_d_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_plans
    ADD CONSTRAINT registry_cleanup_plans_registry_target_id_service_id_plan_d_key UNIQUE (registry_target_id, service_id, plan_digest);


--
-- Name: registry_inventory_observations registry_inventory_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_inventory_observations
    ADD CONSTRAINT registry_inventory_observations_pkey PRIMARY KEY (registry_target_id);


--
-- Name: registry_manifest_blobs registry_manifest_blobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifest_blobs
    ADD CONSTRAINT registry_manifest_blobs_pkey PRIMARY KEY (registry_target_id, repository, manifest_digest, blob_digest);


--
-- Name: registry_manifest_children registry_manifest_children_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifest_children
    ADD CONSTRAINT registry_manifest_children_pkey PRIMARY KEY (registry_target_id, repository, parent_digest, child_digest);


--
-- Name: registry_manifests registry_manifests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifests
    ADD CONSTRAINT registry_manifests_pkey PRIMARY KEY (registry_target_id, repository, digest);


--
-- Name: registry_releases registry_releases_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_releases
    ADD CONSTRAINT registry_releases_pkey PRIMARY KEY (id);


--
-- Name: registry_runtime_gc_sweep_receipts registry_runtime_gc_sweep_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_runtime_gc_sweep_receipts
    ADD CONSTRAINT registry_runtime_gc_sweep_receipts_pkey PRIMARY KEY (registry_target_id, execution_key);


--
-- Name: registry_runtime_maintenance_executions registry_runtime_maintenance_executions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_runtime_maintenance_executions
    ADD CONSTRAINT registry_runtime_maintenance_executions_pkey PRIMARY KEY (registry_target_id, execution_key);


--
-- Name: registry_runtime_observation_cursors registry_runtime_observation_cursors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_runtime_observation_cursors
    ADD CONSTRAINT registry_runtime_observation_cursors_pkey PRIMARY KEY (registry_target_id);


--
-- Name: registry_targets registry_targets_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_targets
    ADD CONSTRAINT registry_targets_name_key UNIQUE (name);


--
-- Name: registry_targets registry_targets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_targets
    ADD CONSTRAINT registry_targets_pkey PRIMARY KEY (id);


--
-- Name: runtime_readiness runtime_readiness_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runtime_readiness
    ADD CONSTRAINT runtime_readiness_pkey PRIMARY KEY (runtime_kind, scope_key, worker_id);


--
-- Name: runtime_registry_pull_artifacts runtime_registry_pull_artifacts_namespace_secret_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runtime_registry_pull_artifacts
    ADD CONSTRAINT runtime_registry_pull_artifacts_namespace_secret_name_key UNIQUE (namespace, secret_name);


--
-- Name: runtime_registry_pull_artifacts runtime_registry_pull_artifacts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runtime_registry_pull_artifacts
    ADD CONSTRAINT runtime_registry_pull_artifacts_pkey PRIMARY KEY (environment_id, registry_target_id, profile_revision);


--
-- Name: secret_binding_deliveries secret_binding_deliveries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_deliveries
    ADD CONSTRAINT secret_binding_deliveries_pkey PRIMARY KEY (version_id, ordinal);


--
-- Name: secret_binding_events secret_binding_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_events
    ADD CONSTRAINT secret_binding_events_pkey PRIMARY KEY (id);


--
-- Name: secret_binding_references secret_binding_references_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_references
    ADD CONSTRAINT secret_binding_references_pkey PRIMARY KEY (binding_id, kind, reference_id);


--
-- Name: secret_binding_runtime_reconciliations secret_binding_runtime_reconciliation_version_id_binding_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_runtime_reconciliations
    ADD CONSTRAINT secret_binding_runtime_reconciliation_version_id_binding_id_key UNIQUE (version_id, binding_id);


--
-- Name: secret_binding_runtime_reconciliations secret_binding_runtime_reconciliations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_runtime_reconciliations
    ADD CONSTRAINT secret_binding_runtime_reconciliations_pkey PRIMARY KEY (version_id);


--
-- Name: secret_binding_versions secret_binding_versions_binding_id_version_number_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_versions
    ADD CONSTRAINT secret_binding_versions_binding_id_version_number_key UNIQUE (binding_id, version_number);


--
-- Name: secret_binding_versions secret_binding_versions_id_binding_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_versions
    ADD CONSTRAINT secret_binding_versions_id_binding_id_key UNIQUE (id, binding_id);


--
-- Name: secret_binding_versions secret_binding_versions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_versions
    ADD CONSTRAINT secret_binding_versions_pkey PRIMARY KEY (id);


--
-- Name: secret_bindings secret_bindings_id_provider_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_id_provider_key UNIQUE (id, provider);


--
-- Name: secret_bindings secret_bindings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_pkey PRIMARY KEY (id);


--
-- Name: secret_bindings secret_bindings_project_id_environment_id_application_id_na_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_project_id_environment_id_application_id_na_key UNIQUE (project_id, environment_id, application_id, name);


--
-- Name: service_account_tokens service_account_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_account_tokens
    ADD CONSTRAINT service_account_tokens_pkey PRIMARY KEY (id);


--
-- Name: service_account_tokens service_account_tokens_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_account_tokens
    ADD CONSTRAINT service_account_tokens_token_hash_key UNIQUE (token_hash);


--
-- Name: service_accounts service_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_accounts
    ADD CONSTRAINT service_accounts_pkey PRIMARY KEY (id);


--
-- Name: service_registry_policies service_registry_policies_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_registry_policies
    ADD CONSTRAINT service_registry_policies_pkey PRIMARY KEY (registry_target_id, service_id);


--
-- Name: service_registry_policies service_registry_policies_registry_target_id_repository_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_registry_policies
    ADD CONSTRAINT service_registry_policies_registry_target_id_repository_key UNIQUE (registry_target_id, repository);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (token_hash);


--
-- Name: team_memberships team_memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_pkey PRIMARY KEY (team_id, user_id);


--
-- Name: teams teams_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_pkey PRIMARY KEY (id);


--
-- Name: teams teams_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_slug_key UNIQUE (slug);


--
-- Name: tls_certificate_observations tls_certificate_observations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_certificate_observations
    ADD CONSTRAINT tls_certificate_observations_pkey PRIMARY KEY (version_id);


--
-- Name: tls_certificate_versions tls_certificate_versions_binding_id_version_number_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_certificate_versions
    ADD CONSTRAINT tls_certificate_versions_binding_id_version_number_key UNIQUE (binding_id, version_number);


--
-- Name: tls_certificate_versions tls_certificate_versions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_certificate_versions
    ADD CONSTRAINT tls_certificate_versions_pkey PRIMARY KEY (version_id);


--
-- Name: tls_certificate_versions tls_certificate_versions_version_id_binding_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_certificate_versions
    ADD CONSTRAINT tls_certificate_versions_version_id_binding_id_key UNIQUE (version_id, binding_id);


--
-- Name: user_invitations user_invitations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_invitations
    ADD CONSTRAINT user_invitations_pkey PRIMARY KEY (id);


--
-- Name: user_invitations user_invitations_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_invitations
    ADD CONSTRAINT user_invitations_token_hash_key UNIQUE (token_hash);


--
-- Name: user_password_credentials user_password_credentials_email_normalized_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_password_credentials
    ADD CONSTRAINT user_password_credentials_email_normalized_key UNIQUE (email_normalized);


--
-- Name: user_password_credentials user_password_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_password_credentials
    ADD CONSTRAINT user_password_credentials_pkey PRIMARY KEY (user_id);


--
-- Name: users users_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_email_key UNIQUE (email);


--
-- Name: users users_issuer_subject_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_issuer_subject_key UNIQUE (issuer, subject);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: access_grants_scope_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_grants_scope_idx ON public.access_grants USING btree (scope_type, scope_id, subject_user_id, subject_team_id);


--
-- Name: access_grants_team_subject_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_grants_team_subject_idx ON public.access_grants USING btree (subject_team_id, scope_type, scope_id) WHERE (subject_team_id IS NOT NULL);


--
-- Name: access_grants_team_subject_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX access_grants_team_subject_unique ON public.access_grants USING btree (subject_team_id, role, scope_type, scope_id) WHERE (subject_team_id IS NOT NULL);


--
-- Name: access_grants_user_subject_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_grants_user_subject_idx ON public.access_grants USING btree (subject_user_id, scope_type, scope_id) WHERE (subject_user_id IS NOT NULL);


--
-- Name: access_grants_user_subject_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX access_grants_user_subject_unique ON public.access_grants USING btree (subject_user_id, role, scope_type, scope_id) WHERE (subject_user_id IS NOT NULL);


--
-- Name: argo_desired_state_commands_binding_claim_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX argo_desired_state_commands_binding_claim_idx ON public.argo_desired_state_commands USING btree (platform_binding_id) WHERE (lease_owner IS NOT NULL);


--
-- Name: argo_desired_state_commands_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX argo_desired_state_commands_due_idx ON public.argo_desired_state_commands USING btree (next_attempt_at, created_at, id) WHERE (state = ANY (ARRAY['pending'::text, 'claimed'::text, 'git-committed'::text]));


--
-- Name: argo_desired_state_commands_environment_live_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX argo_desired_state_commands_environment_live_idx ON public.argo_desired_state_commands USING btree (environment_id) WHERE (state = ANY (ARRAY['pending'::text, 'claimed'::text, 'git-committed'::text]));


--
-- Name: argo_desired_state_commands_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX argo_desired_state_commands_status_idx ON public.argo_desired_state_commands USING btree (environment_id, generation DESC);


--
-- Name: argo_desired_state_materialization_command_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX argo_desired_state_materialization_command_idx ON public.argo_desired_state_materialization_receipts USING btree (desired_state_command_id, desired_state_generation, desired_state_revision);


--
-- Name: argo_desired_state_materialization_exact_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX argo_desired_state_materialization_exact_idx ON public.argo_desired_state_materialization_receipts USING btree (environment_binding_id, environment_revision, environment_generation, created_at DESC, id DESC);


--
-- Name: argo_observation_runtime_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX argo_observation_runtime_due_idx ON public.argo_observation_runtime USING btree (next_poll_at, argo_namespace);


--
-- Name: audit_events_target_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_events_target_idx ON public.audit_events USING btree (target_type, target_id, created_at);


--
-- Name: auto_deploy_runs_work_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX auto_deploy_runs_work_idx ON public.auto_deploy_runs USING btree (available_at, created_at, attempt_id, policy_id) WHERE (state = ANY (ARRAY['pending'::text, 'processing'::text]));


--
-- Name: build_attempts_service_cache_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX build_attempts_service_cache_idx ON public.build_attempts USING btree (project_id, service_id, definition_digest, generation DESC) WHERE ((state = 'succeeded'::text) AND (cache_reference <> ''::text));


--
-- Name: build_attempts_work_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX build_attempts_work_idx ON public.build_attempts USING btree (state, available_at, created_at) WHERE (state = ANY (ARRAY['queued'::text, 'preparing'::text, 'running'::text, 'cancelling'::text]));


--
-- Name: applications_build_source_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_build_source_id_key UNIQUE (build_source_id);


--
-- Name: applications_build_source_push_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX applications_build_source_push_idx ON public.applications USING btree (build_source_installation_id, build_source_repository_id, build_source_trigger_ref) WHERE (build_source_kind = 'github'::text);


--
-- Name: build_release_projections_work_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX build_release_projections_work_idx ON public.build_release_projections USING btree (available_at, created_at, attempt_id) WHERE (state = ANY (ARRAY['pending'::text, 'processing'::text]));


--
-- Name: builder_platform_setting_mutations_revision_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX builder_platform_setting_mutations_revision_idx ON public.builder_platform_setting_mutations USING btree (revision);


--
-- Name: cert_manager_issuer_references_profile_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cert_manager_issuer_references_profile_idx ON public.cert_manager_issuer_references USING btree (profile_id, git_path);


--
-- Name: configuration_profiles_catalog_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX configuration_profiles_catalog_idx ON public.configuration_profiles USING btree (kind, name, id);


--
-- Name: configuration_profiles_global_name_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX configuration_profiles_global_name_idx ON public.configuration_profiles USING btree (kind, name) WHERE (kind = ANY (ARRAY['scheduling'::text, 'certificate-issuer'::text]));


--
-- Name: deployment_operation_inputs_deployment_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deployment_operation_inputs_deployment_idx ON public.deployment_operation_inputs USING btree (deployment_id, created_at);


--
-- Name: deployments_environment_application_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX deployments_environment_application_unique ON public.deployments USING btree (environment_id, application_id);


--
-- Name: edge_runtime_targets_active_key_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX edge_runtime_targets_active_key_idx ON public.edge_runtime_targets USING btree (target_key) WHERE active;


--
-- Name: edge_runtime_targets_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX edge_runtime_targets_due_idx ON public.edge_runtime_targets USING btree (next_observation_at, target_key, profile_revision) WHERE (active AND (runtime_state <> 'failed'::text));


--
-- Name: edge_runtime_targets_readiness_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX edge_runtime_targets_readiness_idx ON public.edge_runtime_targets USING btree (runtime_config_digest, runtime_state, last_observed_at) WHERE active;


--
-- Name: edge_sslip_ingress_fresh_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX edge_sslip_ingress_fresh_idx ON public.edge_sslip_ingress_observations USING btree (runtime_config_digest, observed_at DESC);


--
-- Name: environment_app_placements_project_application_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX environment_app_placements_project_application_idx ON public.environment_app_placements USING btree (project_id, application_id);


--
-- Name: environment_foundation_active_environment_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX environment_foundation_active_environment_idx ON public.environment_foundation_intents USING btree (environment_id) WHERE active;


--
-- Name: environment_foundation_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX environment_foundation_due_idx ON public.environment_foundation_intents USING btree (next_attempt_at, id) WHERE (active AND (state = ANY (ARRAY['pending'::text, 'claimed'::text])));


--
-- Name: environment_foundation_exact_ready_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX environment_foundation_exact_ready_idx ON public.environment_foundation_intents USING btree (profile_digest, publisher_config_digest, state) WHERE active;


--
-- Name: external_dns_integration_environments_environment_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX external_dns_integration_environments_environment_idx ON public.external_dns_integration_environments USING btree (environment_id, integration_id);


--
-- Name: external_dns_integrations_active_runtime_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX external_dns_integrations_active_runtime_idx ON public.external_dns_integrations USING btree (runtime_revision, id) WHERE (lifecycle = 'active'::text);


--
-- Name: git_path_reservations_repair_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_path_reservations_repair_idx ON public.git_path_reservations USING btree (lease_until) WHERE (state = 'candidate'::text);


--
-- Name: git_projected_documents_application_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_projected_documents_application_idx ON public.git_projected_documents USING btree (application_id, binding_id, generation) WHERE (application_id IS NOT NULL);


--
-- Name: git_pull_request_publications_provider_pr_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX git_pull_request_publications_provider_pr_idx ON public.git_pull_request_publications USING btree (repository_id, pull_request_number) WHERE (pull_request_number > 0);


--
-- Name: git_pull_request_publications_reconcile_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_pull_request_publications_reconcile_idx ON public.git_pull_request_publications USING btree (state, updated_at, operation_id) WHERE (state = ANY (ARRAY['candidate-ready'::text, 'pull-request-open'::text, 'pull-request-closed'::text, 'merge-pending'::text]));


--
-- Name: git_repository_bindings_environment_authority; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX git_repository_bindings_environment_authority ON public.git_repository_bindings USING btree (environment_id) WHERE (kind = 'environment'::text);


--
-- Name: git_repository_bindings_platform_authority; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX git_repository_bindings_platform_authority ON public.git_repository_bindings USING btree ((true)) WHERE (kind = 'platform'::text);


--
-- Name: git_repository_bindings_work_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_repository_bindings_work_idx ON public.git_repository_bindings USING btree (state, updated_at, id) WHERE (state <> 'ready'::text);


--
-- Name: git_safety_poll_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_safety_poll_due_idx ON public.git_safety_poll_cursors USING btree (next_poll_at, binding_id);


--
-- Name: git_safety_poll_reconcile_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_safety_poll_reconcile_due_idx ON public.git_safety_poll_cursors USING btree (lease_until, next_poll_at, binding_id);


--
-- Name: git_ssh_key_mutation_receipts_revision_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_ssh_key_mutation_receipts_revision_idx ON public.git_ssh_key_mutation_receipts USING btree (scope, owner_id, key_revision);


--
-- Name: git_ssh_key_revisions_active_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX git_ssh_key_revisions_active_key ON public.git_ssh_key_revisions USING btree (scope, owner_id) WHERE (status = 'active'::text);


--
-- Name: git_ssh_key_revisions_owner_history_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_ssh_key_revisions_owner_history_idx ON public.git_ssh_key_revisions USING btree (scope, owner_id, revision DESC);


--
-- Name: git_verified_head_observations_latest_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_verified_head_observations_latest_idx ON public.git_verified_head_observations USING btree (binding_id, observed_at DESC);


--
-- Name: git_write_commands_binding_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_write_commands_binding_state_idx ON public.git_write_commands USING btree (binding_id, state, created_at, operation_id);


--
-- Name: git_write_commands_committed_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_write_commands_committed_idx ON public.git_write_commands USING btree (binding_id, committed_revision) WHERE (state = ANY (ARRAY['git-committed'::text, 'indexed'::text]));


--
-- Name: github_installations_owner_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_installations_owner_idx ON public.github_installations USING btree (owner_user_id, created_at);


--
-- Name: github_installations_provider_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_installations_provider_idx ON public.github_installations USING btree (github_app_id, github_installation_id, lifecycle);


--
-- Name: github_installations_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_installations_team_idx ON public.github_installations USING btree (team_id, created_at) WHERE (visibility = 'team'::text);


--
-- Name: github_one_time_claims_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_one_time_claims_expiry_idx ON public.github_one_time_claims USING btree (retain_until) WHERE (permanent = false);


--
-- Name: github_repositories_provider_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_repositories_provider_idx ON public.github_repositories USING btree (github_repository_id, installation_id, lifecycle);


--
-- Name: github_setup_authorizations_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_setup_authorizations_expiry_idx ON public.github_setup_authorizations USING btree (expires_at);


--
-- Name: github_setup_handoffs_actor_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_setup_handoffs_actor_idx ON public.github_setup_handoffs USING btree (actor_id, expires_at);


--
-- Name: github_webhook_receipts_work_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX github_webhook_receipts_work_idx ON public.github_webhook_receipts USING btree (state, available_at, received_at) WHERE (state = ANY (ARRAY['claimed'::text, 'processing'::text]));


--
-- Name: middleware_profile_references_profile_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX middleware_profile_references_profile_idx ON public.middleware_profile_references USING btree (profile_id, git_path);


--
-- Name: mutation_receipts_operation_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX mutation_receipts_operation_idx ON public.mutation_receipts USING btree (operation_id) WHERE (operation_id IS NOT NULL);


--
-- Name: mutation_receipts_resource_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX mutation_receipts_resource_idx ON public.mutation_receipts USING btree (receipt_kind, resource_type, resource_id);


--
-- Name: operations_status_lease_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX operations_status_lease_idx ON public.operations USING btree (status, lease_until, created_at);


--
-- Name: outbox_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX outbox_pending_idx ON public.outbox USING btree (available_at, created_at) WHERE (published_at IS NULL);


--
-- Name: preview_authorities_deployment_lookup_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX preview_authorities_deployment_lookup_idx ON public.preview_authorities USING btree (deployment_id, actor_id, expires_at DESC) WHERE (preview_kind = 'deployment-config'::text);


--
-- Name: preview_authorities_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX preview_authorities_expiry_idx ON public.preview_authorities USING btree (expires_at) WHERE (consumed_at IS NULL);


--
-- Name: projects_team_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX projects_team_idx ON public.projects USING btree (team_id, created_at);


--
-- Name: registry_artifact_references_digest_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_artifact_references_digest_idx ON public.registry_artifact_references USING btree (registry_target_id, repository, digest);


--
-- Name: registry_blobs_present_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_blobs_present_idx ON public.registry_blobs USING btree (registry_target_id, repository, present, last_observed_at);


--
-- Name: registry_cache_generations_lifecycle_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_cache_generations_lifecycle_idx ON public.registry_cache_generations USING btree (registry_target_id, service_id, platform_set, trust_lane, cache_schema, build_definition_hash, state, generation DESC);


--
-- Name: registry_catalog_observations_latest_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_catalog_observations_latest_idx ON public.registry_catalog_observations USING btree (registry_target_id, repository, revision DESC);


--
-- Name: registry_cleanup_leases_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_cleanup_leases_expiry_idx ON public.registry_cleanup_leases USING btree (lease_until);


--
-- Name: registry_cleanup_plans_one_executing_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX registry_cleanup_plans_one_executing_idx ON public.registry_cleanup_plans USING btree (registry_target_id, service_id) WHERE (state = 'executing'::text);


--
-- Name: registry_manifest_blobs_blob_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_manifest_blobs_blob_idx ON public.registry_manifest_blobs USING btree (registry_target_id, repository, blob_digest);


--
-- Name: registry_manifest_children_child_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_manifest_children_child_idx ON public.registry_manifest_children USING btree (registry_target_id, repository, child_digest);


--
-- Name: registry_manifests_present_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_manifests_present_idx ON public.registry_manifests USING btree (registry_target_id, repository, present, last_observed_at);


--
-- Name: registry_releases_digest_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_releases_digest_idx ON public.registry_releases USING btree (registry_target_id, repository, root_digest);


--
-- Name: registry_releases_retention_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_releases_retention_idx ON public.registry_releases USING btree (registry_target_id, service_id, succeeded_at DESC, id DESC) WHERE (succeeded_at IS NOT NULL);


--
-- Name: registry_runtime_maintenance_lease_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_runtime_maintenance_lease_idx ON public.registry_runtime_maintenance_executions USING btree (lease_until, registry_target_id, execution_key) WHERE (released_at IS NULL);


--
-- Name: registry_runtime_maintenance_target_exclusive_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX registry_runtime_maintenance_target_exclusive_idx ON public.registry_runtime_maintenance_executions USING btree (registry_target_id) WHERE (released_at IS NULL);


--
-- Name: registry_runtime_observation_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX registry_runtime_observation_due_idx ON public.registry_runtime_observation_cursors USING btree (lease_until, next_observe_at, registry_target_id);


--
-- Name: runtime_readiness_certificate_config_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX runtime_readiness_certificate_config_idx ON public.runtime_readiness USING btree (runtime_kind, config_digest) WHERE (runtime_kind = 'certificate-issuer-observer'::text);


--
-- Name: runtime_readiness_match_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX runtime_readiness_match_idx ON public.runtime_readiness USING btree (runtime_kind, scope_key, contract_version, config_digest, observed_at DESC, lease_until);


--
-- Name: runtime_registry_pull_artifacts_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX runtime_registry_pull_artifacts_due_idx ON public.runtime_registry_pull_artifacts USING btree (next_observation_at, environment_id, registry_target_id) WHERE (active AND (runtime_state <> 'failed'::text));


--
-- Name: runtime_registry_pull_artifacts_one_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX runtime_registry_pull_artifacts_one_active_idx ON public.runtime_registry_pull_artifacts USING btree (environment_id, registry_target_id) WHERE active;


--
-- Name: secret_binding_delivery_environment_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX secret_binding_delivery_environment_unique ON public.secret_binding_deliveries USING btree (version_id, environment_name) WHERE (kind = 'environment'::text);


--
-- Name: secret_binding_delivery_file_unique; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX secret_binding_delivery_file_unique ON public.secret_binding_deliveries USING btree (version_id, file_path) WHERE (kind = 'file'::text);


--
-- Name: secret_binding_events_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX secret_binding_events_pending_idx ON public.secret_binding_events USING btree (occurred_at, id) WHERE (published_at IS NULL);


--
-- Name: secret_binding_one_active_version; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX secret_binding_one_active_version ON public.secret_binding_versions USING btree (binding_id) WHERE (state = 'active'::text);


--
-- Name: secret_binding_one_pending_version; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX secret_binding_one_pending_version ON public.secret_binding_versions USING btree (binding_id) WHERE (state = ANY (ARRAY['staging'::text, 'awaiting-readiness'::text]));


--
-- Name: secret_binding_references_version_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX secret_binding_references_version_idx ON public.secret_binding_references USING btree (version_id, kind, reference_id);


--
-- Name: secret_binding_runtime_reconcile_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX secret_binding_runtime_reconcile_due_idx ON public.secret_binding_runtime_reconciliations USING btree (next_attempt_at, version_id) WHERE (runtime_state = 'awaiting'::text);


--
-- Name: secret_bindings_scope_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX secret_bindings_scope_idx ON public.secret_bindings USING btree (organization_id, project_id, environment_id, application_id, created_at, id);


--
-- Name: service_account_tokens_account_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX service_account_tokens_account_created_idx ON public.service_account_tokens USING btree (service_account_id, created_at, id);


--
-- Name: service_account_tokens_active_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX service_account_tokens_active_expiry_idx ON public.service_account_tokens USING btree (expires_at) WHERE (revoked_at IS NULL);


--
-- Name: service_accounts_project_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX service_accounts_project_created_idx ON public.service_accounts USING btree (project_id, created_at, id);


--
-- Name: service_accounts_project_name_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX service_accounts_project_name_idx ON public.service_accounts USING btree (project_id, lower(name));


--
-- Name: sessions_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_expires_idx ON public.sessions USING btree (expires_at);


--
-- Name: team_memberships_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX team_memberships_user_idx ON public.team_memberships USING btree (user_id, team_id);


--
-- Name: tls_certificate_observations_binding_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tls_certificate_observations_binding_idx ON public.tls_certificate_observations USING btree (binding_id, version_id, state, last_ready_at);


--
-- Name: tls_certificate_observations_claim_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tls_certificate_observations_claim_idx ON public.tls_certificate_observations USING btree (next_observation_at, version_id) WHERE (lease_owner IS NULL);


--
-- Name: tls_certificate_versions_binding_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tls_certificate_versions_binding_idx ON public.tls_certificate_versions USING btree (binding_id, version_number);


--
-- Name: user_invitations_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_invitations_expires_idx ON public.user_invitations USING btree (expires_at) WHERE (accepted_at IS NULL);


--
-- Name: applications application_registry_pull_selection_scope; Type: TRIGGER; Schema: public; Owner: -
--

CREATE CONSTRAINT TRIGGER application_registry_pull_selection_scope AFTER INSERT OR UPDATE OF project_id, registry_pull_mode, registry_pull_project_credential_id ON public.applications DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.enforce_application_registry_pull_selection_scope();


--
-- Name: applications applications_configuration_assignment_restrict; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER applications_configuration_assignment_restrict BEFORE DELETE ON public.applications FOR EACH ROW EXECUTE FUNCTION public.protect_configuration_assigned_scope('application');


--
-- Name: argo_desired_state_commands argo_desired_state_app_project_content_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER argo_desired_state_app_project_content_validate BEFORE INSERT OR UPDATE ON public.argo_desired_state_commands FOR EACH ROW EXECUTE FUNCTION public.validate_argo_desired_state_app_project_content();


--
-- Name: argo_desired_state_commands argo_desired_state_commands_fence_legacy_recovery; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER argo_desired_state_commands_fence_legacy_recovery BEFORE UPDATE ON public.argo_desired_state_commands FOR EACH ROW EXECUTE FUNCTION public.fence_legacy_argo_desired_state_recovery();


--
-- Name: argo_desired_state_commands argo_desired_state_commands_require_policy_digest; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER argo_desired_state_commands_require_policy_digest BEFORE INSERT ON public.argo_desired_state_commands FOR EACH ROW EXECUTE FUNCTION public.require_argo_desired_state_policy_digest();


--
-- Name: argo_desired_state_commands argo_desired_state_commands_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER argo_desired_state_commands_validate BEFORE INSERT OR UPDATE ON public.argo_desired_state_commands FOR EACH ROW EXECUTE FUNCTION public.validate_argo_desired_state_command();


--
-- Name: argo_desired_state_commands argo_desired_state_materialization_on_verified; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER argo_desired_state_materialization_on_verified AFTER INSERT OR UPDATE OF state ON public.argo_desired_state_commands FOR EACH ROW EXECUTE FUNCTION public.record_verified_argo_desired_state_materialization();


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materialization_receipts_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER argo_desired_state_materialization_receipts_validate BEFORE INSERT OR DELETE OR UPDATE ON public.argo_desired_state_materialization_receipts FOR EACH ROW EXECUTE FUNCTION public.validate_argo_desired_state_materialization_receipt();


--
-- Name: argo_desired_state_materialization_receipts argo_materialization_app_project_content_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER argo_materialization_app_project_content_validate BEFORE INSERT OR DELETE OR UPDATE ON public.argo_desired_state_materialization_receipts FOR EACH ROW EXECUTE FUNCTION public.validate_argo_materialization_app_project_content();


--
-- Name: auto_deploy_policies auto_deploy_policies_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER auto_deploy_policies_protect BEFORE UPDATE ON public.auto_deploy_policies FOR EACH ROW EXECUTE FUNCTION public.protect_auto_deploy_policy();


--
-- Name: auto_deploy_policy_revisions auto_deploy_policy_revision_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE CONSTRAINT TRIGGER auto_deploy_policy_revision_validate AFTER INSERT ON public.auto_deploy_policy_revisions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.validate_auto_deploy_policy_revision();


--
-- Name: auto_deploy_policy_revisions auto_deploy_revisions_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER auto_deploy_revisions_immutable BEFORE DELETE OR UPDATE ON public.auto_deploy_policy_revisions FOR EACH ROW EXECUTE FUNCTION public.reject_auto_deploy_immutable_change();


--
-- Name: auto_deploy_runs auto_deploy_runs_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER auto_deploy_runs_protect BEFORE INSERT OR DELETE OR UPDATE ON public.auto_deploy_runs FOR EACH ROW EXECUTE FUNCTION public.protect_auto_deploy_run();


--
-- Name: build_attempts build_attempts_enqueue_release_projection; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER build_attempts_enqueue_release_projection AFTER INSERT OR UPDATE OF state ON public.build_attempts FOR EACH ROW EXECUTE FUNCTION public.enqueue_build_release_projection();


--
-- Name: build_release_projections build_release_enqueue_auto_deploy; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER build_release_enqueue_auto_deploy AFTER INSERT OR UPDATE OF state ON public.build_release_projections FOR EACH ROW EXECUTE FUNCTION public.enqueue_auto_deploy_runs();


--
-- Name: cert_manager_issuer_observations cert_manager_issuer_observations_profile_kind; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER cert_manager_issuer_observations_profile_kind BEFORE INSERT OR UPDATE ON public.cert_manager_issuer_observations FOR EACH ROW EXECUTE FUNCTION public.validate_certificate_issuer_profile_child();


--
-- Name: cert_manager_issuer_references cert_manager_issuer_reference_scope; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER cert_manager_issuer_reference_scope BEFORE INSERT ON public.cert_manager_issuer_references FOR EACH ROW EXECUTE FUNCTION public.validate_cert_manager_issuer_reference_scope();


--
-- Name: cert_manager_issuer_references cert_manager_issuer_references_profile_kind; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER cert_manager_issuer_references_profile_kind BEFORE INSERT OR UPDATE ON public.cert_manager_issuer_references FOR EACH ROW EXECUTE FUNCTION public.validate_certificate_issuer_profile_child();


--
-- Name: configuration_profile_assignments configuration_profile_assignment_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER configuration_profile_assignment_validate BEFORE INSERT ON public.configuration_profile_assignments FOR EACH ROW EXECUTE FUNCTION public.validate_configuration_profile_assignment();


--
-- Name: configuration_profile_assignments configuration_profile_assignments_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER configuration_profile_assignments_immutable BEFORE DELETE OR UPDATE ON public.configuration_profile_assignments FOR EACH ROW EXECUTE FUNCTION public.reject_configuration_profile_immutable_change();


--
-- Name: configuration_profile_revisions configuration_profile_revision_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER configuration_profile_revision_validate BEFORE INSERT ON public.configuration_profile_revisions FOR EACH ROW EXECUTE FUNCTION public.validate_configuration_profile_revision();


--
-- Name: configuration_profile_revisions configuration_profile_revisions_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER configuration_profile_revisions_immutable BEFORE DELETE OR UPDATE ON public.configuration_profile_revisions FOR EACH ROW EXECUTE FUNCTION public.reject_configuration_profile_immutable_change();


--
-- Name: configuration_profiles configuration_profiles_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER configuration_profiles_protect BEFORE UPDATE ON public.configuration_profiles FOR EACH ROW EXECUTE FUNCTION public.protect_configuration_profile();


--
-- Name: edge_runtime_targets edge_runtime_target_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER edge_runtime_target_validate BEFORE INSERT OR UPDATE ON public.edge_runtime_targets FOR EACH ROW EXECUTE FUNCTION public.validate_edge_runtime_target();


--
-- Name: edge_sslip_ingress_observations edge_sslip_ingress_observation_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER edge_sslip_ingress_observation_protect BEFORE INSERT OR DELETE OR UPDATE ON public.edge_sslip_ingress_observations FOR EACH ROW EXECUTE FUNCTION public.protect_edge_sslip_ingress_observation();


--
-- Name: environment_foundation_intents environment_foundation_intent_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER environment_foundation_intent_protect BEFORE UPDATE ON public.environment_foundation_intents FOR EACH ROW EXECUTE FUNCTION public.protect_environment_foundation_intent();


--
-- Name: environments environments_configuration_assignment_restrict; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER environments_configuration_assignment_restrict BEFORE DELETE ON public.environments FOR EACH ROW EXECUTE FUNCTION public.protect_configuration_assigned_scope('environment');


--
-- Name: environments environments_protection_policy_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER environments_protection_policy_immutable BEFORE UPDATE ON public.environments FOR EACH ROW EXECUTE FUNCTION public.protect_environment_protection_policy();


--
-- Name: external_dns_integrations external_dns_integrations_identity; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER external_dns_integrations_identity BEFORE UPDATE ON public.external_dns_integrations FOR EACH ROW EXECUTE FUNCTION public.protect_external_dns_integration_identity();


--
-- Name: git_pull_request_publications git_pull_request_publications_closed_command; Type: TRIGGER; Schema: public; Owner: -
--

CREATE CONSTRAINT TRIGGER git_pull_request_publications_closed_command AFTER INSERT OR UPDATE ON public.git_pull_request_publications DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.require_closed_git_publication_command();


--
-- Name: git_pull_request_publications git_pull_request_publications_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER git_pull_request_publications_protect BEFORE INSERT OR UPDATE ON public.git_pull_request_publications FOR EACH ROW EXECUTE FUNCTION public.protect_git_pull_request_publication();


--
-- Name: git_projection_push_wake_targets git_push_wake_targets_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER git_push_wake_targets_immutable BEFORE DELETE OR UPDATE ON public.git_projection_push_wake_targets FOR EACH ROW EXECUTE FUNCTION public.reject_git_push_wake_change();


--
-- Name: git_projection_push_wakes git_push_wakes_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER git_push_wakes_immutable BEFORE DELETE OR UPDATE ON public.git_projection_push_wakes FOR EACH ROW EXECUTE FUNCTION public.reject_git_push_wake_change();


--
-- Name: git_repository_bindings git_repository_bindings_identity; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER git_repository_bindings_identity BEFORE UPDATE ON public.git_repository_bindings FOR EACH ROW EXECUTE FUNCTION public.protect_git_binding_identity();


--
-- Name: git_safety_poll_cursors git_safety_poll_lease_epoch; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER git_safety_poll_lease_epoch BEFORE UPDATE ON public.git_safety_poll_cursors FOR EACH ROW EXECUTE FUNCTION public.protect_git_reconciliation_lease_epoch();


--
-- Name: git_webhook_tombstones git_webhook_tombstones_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER git_webhook_tombstones_protect BEFORE DELETE OR UPDATE ON public.git_webhook_tombstones FOR EACH ROW EXECUTE FUNCTION public.protect_git_webhook_tombstone();


--
-- Name: git_write_commands git_write_commands_operation; Type: TRIGGER; Schema: public; Owner: -
--

CREATE CONSTRAINT TRIGGER git_write_commands_operation AFTER INSERT ON public.git_write_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.validate_git_write_operation();


--
-- Name: git_write_commands git_write_commands_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER git_write_commands_protect BEFORE INSERT OR DELETE OR UPDATE ON public.git_write_commands FOR EACH ROW EXECUTE FUNCTION public.protect_git_write_command();


--
-- Name: github_one_time_claims github_one_time_claims_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER github_one_time_claims_protect BEFORE DELETE OR UPDATE ON public.github_one_time_claims FOR EACH ROW EXECUTE FUNCTION public.protect_permanent_github_claim();


--
-- Name: audit_events managed_audit_events_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER managed_audit_events_protect BEFORE INSERT OR DELETE OR UPDATE ON public.audit_events FOR EACH ROW EXECUTE FUNCTION public.protect_managed_audit_event();


--
-- Name: middleware_profile_references middleware_profile_reference_scope; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER middleware_profile_reference_scope BEFORE INSERT ON public.middleware_profile_references FOR EACH ROW EXECUTE FUNCTION public.validate_middleware_profile_reference_scope();


--
-- Name: mutation_receipts mutation_receipts_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER mutation_receipts_validate BEFORE INSERT OR DELETE OR UPDATE ON public.mutation_receipts FOR EACH ROW EXECUTE FUNCTION public.validate_mutation_receipt();


--
-- Name: preview_authorities preview_authorities_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER preview_authorities_protect BEFORE INSERT OR DELETE OR UPDATE ON public.preview_authorities FOR EACH ROW EXECUTE FUNCTION public.protect_preview_authority();


--
-- Name: project_registry_pull_credentials project_registry_pull_target; Type: TRIGGER; Schema: public; Owner: -
--

CREATE CONSTRAINT TRIGGER project_registry_pull_target AFTER INSERT OR UPDATE ON public.project_registry_pull_credentials DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.enforce_project_registry_pull_target();


--
-- Name: projects projects_configuration_assignment_restrict; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER projects_configuration_assignment_restrict BEFORE DELETE ON public.projects FOR EACH ROW EXECUTE FUNCTION public.protect_configuration_assigned_scope('project');


--
-- Name: deployments protected_deployment_desired_revision_authority; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER protected_deployment_desired_revision_authority BEFORE INSERT OR UPDATE OF state, desired_revision, operation_id, generation ON public.deployments FOR EACH ROW EXECUTE FUNCTION public.enforce_protected_deployment_desired_revision();


--
-- Name: registry_cleanup_plans registry_cleanup_plans_managed_only; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER registry_cleanup_plans_managed_only BEFORE INSERT OR UPDATE OF registry_target_id ON public.registry_cleanup_plans FOR EACH ROW EXECUTE FUNCTION public.reject_external_registry_cleanup_plan();


--
-- Name: registry_runtime_gc_sweep_receipts registry_runtime_gc_receipt_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER registry_runtime_gc_receipt_immutable BEFORE DELETE OR UPDATE ON public.registry_runtime_gc_sweep_receipts FOR EACH ROW EXECUTE FUNCTION public.protect_registry_runtime_gc_receipt();


--
-- Name: registry_runtime_maintenance_executions registry_runtime_maintenance_epoch; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER registry_runtime_maintenance_epoch BEFORE UPDATE ON public.registry_runtime_maintenance_executions FOR EACH ROW EXECUTE FUNCTION public.protect_registry_runtime_maintenance_epoch();


--
-- Name: registry_runtime_maintenance_executions registry_runtime_maintenance_target; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER registry_runtime_maintenance_target BEFORE INSERT OR UPDATE ON public.registry_runtime_maintenance_executions FOR EACH ROW EXECUTE FUNCTION public.validate_registry_runtime_maintenance_target();


--
-- Name: registry_runtime_observation_cursors registry_runtime_observation_epoch; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER registry_runtime_observation_epoch BEFORE UPDATE ON public.registry_runtime_observation_cursors FOR EACH ROW EXECUTE FUNCTION public.protect_registry_runtime_observation_epoch();


--
-- Name: registry_targets registry_targets_mode_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER registry_targets_mode_immutable BEFORE UPDATE OF mode ON public.registry_targets FOR EACH ROW EXECUTE FUNCTION public.reject_registry_target_mode_change();


--
-- Name: runtime_readiness runtime_readiness_tls_certificate_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER runtime_readiness_tls_certificate_validate BEFORE INSERT OR DELETE OR UPDATE ON public.runtime_readiness FOR EACH ROW EXECUTE FUNCTION public.validate_tls_certificate_runtime_readiness();


--
-- Name: runtime_readiness runtime_readiness_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER runtime_readiness_validate BEFORE INSERT OR DELETE OR UPDATE ON public.runtime_readiness FOR EACH ROW EXECUTE FUNCTION public.validate_runtime_readiness();


--
-- Name: runtime_registry_pull_artifacts runtime_registry_pull_artifacts_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER runtime_registry_pull_artifacts_validate BEFORE INSERT OR UPDATE ON public.runtime_registry_pull_artifacts FOR EACH ROW EXECUTE FUNCTION public.validate_runtime_registry_pull_artifact();


--
-- Name: secret_binding_deliveries secret_binding_deliveries_immutable; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_binding_deliveries_immutable BEFORE DELETE OR UPDATE ON public.secret_binding_deliveries FOR EACH ROW EXECUTE FUNCTION public.reject_secret_binding_delivery_mutation();


--
-- Name: secret_binding_events secret_binding_events_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_binding_events_protect BEFORE DELETE OR UPDATE ON public.secret_binding_events FOR EACH ROW EXECUTE FUNCTION public.protect_secret_binding_event();


--
-- Name: secret_binding_references secret_binding_references_no_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_binding_references_no_update BEFORE UPDATE ON public.secret_binding_references FOR EACH ROW EXECUTE FUNCTION public.reject_secret_binding_reference_update();


--
-- Name: secret_binding_runtime_reconciliations secret_binding_runtime_reconcile_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_binding_runtime_reconcile_validate BEFORE INSERT OR UPDATE ON public.secret_binding_runtime_reconciliations FOR EACH ROW EXECUTE FUNCTION public.validate_secret_binding_runtime_reconciliation();


--
-- Name: secret_binding_versions secret_binding_versions_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_binding_versions_protect BEFORE UPDATE ON public.secret_binding_versions FOR EACH ROW EXECUTE FUNCTION public.protect_secret_binding_version();


--
-- Name: secret_binding_versions secret_binding_versions_target_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_binding_versions_target_protect BEFORE INSERT OR UPDATE ON public.secret_binding_versions FOR EACH ROW EXECUTE FUNCTION public.enforce_secret_binding_version_target();


--
-- Name: secret_bindings secret_bindings_identity; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_bindings_identity BEFORE UPDATE ON public.secret_bindings FOR EACH ROW EXECUTE FUNCTION public.protect_secret_binding_identity();


--
-- Name: secret_bindings secret_bindings_purpose_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_bindings_purpose_protect BEFORE UPDATE ON public.secret_bindings FOR EACH ROW EXECUTE FUNCTION public.protect_secret_binding_purpose();


--
-- Name: secret_bindings secret_bindings_scope; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER secret_bindings_scope BEFORE INSERT OR UPDATE ON public.secret_bindings FOR EACH ROW EXECUTE FUNCTION public.enforce_secret_binding_scope();


--
-- Name: teams teams_configuration_assignment_restrict; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER teams_configuration_assignment_restrict BEFORE DELETE ON public.teams FOR EACH ROW EXECUTE FUNCTION public.protect_configuration_assigned_scope('team');


--
-- Name: tls_certificate_observations tls_certificate_observations_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER tls_certificate_observations_protect BEFORE INSERT OR DELETE OR UPDATE ON public.tls_certificate_observations FOR EACH ROW EXECUTE FUNCTION public.protect_tls_certificate_observation();


--
-- Name: tls_certificate_versions tls_certificate_versions_protect; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER tls_certificate_versions_protect BEFORE DELETE OR UPDATE ON public.tls_certificate_versions FOR EACH ROW EXECUTE FUNCTION public.protect_tls_certificate_version();


--
-- Name: tls_certificate_versions tls_certificate_versions_validate; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER tls_certificate_versions_validate BEFORE INSERT ON public.tls_certificate_versions FOR EACH ROW EXECUTE FUNCTION public.validate_tls_certificate_version();


--
-- Name: access_grants access_grants_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_grants
    ADD CONSTRAINT access_grants_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: access_grants access_grants_subject_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_grants
    ADD CONSTRAINT access_grants_subject_team_id_fkey FOREIGN KEY (subject_team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: access_grants access_grants_subject_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_grants
    ADD CONSTRAINT access_grants_subject_user_id_fkey FOREIGN KEY (subject_user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: applications applications_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: applications applications_build_source_installation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_build_source_installation_id_fkey FOREIGN KEY (build_source_installation_id) REFERENCES public.github_installations(id) ON DELETE RESTRICT;


--
-- Name: applications applications_build_source_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_build_source_registry_target_id_fkey FOREIGN KEY (build_source_registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: applications applications_build_source_repository_installation_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_build_source_repository_installation_fkey FOREIGN KEY (build_source_repository_id, build_source_installation_id) REFERENCES public.github_repositories(id, installation_id) ON DELETE RESTRICT;


--
-- Name: applications applications_registry_pull_project_credential_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_registry_pull_project_credential_id_fkey FOREIGN KEY (registry_pull_project_credential_id) REFERENCES public.project_registry_pull_credentials(id) ON DELETE RESTRICT;


--
-- Name: applications applications_registry_pull_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.applications
    ADD CONSTRAINT applications_registry_pull_updated_by_fkey FOREIGN KEY (registry_pull_updated_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: argo_application_observations argo_application_observations_application_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_application_id_fkey FOREIGN KEY (application_id) REFERENCES public.applications(id) ON DELETE CASCADE;


--
-- Name: argo_application_observations argo_application_observations_application_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_application_id_project_id_fkey FOREIGN KEY (application_id, project_id) REFERENCES public.applications(id, project_id) ON DELETE CASCADE;


--
-- Name: argo_application_observations argo_application_observations_deployment_identity_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_deployment_identity_fk FOREIGN KEY (deployment_id, application_id, environment_id) REFERENCES public.deployments(id, application_id, environment_id) ON DELETE CASCADE;


--
-- Name: argo_application_observations argo_application_observations_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE CASCADE;


--
-- Name: argo_application_observations argo_application_observations_environment_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_environment_id_project_id_fkey FOREIGN KEY (environment_id, project_id) REFERENCES public.environments(id, project_id) ON DELETE CASCADE;


--
-- Name: argo_application_observations argo_application_observations_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_application_observations
    ADD CONSTRAINT argo_application_observations_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: argo_desired_state_commands argo_desired_state_commands_environment_binding_id_environ_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_environment_binding_id_environ_fkey FOREIGN KEY (environment_binding_id, environment_target_ref) REFERENCES public.git_repository_bindings(id, target_ref) ON DELETE CASCADE;


--
-- Name: argo_desired_state_commands argo_desired_state_commands_environment_binding_id_project_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_environment_binding_id_project_fkey FOREIGN KEY (environment_binding_id, project_id, environment_id) REFERENCES public.git_repository_bindings(id, project_id, environment_id) ON DELETE CASCADE;


--
-- Name: argo_desired_state_commands argo_desired_state_commands_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: argo_desired_state_commands argo_desired_state_commands_environment_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_environment_id_project_id_fkey FOREIGN KEY (environment_id, project_id) REFERENCES public.environments(id, project_id) ON DELETE RESTRICT;


--
-- Name: argo_desired_state_commands argo_desired_state_commands_platform_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_platform_binding_id_fkey FOREIGN KEY (platform_binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT;


--
-- Name: argo_desired_state_commands argo_desired_state_commands_platform_binding_id_platform_t_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_platform_binding_id_platform_t_fkey FOREIGN KEY (platform_binding_id, platform_target_ref) REFERENCES public.git_repository_bindings(id, target_ref) ON DELETE RESTRICT;


--
-- Name: argo_desired_state_commands argo_desired_state_commands_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_commands
    ADD CONSTRAINT argo_desired_state_commands_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materializatio_desired_state_command_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_materialization_receipts
    ADD CONSTRAINT argo_desired_state_materializatio_desired_state_command_id_fkey FOREIGN KEY (desired_state_command_id) REFERENCES public.argo_desired_state_commands(id);


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materialization__environment_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_materialization_receipts
    ADD CONSTRAINT argo_desired_state_materialization__environment_binding_id_fkey FOREIGN KEY (environment_binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materialization_generation_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_materialization_receipts
    ADD CONSTRAINT argo_desired_state_materialization_generation_fk FOREIGN KEY (environment_binding_id, environment_generation) REFERENCES public.git_projection_generations(binding_id, generation);


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materialization_rec_platform_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_materialization_receipts
    ADD CONSTRAINT argo_desired_state_materialization_rec_platform_binding_id_fkey FOREIGN KEY (platform_binding_id) REFERENCES public.git_repository_bindings(id);


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materialization_receipts_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_materialization_receipts
    ADD CONSTRAINT argo_desired_state_materialization_receipts_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id);


--
-- Name: argo_desired_state_materialization_receipts argo_desired_state_materialization_receipts_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_desired_state_materialization_receipts
    ADD CONSTRAINT argo_desired_state_materialization_receipts_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id);


--
-- Name: argo_rollback_commands argo_rollback_commands_application_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_application_id_fkey FOREIGN KEY (application_id) REFERENCES public.applications(id) ON DELETE RESTRICT;


--
-- Name: argo_rollback_commands argo_rollback_commands_application_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_application_id_project_id_fkey FOREIGN KEY (application_id, project_id) REFERENCES public.applications(id, project_id) ON DELETE RESTRICT;


--
-- Name: argo_rollback_commands argo_rollback_commands_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: argo_rollback_commands argo_rollback_commands_binding_id_project_id_environment_i_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_binding_id_project_id_environment_i_fkey FOREIGN KEY (binding_id, project_id, environment_id) REFERENCES public.git_repository_bindings(id, project_id, environment_id) ON DELETE CASCADE;


--
-- Name: argo_rollback_commands argo_rollback_commands_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: argo_rollback_commands argo_rollback_commands_environment_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_environment_id_project_id_fkey FOREIGN KEY (environment_id, project_id) REFERENCES public.environments(id, project_id) ON DELETE RESTRICT;


--
-- Name: argo_rollback_commands argo_rollback_commands_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.argo_rollback_commands
    ADD CONSTRAINT argo_rollback_commands_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: audit_events audit_events_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policies auto_deploy_policies_application_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policies
    ADD CONSTRAINT auto_deploy_policies_application_id_project_id_fkey FOREIGN KEY (application_id, project_id) REFERENCES public.applications(id, project_id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policies auto_deploy_policies_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policies
    ADD CONSTRAINT auto_deploy_policies_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policies auto_deploy_policies_environment_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policies
    ADD CONSTRAINT auto_deploy_policies_environment_id_project_id_fkey FOREIGN KEY (environment_id, project_id) REFERENCES public.environments(id, project_id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policies auto_deploy_policies_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policies
    ADD CONSTRAINT auto_deploy_policies_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policies auto_deploy_policy_current_revision_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policies
    ADD CONSTRAINT auto_deploy_policy_current_revision_fk FOREIGN KEY (id, current_revision) REFERENCES public.auto_deploy_policy_revisions(policy_id, revision) DEFERRABLE INITIALLY DEFERRED;


--
-- Name: auto_deploy_policy_revisions auto_deploy_policy_revisions_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policy_revisions
    ADD CONSTRAINT auto_deploy_policy_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policy_revisions auto_deploy_policy_revisions_policy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policy_revisions
    ADD CONSTRAINT auto_deploy_policy_revisions_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES public.auto_deploy_policies(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policy_revisions auto_deploy_policy_revisions_service_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policy_revisions
    ADD CONSTRAINT auto_deploy_policy_revisions_service_actor_id_fkey FOREIGN KEY (service_actor_id) REFERENCES public.service_accounts(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_policy_revisions auto_deploy_policy_revisions_source_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_policy_revisions
    ADD CONSTRAINT auto_deploy_policy_revisions_source_deployment_id_fkey FOREIGN KEY (source_deployment_id) REFERENCES public.deployments(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_runs auto_deploy_runs_attempt_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES public.build_attempts(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_runs auto_deploy_runs_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_runs auto_deploy_runs_operation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_runs auto_deploy_runs_policy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES public.auto_deploy_policies(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_runs auto_deploy_runs_policy_id_policy_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_policy_id_policy_revision_fkey FOREIGN KEY (policy_id, policy_revision) REFERENCES public.auto_deploy_policy_revisions(policy_id, revision) ON DELETE RESTRICT;


--
-- Name: auto_deploy_runs auto_deploy_runs_release_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_release_id_fkey FOREIGN KEY (release_id) REFERENCES public.registry_releases(id) ON DELETE RESTRICT;


--
-- Name: auto_deploy_runs auto_deploy_runs_source_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auto_deploy_runs
    ADD CONSTRAINT auto_deploy_runs_source_deployment_id_fkey FOREIGN KEY (source_deployment_id) REFERENCES public.deployments(id) ON DELETE RESTRICT;


--
-- Name: build_attempts build_attempts_delivery_claim_key_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_attempts
    ADD CONSTRAINT build_attempts_delivery_claim_key_fkey FOREIGN KEY (delivery_claim_key) REFERENCES public.github_webhook_receipts(claim_key) ON DELETE RESTRICT;


--
-- Name: build_attempts build_attempts_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_attempts
    ADD CONSTRAINT build_attempts_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: build_attempts build_attempts_service_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_attempts
    ADD CONSTRAINT build_attempts_service_id_project_id_fkey FOREIGN KEY (service_id, project_id) REFERENCES public.applications(id, project_id) ON DELETE RESTRICT;


--
-- Name: build_release_projections build_release_projections_attempt_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.build_release_projections
    ADD CONSTRAINT build_release_projections_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES public.build_attempts(id) ON DELETE RESTRICT;


--
-- Name: builder_platform_setting_mutations builder_platform_setting_mutations_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.builder_platform_setting_mutations
    ADD CONSTRAINT builder_platform_setting_mutations_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: builder_platform_setting_mutations builder_platform_setting_mutations_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.builder_platform_setting_mutations
    ADD CONSTRAINT builder_platform_setting_mutations_revision_fkey FOREIGN KEY (revision) REFERENCES public.builder_platform_settings_revisions(revision) ON DELETE RESTRICT;


--
-- Name: builder_platform_settings_revisions builder_platform_settings_revisions_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.builder_platform_settings_revisions
    ADD CONSTRAINT builder_platform_settings_revisions_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: cert_manager_issuer_observations cert_manager_issuer_observations_profile_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cert_manager_issuer_observations
    ADD CONSTRAINT cert_manager_issuer_observations_profile_revision_fkey FOREIGN KEY (profile_id, revision) REFERENCES public.configuration_profile_revisions(profile_id, revision) ON DELETE RESTRICT;


--
-- Name: cert_manager_issuer_references cert_manager_issuer_references_application_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cert_manager_issuer_references
    ADD CONSTRAINT cert_manager_issuer_references_application_id_fkey FOREIGN KEY (application_id) REFERENCES public.applications(id) ON DELETE RESTRICT;


--
-- Name: cert_manager_issuer_references cert_manager_issuer_references_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cert_manager_issuer_references
    ADD CONSTRAINT cert_manager_issuer_references_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: cert_manager_issuer_references cert_manager_issuer_references_profile_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cert_manager_issuer_references
    ADD CONSTRAINT cert_manager_issuer_references_profile_revision_fkey FOREIGN KEY (profile_id, revision) REFERENCES public.configuration_profile_revisions(profile_id, revision) ON DELETE RESTRICT;


--
-- Name: configuration_profile_assignments configuration_profile_assignm_profile_id_revision_profile__fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_assignments
    ADD CONSTRAINT configuration_profile_assignm_profile_id_revision_profile__fkey FOREIGN KEY (profile_id, revision, profile_kind) REFERENCES public.configuration_profile_revisions(profile_id, revision, profile_kind) ON DELETE RESTRICT;


--
-- Name: configuration_profile_revisions configuration_profile_revisio_cloned_from_profile_id_clone_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_revisions
    ADD CONSTRAINT configuration_profile_revisio_cloned_from_profile_id_clone_fkey FOREIGN KEY (cloned_from_profile_id, cloned_from_revision, profile_kind) REFERENCES public.configuration_profile_revisions(profile_id, revision, profile_kind) ON DELETE RESTRICT;


--
-- Name: configuration_profile_revisions configuration_profile_revisions_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_revisions
    ADD CONSTRAINT configuration_profile_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: configuration_profile_revisions configuration_profile_revisions_profile_id_profile_kind_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profile_revisions
    ADD CONSTRAINT configuration_profile_revisions_profile_id_profile_kind_fkey FOREIGN KEY (profile_id, profile_kind) REFERENCES public.configuration_profiles(id, kind) ON DELETE RESTRICT;


--
-- Name: configuration_profiles configuration_profiles_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profiles
    ADD CONSTRAINT configuration_profiles_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: configuration_profiles configuration_profiles_current_revision_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profiles
    ADD CONSTRAINT configuration_profiles_current_revision_fk FOREIGN KEY (id, current_revision, kind) REFERENCES public.configuration_profile_revisions(profile_id, revision, profile_kind) DEFERRABLE INITIALLY DEFERRED;


--
-- Name: configuration_profiles configuration_profiles_deactivated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.configuration_profiles
    ADD CONSTRAINT configuration_profiles_deactivated_by_fkey FOREIGN KEY (deactivated_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: deployment_operation_inputs deployment_operation_inputs_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_operation_inputs
    ADD CONSTRAINT deployment_operation_inputs_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id) ON DELETE CASCADE;


--
-- Name: deployment_operation_inputs deployment_operation_inputs_operation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_operation_inputs
    ADD CONSTRAINT deployment_operation_inputs_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE CASCADE;


--
-- Name: deployments deployments_application_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_application_id_fkey FOREIGN KEY (application_id) REFERENCES public.applications(id) ON DELETE RESTRICT;


--
-- Name: deployments deployments_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: deployments deployments_operation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) DEFERRABLE INITIALLY DEFERRED;


--
-- Name: edge_runtime_targets edge_runtime_targets_integration_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.edge_runtime_targets
    ADD CONSTRAINT edge_runtime_targets_integration_id_fkey FOREIGN KEY (integration_id) REFERENCES public.external_dns_integrations(id) ON DELETE RESTRICT;


--
-- Name: edge_sslip_ingress_observations edge_sslip_ingress_observation_target_key_profile_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.edge_sslip_ingress_observations
    ADD CONSTRAINT edge_sslip_ingress_observation_target_key_profile_revision_fkey FOREIGN KEY (target_key, profile_revision) REFERENCES public.edge_runtime_targets(target_key, profile_revision) ON DELETE RESTRICT;


--
-- Name: environment_app_placements environment_app_placements_application_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_app_placements
    ADD CONSTRAINT environment_app_placements_application_id_project_id_fkey FOREIGN KEY (application_id, project_id) REFERENCES public.applications(id, project_id) ON DELETE CASCADE;


--
-- Name: environment_app_placements environment_app_placements_environment_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_app_placements
    ADD CONSTRAINT environment_app_placements_environment_id_project_id_fkey FOREIGN KEY (environment_id, project_id) REFERENCES public.environments(id, project_id) ON DELETE CASCADE;


--
-- Name: environment_foundation_intents environment_foundation_intent_platform_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_foundation_intents
    ADD CONSTRAINT environment_foundation_intent_platform_binding_id_fkey FOREIGN KEY (platform_binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT;


--
-- Name: environment_foundation_intents environment_foundation_intent_platform_binding_id_target_r_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_foundation_intents
    ADD CONSTRAINT environment_foundation_intent_platform_binding_id_target_r_fkey FOREIGN KEY (platform_binding_id, target_ref) REFERENCES public.git_repository_bindings(id, target_ref) ON DELETE RESTRICT;


--
-- Name: environment_foundation_intents environment_foundation_intents_environment_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environment_foundation_intents
    ADD CONSTRAINT environment_foundation_intents_environment_id_project_id_fkey FOREIGN KEY (environment_id, project_id) REFERENCES public.environments(id, project_id) ON DELETE RESTRICT;


--
-- Name: environments environments_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.environments
    ADD CONSTRAINT environments_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: external_dns_integration_environments external_dns_integration_environments_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integration_environments
    ADD CONSTRAINT external_dns_integration_environments_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: external_dns_integration_environments external_dns_integration_environments_integration_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integration_environments
    ADD CONSTRAINT external_dns_integration_environments_integration_id_fkey FOREIGN KEY (integration_id) REFERENCES public.external_dns_integrations(id) ON DELETE CASCADE;


--
-- Name: external_dns_integrations external_dns_integrations_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integrations
    ADD CONSTRAINT external_dns_integrations_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: external_dns_integrations external_dns_integrations_deactivated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.external_dns_integrations
    ADD CONSTRAINT external_dns_integrations_deactivated_by_fkey FOREIGN KEY (deactivated_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: git_path_reservations git_path_reservations_binding_id_target_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_path_reservations
    ADD CONSTRAINT git_path_reservations_binding_id_target_ref_fkey FOREIGN KEY (binding_id, target_ref) REFERENCES public.git_repository_bindings(id, target_ref) ON DELETE RESTRICT;


--
-- Name: git_projected_documents git_projected_documents_binding_id_generation_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projected_documents
    ADD CONSTRAINT git_projected_documents_binding_id_generation_fkey FOREIGN KEY (binding_id, generation) REFERENCES public.git_projection_generations(binding_id, generation) ON DELETE CASCADE;


--
-- Name: git_projection_generations git_projection_generations_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_generations
    ADD CONSTRAINT git_projection_generations_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: git_projection_push_wake_targets git_projection_push_wake_targets_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_push_wake_targets
    ADD CONSTRAINT git_projection_push_wake_targets_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: git_projection_push_wake_targets git_projection_push_wake_targets_delivery_hash_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_projection_push_wake_targets
    ADD CONSTRAINT git_projection_push_wake_targets_delivery_hash_fkey FOREIGN KEY (delivery_hash) REFERENCES public.git_projection_push_wakes(delivery_hash) ON DELETE RESTRICT;


--
-- Name: git_pull_request_publications git_pull_request_publications_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_pull_request_publications
    ADD CONSTRAINT git_pull_request_publications_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: git_pull_request_publications git_pull_request_publications_operation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_pull_request_publications
    ADD CONSTRAINT git_pull_request_publications_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE CASCADE;


--
-- Name: git_repository_bindings git_repository_bindings_environment_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_environment_id_project_id_fkey FOREIGN KEY (environment_id, project_id) REFERENCES public.environments(id, project_id) ON DELETE RESTRICT;


--
-- Name: git_repository_bindings git_repository_bindings_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_repository_bindings
    ADD CONSTRAINT git_repository_bindings_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: git_safety_poll_cursors git_safety_poll_cursors_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_safety_poll_cursors
    ADD CONSTRAINT git_safety_poll_cursors_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: git_ssh_key_mutation_receipts git_ssh_key_mutation_receipts_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_ssh_key_mutation_receipts
    ADD CONSTRAINT git_ssh_key_mutation_receipts_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: git_ssh_key_mutation_receipts git_ssh_key_mutation_receipts_key_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_ssh_key_mutation_receipts
    ADD CONSTRAINT git_ssh_key_mutation_receipts_key_revision_fkey FOREIGN KEY (scope, owner_id, key_revision) REFERENCES public.git_ssh_key_revisions(scope, owner_id, revision) ON DELETE RESTRICT;


--
-- Name: git_verified_head_observations git_verified_head_observations_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_verified_head_observations
    ADD CONSTRAINT git_verified_head_observations_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: git_verified_head_observations git_verified_head_observations_binding_id_target_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_verified_head_observations
    ADD CONSTRAINT git_verified_head_observations_binding_id_target_ref_fkey FOREIGN KEY (binding_id, target_ref) REFERENCES public.git_repository_bindings(id, target_ref) ON DELETE CASCADE;


--
-- Name: git_write_commands git_write_commands_application_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_application_id_fkey FOREIGN KEY (application_id) REFERENCES public.applications(id) ON DELETE RESTRICT;


--
-- Name: git_write_commands git_write_commands_binding_id_project_id_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_binding_id_project_id_environment_id_fkey FOREIGN KEY (binding_id, project_id, environment_id) REFERENCES public.git_repository_bindings(id, project_id, environment_id) ON DELETE RESTRICT;


--
-- Name: git_write_commands git_write_commands_binding_id_target_ref_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_binding_id_target_ref_fkey FOREIGN KEY (binding_id, target_ref) REFERENCES public.git_repository_bindings(id, target_ref) ON DELETE RESTRICT;


--
-- Name: git_write_commands git_write_commands_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id) DEFERRABLE INITIALLY DEFERRED;


--
-- Name: git_write_commands git_write_commands_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: git_write_commands git_write_commands_operation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE CASCADE;


--
-- Name: git_write_commands git_write_commands_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_write_commands
    ADD CONSTRAINT git_write_commands_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: github_installations github_installations_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_installations
    ADD CONSTRAINT github_installations_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: github_installations github_installations_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_installations
    ADD CONSTRAINT github_installations_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE RESTRICT;


--
-- Name: github_repositories github_repositories_installation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_repositories
    ADD CONSTRAINT github_repositories_installation_id_fkey FOREIGN KEY (installation_id) REFERENCES public.github_installations(id) ON DELETE CASCADE;


--
-- Name: github_setup_authorizations github_setup_authorizations_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_setup_authorizations
    ADD CONSTRAINT github_setup_authorizations_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: github_setup_handoffs github_setup_handoffs_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_setup_handoffs
    ADD CONSTRAINT github_setup_handoffs_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: github_setup_handoffs github_setup_handoffs_linked_installation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_setup_handoffs
    ADD CONSTRAINT github_setup_handoffs_linked_installation_id_fkey FOREIGN KEY (linked_installation_id) REFERENCES public.github_installations(id) ON DELETE RESTRICT;


--
-- Name: github_setup_handoffs github_setup_handoffs_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_setup_handoffs
    ADD CONSTRAINT github_setup_handoffs_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE RESTRICT;


--
-- Name: github_user_bindings github_user_bindings_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_user_bindings
    ADD CONSTRAINT github_user_bindings_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: github_webhook_receipts github_webhook_receipts_claim_kind_claim_key_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.github_webhook_receipts
    ADD CONSTRAINT github_webhook_receipts_claim_kind_claim_key_fkey FOREIGN KEY (claim_kind, claim_key) REFERENCES public.github_one_time_claims(kind, claim_key) ON DELETE RESTRICT;


--
-- Name: middleware_profile_references middleware_profile_references_application_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.middleware_profile_references
    ADD CONSTRAINT middleware_profile_references_application_id_fkey FOREIGN KEY (application_id) REFERENCES public.applications(id) ON DELETE RESTRICT;


--
-- Name: middleware_profile_references middleware_profile_references_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.middleware_profile_references
    ADD CONSTRAINT middleware_profile_references_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: middleware_profile_references middleware_profile_references_profile_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.middleware_profile_references
    ADD CONSTRAINT middleware_profile_references_profile_revision_fkey FOREIGN KEY (profile_id, revision) REFERENCES public.configuration_profile_revisions(profile_id, revision) ON DELETE RESTRICT;


--
-- Name: mutation_receipts mutation_receipts_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mutation_receipts
    ADD CONSTRAINT mutation_receipts_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: mutation_receipts mutation_receipts_auto_deploy_policy_id_result_revision_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mutation_receipts
    ADD CONSTRAINT mutation_receipts_auto_deploy_policy_id_result_revision_fkey FOREIGN KEY (auto_deploy_policy_id, result_revision) REFERENCES public.auto_deploy_policy_revisions(policy_id, revision) ON DELETE RESTRICT;


--
-- Name: mutation_receipts mutation_receipts_profile_id_namespace_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mutation_receipts
    ADD CONSTRAINT mutation_receipts_profile_id_namespace_fkey FOREIGN KEY (profile_id, namespace) REFERENCES public.configuration_profiles(id, kind) ON DELETE RESTRICT;


--
-- Name: mutation_receipts mutation_receipts_profile_id_result_revision_namespace_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mutation_receipts
    ADD CONSTRAINT mutation_receipts_profile_id_result_revision_namespace_fkey FOREIGN KEY (profile_id, result_revision, namespace) REFERENCES public.configuration_profile_revisions(profile_id, revision, profile_kind) ON DELETE RESTRICT;


--
-- Name: outbox outbox_operation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.outbox
    ADD CONSTRAINT outbox_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE CASCADE;


--
-- Name: preview_authorities preview_authorities_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preview_authorities
    ADD CONSTRAINT preview_authorities_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE CASCADE;


--
-- Name: preview_authorities preview_authorities_binding_id_project_id_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preview_authorities
    ADD CONSTRAINT preview_authorities_binding_id_project_id_environment_id_fkey FOREIGN KEY (binding_id, project_id, environment_id) REFERENCES public.git_repository_bindings(id, project_id, environment_id) ON DELETE CASCADE;


--
-- Name: preview_authorities preview_authorities_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preview_authorities
    ADD CONSTRAINT preview_authorities_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id) ON DELETE CASCADE;


--
-- Name: preview_authorities preview_authorities_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preview_authorities
    ADD CONSTRAINT preview_authorities_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE CASCADE;


--
-- Name: preview_authorities preview_authorities_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.preview_authorities
    ADD CONSTRAINT preview_authorities_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: project_registry_pull_credentials project_registry_pull_credentials_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_registry_pull_credentials
    ADD CONSTRAINT project_registry_pull_credentials_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: project_registry_pull_credentials project_registry_pull_credentials_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_registry_pull_credentials
    ADD CONSTRAINT project_registry_pull_credentials_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: project_registry_pull_credentials project_registry_pull_credentials_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_registry_pull_credentials
    ADD CONSTRAINT project_registry_pull_credentials_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: projects projects_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE RESTRICT;


--
-- Name: registry_artifact_references registry_artifact_references_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_artifact_references
    ADD CONSTRAINT registry_artifact_references_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: registry_authority_observations registry_authority_observations_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_authority_observations
    ADD CONSTRAINT registry_authority_observations_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: registry_blobs registry_blobs_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_blobs
    ADD CONSTRAINT registry_blobs_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: registry_cache_generations registry_cache_generations_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cache_generations
    ADD CONSTRAINT registry_cache_generations_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: registry_catalog_observations registry_catalog_observations_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_catalog_observations
    ADD CONSTRAINT registry_catalog_observations_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: registry_cleanup_items registry_cleanup_items_plan_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_items
    ADD CONSTRAINT registry_cleanup_items_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES public.registry_cleanup_plans(id) ON DELETE CASCADE;


--
-- Name: registry_cleanup_leases registry_cleanup_leases_plan_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_leases
    ADD CONSTRAINT registry_cleanup_leases_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES public.registry_cleanup_plans(id) ON DELETE CASCADE;


--
-- Name: registry_cleanup_leases registry_cleanup_leases_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_leases
    ADD CONSTRAINT registry_cleanup_leases_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: registry_cleanup_plans registry_cleanup_plans_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_cleanup_plans
    ADD CONSTRAINT registry_cleanup_plans_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: registry_inventory_observations registry_inventory_observations_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_inventory_observations
    ADD CONSTRAINT registry_inventory_observations_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: registry_manifest_blobs registry_manifest_blobs_registry_target_id_repository_blob_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifest_blobs
    ADD CONSTRAINT registry_manifest_blobs_registry_target_id_repository_blob_fkey FOREIGN KEY (registry_target_id, repository, blob_digest) REFERENCES public.registry_blobs(registry_target_id, repository, digest) ON DELETE RESTRICT;


--
-- Name: registry_manifest_blobs registry_manifest_blobs_registry_target_id_repository_mani_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifest_blobs
    ADD CONSTRAINT registry_manifest_blobs_registry_target_id_repository_mani_fkey FOREIGN KEY (registry_target_id, repository, manifest_digest) REFERENCES public.registry_manifests(registry_target_id, repository, digest) ON DELETE CASCADE;


--
-- Name: registry_manifest_children registry_manifest_children_registry_target_id_repository_c_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifest_children
    ADD CONSTRAINT registry_manifest_children_registry_target_id_repository_c_fkey FOREIGN KEY (registry_target_id, repository, child_digest) REFERENCES public.registry_manifests(registry_target_id, repository, digest) ON DELETE RESTRICT;


--
-- Name: registry_manifest_children registry_manifest_children_registry_target_id_repository_p_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifest_children
    ADD CONSTRAINT registry_manifest_children_registry_target_id_repository_p_fkey FOREIGN KEY (registry_target_id, repository, parent_digest) REFERENCES public.registry_manifests(registry_target_id, repository, digest) ON DELETE CASCADE;


--
-- Name: registry_manifests registry_manifests_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_manifests
    ADD CONSTRAINT registry_manifests_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: registry_releases registry_releases_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_releases
    ADD CONSTRAINT registry_releases_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: registry_runtime_gc_sweep_receipts registry_runtime_gc_sweep_rec_registry_target_id_execution_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_runtime_gc_sweep_receipts
    ADD CONSTRAINT registry_runtime_gc_sweep_rec_registry_target_id_execution_fkey FOREIGN KEY (registry_target_id, execution_key) REFERENCES public.registry_runtime_maintenance_executions(registry_target_id, execution_key) ON DELETE RESTRICT;


--
-- Name: registry_runtime_maintenance_executions registry_runtime_maintenance_executions_plan_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_runtime_maintenance_executions
    ADD CONSTRAINT registry_runtime_maintenance_executions_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES public.registry_cleanup_plans(id) ON DELETE RESTRICT;


--
-- Name: registry_runtime_maintenance_executions registry_runtime_maintenance_executions_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_runtime_maintenance_executions
    ADD CONSTRAINT registry_runtime_maintenance_executions_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: registry_runtime_observation_cursors registry_runtime_observation_cursors_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.registry_runtime_observation_cursors
    ADD CONSTRAINT registry_runtime_observation_cursors_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE CASCADE;


--
-- Name: runtime_readiness runtime_readiness_platform_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runtime_readiness
    ADD CONSTRAINT runtime_readiness_platform_binding_id_fkey FOREIGN KEY (platform_binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT;


--
-- Name: runtime_readiness runtime_readiness_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runtime_readiness
    ADD CONSTRAINT runtime_readiness_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: runtime_registry_pull_artifacts runtime_registry_pull_artifacts_environment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runtime_registry_pull_artifacts
    ADD CONSTRAINT runtime_registry_pull_artifacts_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES public.environments(id) ON DELETE RESTRICT;


--
-- Name: runtime_registry_pull_artifacts runtime_registry_pull_artifacts_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.runtime_registry_pull_artifacts
    ADD CONSTRAINT runtime_registry_pull_artifacts_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: secret_binding_deliveries secret_binding_deliveries_version_id_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_deliveries
    ADD CONSTRAINT secret_binding_deliveries_version_id_binding_id_fkey FOREIGN KEY (version_id, binding_id) REFERENCES public.secret_binding_versions(id, binding_id) ON DELETE RESTRICT;


--
-- Name: secret_binding_events secret_binding_events_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_events
    ADD CONSTRAINT secret_binding_events_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: secret_binding_events secret_binding_events_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_events
    ADD CONSTRAINT secret_binding_events_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.secret_bindings(id) ON DELETE RESTRICT;


--
-- Name: secret_binding_events secret_binding_events_version_id_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_events
    ADD CONSTRAINT secret_binding_events_version_id_binding_id_fkey FOREIGN KEY (version_id, binding_id) REFERENCES public.secret_binding_versions(id, binding_id) ON DELETE RESTRICT;


--
-- Name: secret_binding_references secret_binding_references_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_references
    ADD CONSTRAINT secret_binding_references_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES public.secret_bindings(id) ON DELETE RESTRICT;


--
-- Name: secret_binding_references secret_binding_references_version_id_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_references
    ADD CONSTRAINT secret_binding_references_version_id_binding_id_fkey FOREIGN KEY (version_id, binding_id) REFERENCES public.secret_binding_versions(id, binding_id) ON DELETE RESTRICT;


--
-- Name: secret_binding_runtime_reconciliations secret_binding_runtime_reconciliatio_version_id_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_runtime_reconciliations
    ADD CONSTRAINT secret_binding_runtime_reconciliatio_version_id_binding_id_fkey FOREIGN KEY (version_id, binding_id) REFERENCES public.secret_binding_versions(id, binding_id) ON DELETE RESTRICT;


--
-- Name: secret_binding_versions secret_binding_versions_binding_id_provider_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_binding_versions
    ADD CONSTRAINT secret_binding_versions_binding_id_provider_fkey FOREIGN KEY (binding_id, provider) REFERENCES public.secret_bindings(id, provider) ON DELETE RESTRICT;


--
-- Name: secret_bindings secret_bindings_application_id_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_application_id_project_id_fkey FOREIGN KEY (application_id, project_id) REFERENCES public.applications(id, project_id) ON DELETE RESTRICT;


--
-- Name: secret_bindings secret_bindings_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: secret_bindings secret_bindings_environment_id_project_id_target_namespace_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_environment_id_project_id_target_namespace_fkey FOREIGN KEY (environment_id, project_id, target_namespace) REFERENCES public.environments(id, project_id, namespace) ON DELETE RESTRICT;


--
-- Name: secret_bindings secret_bindings_organization_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_organization_id_fkey FOREIGN KEY (organization_id) REFERENCES public.teams(id) ON DELETE RESTRICT;


--
-- Name: secret_bindings secret_bindings_project_id_organization_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_bindings
    ADD CONSTRAINT secret_bindings_project_id_organization_id_fkey FOREIGN KEY (project_id, organization_id) REFERENCES public.projects(id, team_id) ON DELETE RESTRICT;


--
-- Name: service_account_tokens service_account_tokens_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_account_tokens
    ADD CONSTRAINT service_account_tokens_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: service_account_tokens service_account_tokens_service_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_account_tokens
    ADD CONSTRAINT service_account_tokens_service_account_id_fkey FOREIGN KEY (service_account_id) REFERENCES public.service_accounts(id) ON DELETE CASCADE;


--
-- Name: service_accounts service_accounts_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_accounts
    ADD CONSTRAINT service_accounts_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: service_accounts service_accounts_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_accounts
    ADD CONSTRAINT service_accounts_id_fkey FOREIGN KEY (id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: service_accounts service_accounts_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_accounts
    ADD CONSTRAINT service_accounts_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE RESTRICT;


--
-- Name: service_registry_policies service_registry_policies_registry_target_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.service_registry_policies
    ADD CONSTRAINT service_registry_policies_registry_target_id_fkey FOREIGN KEY (registry_target_id) REFERENCES public.registry_targets(id) ON DELETE RESTRICT;


--
-- Name: sessions sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: team_memberships team_memberships_team_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_team_id_fkey FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE CASCADE;


--
-- Name: team_memberships team_memberships_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.team_memberships
    ADD CONSTRAINT team_memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: teams teams_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.teams
    ADD CONSTRAINT teams_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: tls_certificate_observations tls_certificate_observations_version_id_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_certificate_observations
    ADD CONSTRAINT tls_certificate_observations_version_id_binding_id_fkey FOREIGN KEY (version_id, binding_id) REFERENCES public.tls_certificate_versions(version_id, binding_id) ON DELETE RESTRICT;


--
-- Name: tls_certificate_versions tls_certificate_versions_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_certificate_versions
    ADD CONSTRAINT tls_certificate_versions_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: tls_certificate_versions tls_certificate_versions_version_id_binding_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tls_certificate_versions
    ADD CONSTRAINT tls_certificate_versions_version_id_binding_id_fkey FOREIGN KEY (version_id, binding_id) REFERENCES public.secret_binding_versions(id, binding_id) ON DELETE RESTRICT;


--
-- Name: user_invitations user_invitations_accepted_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_invitations
    ADD CONSTRAINT user_invitations_accepted_user_id_fkey FOREIGN KEY (accepted_user_id) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: user_invitations user_invitations_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_invitations
    ADD CONSTRAINT user_invitations_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: user_password_credentials user_password_credentials_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_password_credentials
    ADD CONSTRAINT user_password_credentials_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: helm_app_revisions and helm_app_heads; Type: TABLES; Schema: public; Owner: -
--
CREATE TABLE public.helm_app_revisions (
    id uuid NOT NULL PRIMARY KEY,
    generation bigint NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    release_name text NOT NULL,
    destination_namespace text NOT NULL,
    argo_project text NOT NULL,
    source_kind text NOT NULL,
    repository_url text NOT NULL,
    chart text NOT NULL,
    target_revision text NOT NULL,
    chart_path text NOT NULL,
    values_yaml bytea NOT NULL,
    values_digest text NOT NULL,
    action text NOT NULL,
    desired_enabled boolean NOT NULL,
    state text NOT NULL,
    failure_code text NOT NULL,
    actor_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    request_id text NOT NULL,
    parent_revision_id uuid,
    rollback_source_revision_id uuid,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT helm_app_revisions_source_check CHECK (
      (source_kind = 'git' AND chart = '' AND chart_path <> '') OR
      (source_kind IN ('helm-repository','oci') AND chart <> '' AND chart_path = '')
    ),
    CONSTRAINT helm_app_revisions_generation_check CHECK (generation > 0),
    CONSTRAINT helm_app_revisions_action_check CHECK (action IN ('deploy','retry','disable','rollback')),
    CONSTRAINT helm_app_revisions_enabled_check CHECK (desired_enabled = (action <> 'disable')),
    CONSTRAINT helm_app_revisions_parent_check CHECK ((generation = 1) = (parent_revision_id IS NULL)),
    CONSTRAINT helm_app_revisions_rollback_check CHECK ((action = 'rollback') = (rollback_source_revision_id IS NOT NULL)),
    CONSTRAINT helm_app_revisions_state_check CHECK (state IN ('pending','applied','failed')),
    CONSTRAINT helm_app_revisions_failure_check CHECK ((state = 'failed') = (failure_code <> '')),
    CONSTRAINT helm_app_revisions_values_digest_check CHECK (values_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT helm_app_revisions_time_check CHECK (updated_at >= created_at),
    CONSTRAINT helm_app_revisions_actor_key UNIQUE (actor_id,idempotency_key),
    CONSTRAINT helm_app_revisions_generation_key UNIQUE (environment_id,application_id,generation),
    CONSTRAINT helm_app_revisions_target_key UNIQUE (id,project_id,environment_id,application_id,generation),
    CONSTRAINT helm_app_revisions_application_fkey FOREIGN KEY (application_id,project_id)
      REFERENCES public.applications(id,project_id) ON DELETE CASCADE,
    CONSTRAINT helm_app_revisions_environment_fkey FOREIGN KEY (environment_id,project_id)
      REFERENCES public.environments(id,project_id) ON DELETE CASCADE,
    CONSTRAINT helm_app_revisions_actor_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE RESTRICT,
    CONSTRAINT helm_app_revisions_parent_fkey FOREIGN KEY (parent_revision_id) REFERENCES public.helm_app_revisions(id) ON DELETE RESTRICT,
    CONSTRAINT helm_app_revisions_rollback_fkey FOREIGN KEY (rollback_source_revision_id) REFERENCES public.helm_app_revisions(id) ON DELETE RESTRICT
);

CREATE INDEX helm_app_revisions_history_idx ON public.helm_app_revisions(environment_id,application_id,generation DESC);
CREATE INDEX helm_app_revisions_pending_idx ON public.helm_app_revisions(state,created_at,id) WHERE state='pending';

CREATE TABLE public.helm_app_heads (
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    revision_id uuid NOT NULL UNIQUE,
    generation bigint NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    PRIMARY KEY (environment_id,application_id),
    CONSTRAINT helm_app_heads_application_fkey FOREIGN KEY (application_id,project_id)
      REFERENCES public.applications(id,project_id) ON DELETE CASCADE,
    CONSTRAINT helm_app_heads_environment_fkey FOREIGN KEY (environment_id,project_id)
      REFERENCES public.environments(id,project_id) ON DELETE CASCADE,
    CONSTRAINT helm_app_heads_revision_fkey FOREIGN KEY (revision_id,project_id,environment_id,application_id,generation)
      REFERENCES public.helm_app_revisions(id,project_id,environment_id,application_id,generation) ON DELETE RESTRICT
);

CREATE TABLE public.environment_foundation_deletions (
    id uuid PRIMARY KEY,
    environment_id uuid NOT NULL UNIQUE,
    project_id uuid NOT NULL,
    namespace text NOT NULL,
    argo_project text NOT NULL,
    platform_binding_id uuid NOT NULL,
    target_ref text NOT NULL,
    required_ancestor text NOT NULL,
    manifest_path text NOT NULL,
    expected_manifest_digest text NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    last_failure_code text NOT NULL DEFAULT '',
    lease_owner text,
    lease_epoch bigint NOT NULL DEFAULT 0,
    lease_until timestamp with time zone,
    next_attempt_at timestamp with time zone NOT NULL,
    committed_revision text NOT NULL DEFAULT '',
    provider_request text NOT NULL DEFAULT '',
    completed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT environment_foundation_deletions_binding_fkey FOREIGN KEY (platform_binding_id)
      REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT,
    CONSTRAINT environment_foundation_deletions_state_check CHECK (state IN ('pending','claimed','ready','failed')),
    CONSTRAINT environment_foundation_deletions_attempts_check CHECK (attempts BETWEEN 0 AND 30),
    CONSTRAINT environment_foundation_deletions_path_check CHECK
      (manifest_path = 'platform/argocd/foundations/' || environment_id::text || '.yaml'),
    CONSTRAINT environment_foundation_deletions_digest_check CHECK
      (expected_manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT environment_foundation_deletions_ref_check CHECK
      (target_ref ~ '^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$' AND target_ref !~ '(\.\.|//)'),
    CONSTRAINT environment_foundation_deletions_ancestor_check CHECK
      (required_ancestor ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    CONSTRAINT environment_foundation_deletions_revision_check CHECK
      (committed_revision = '' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    CONSTRAINT environment_foundation_deletions_lease_check CHECK
      ((state = 'claimed' AND lease_owner IS NOT NULL AND lease_epoch > 0 AND lease_until > updated_at) OR
       (state <> 'claimed' AND lease_owner IS NULL AND lease_until IS NULL)),
    CONSTRAINT environment_foundation_deletions_completion_check CHECK
      ((state = 'ready' AND completed_at IS NOT NULL) OR (state <> 'ready' AND completed_at IS NULL)),
    CONSTRAINT environment_foundation_deletions_time_check CHECK
      (updated_at >= created_at AND next_attempt_at >= created_at)
);

CREATE INDEX environment_foundation_deletions_due_idx
  ON public.environment_foundation_deletions(next_attempt_at,id)
  WHERE state IN ('pending','claimed');

--
-- PostgreSQL database dump complete
--
