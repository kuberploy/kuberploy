-- Phase one necessarily advances the shared Git repository. Phase two keeps
-- that immutable payload identity while proving independently rendered current
-- AppProject, materialization, foundation, and Git authority.
ALTER TABLE public.argo_desired_state_commands
ADD COLUMN app_project_content bytea;

UPDATE public.argo_desired_state_commands command
SET app_project_content=pg_catalog.substr(command.content,1,
    position(pg_catalog.convert_to(E'---\n','UTF8') IN command.content)-1)
WHERE position(pg_catalog.convert_to(E'---\n','UTF8') IN command.content)>1
  AND position(pg_catalog.convert_to(E'---\n','UTF8') IN
      pg_catalog.substr(command.content,
        position(pg_catalog.convert_to(E'---\n','UTF8') IN command.content)+4))=0;

CREATE FUNCTION public.validate_argo_desired_state_app_project_content()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
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

CREATE TRIGGER argo_desired_state_app_project_content_validate
BEFORE INSERT OR UPDATE ON public.argo_desired_state_commands
FOR EACH ROW EXECUTE FUNCTION public.validate_argo_desired_state_app_project_content();

ALTER TABLE public.argo_desired_state_materialization_receipts
ADD COLUMN app_project_content bytea;

ALTER TABLE public.argo_desired_state_materialization_receipts
DISABLE TRIGGER argo_desired_state_materialization_receipts_validate;

UPDATE public.argo_desired_state_materialization_receipts receipt
SET app_project_content=command.app_project_content
FROM public.argo_desired_state_commands command
WHERE command.id=receipt.desired_state_command_id AND command.app_project_content IS NOT NULL;

ALTER TABLE public.argo_desired_state_materialization_receipts
ENABLE TRIGGER argo_desired_state_materialization_receipts_validate;

CREATE OR REPLACE FUNCTION public.validate_argo_desired_state_materialization_receipt()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
BEGIN
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
          AND platform.cluster_id=NEW.cluster_id AND platform.target_ref=NEW.platform_target_ref
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
          AND command.cluster_id=NEW.cluster_id
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

CREATE FUNCTION public.validate_argo_materialization_app_project_content()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
BEGIN
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

CREATE TRIGGER argo_materialization_app_project_content_validate
BEFORE INSERT OR UPDATE OR DELETE ON public.argo_desired_state_materialization_receipts
FOR EACH ROW EXECUTE FUNCTION public.validate_argo_materialization_app_project_content();

CREATE OR REPLACE FUNCTION public.record_verified_argo_desired_state_materialization()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
BEGIN
    IF NEW.state='verified' AND NEW.policy_digest IS NOT NULL AND
       NEW.app_project_content IS NOT NULL AND
       (TG_OP='INSERT' OR OLD.state<>'verified') THEN
        INSERT INTO public.argo_desired_state_materialization_receipts(
            id,environment_binding_id,environment_revision,environment_generation,
            project_id,environment_id,platform_binding_id,cluster_id,
            platform_target_ref,environment_target_ref,desired_state_command_id,
            desired_state_generation,desired_state_revision,desired_state_content_sha256,
            catalog_digest,policy_digest,chart_repository,chart_name,chart_version,chart_digest,
            renderer_image,chart_digest_enforcement,app_project_content,created_at
        )
        SELECT NEW.id,NEW.environment_binding_id,NEW.environment_revision,
               NEW.environment_generation,NEW.project_id,NEW.environment_id,
               NEW.platform_binding_id,NEW.cluster_id,NEW.platform_target_ref,
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

LOCK TABLE public.helm_protected_application_intents IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE public.helm_protected_application_intents
ADD COLUMN continuation_required boolean NOT NULL DEFAULT false,
ADD COLUMN continuation_receipt_id uuid,
ADD COLUMN continuation_contract text NOT NULL DEFAULT '';

ALTER TABLE public.helm_protected_application_intents
ALTER COLUMN continuation_required SET DEFAULT true,
ADD CONSTRAINT helm_protected_application_continuation_shape CHECK (
    (NOT continuation_required AND continuation_receipt_id IS NULL AND continuation_contract='') OR
    (continuation_required AND continuation_receipt_id=id AND
     continuation_contract='helm-application-continuation.v1')
);

ALTER TABLE public.helm_protected_application_intents
DROP CONSTRAINT helm_protected_application_intents_release_revision_id_key,
DROP CONSTRAINT helm_protected_application_intents_payload_intent_id_key,
DROP CONSTRAINT helm_protected_application_in_environment_id_application_id_key;

CREATE UNIQUE INDEX helm_protected_application_release_live_key
ON public.helm_protected_application_intents(release_revision_id)
WHERE state<>'superseded';
CREATE UNIQUE INDEX helm_protected_application_payload_live_key
ON public.helm_protected_application_intents(payload_intent_id)
WHERE state<>'superseded';
CREATE UNIQUE INDEX helm_protected_application_generation_live_key
ON public.helm_protected_application_intents(environment_id,application_id,release_generation)
WHERE state<>'superseded';

CREATE TABLE public.helm_application_continuation_receipts (
    application_intent_id uuid PRIMARY KEY,
    release_revision_id uuid NOT NULL,
    payload_intent_id uuid NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    source_environment_revision text NOT NULL,
    source_environment_generation bigint NOT NULL,
    source_foundation_intent_id uuid NOT NULL,
    source_foundation_revision text NOT NULL,
    source_desired_state_command_id uuid NOT NULL,
    source_desired_state_revision text NOT NULL,
    source_desired_state_content_digest text NOT NULL,
    current_environment_revision text NOT NULL,
    current_environment_generation bigint NOT NULL,
    current_foundation_intent_id uuid NOT NULL,
    current_foundation_revision text NOT NULL,
    current_materialization_receipt_id uuid NOT NULL,
    current_desired_state_command_id uuid NOT NULL,
    current_desired_state_revision text NOT NULL,
    current_desired_state_content_digest text NOT NULL,
    planned_base_revision text NOT NULL,
    current_policy_digest text NOT NULL,
    current_chart_repository text NOT NULL,
    current_chart_name text NOT NULL,
    current_chart_version text NOT NULL,
    current_chart_digest text NOT NULL,
    current_renderer_image text NOT NULL,
    current_chart_digest_enforcement text NOT NULL,
    current_app_project_content bytea NOT NULL,
    application_content_digest text NOT NULL,
    application_intent_digest text NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT helm_application_continuation_release_attempt_key
      UNIQUE(release_revision_id,application_intent_id),
    CONSTRAINT helm_application_continuation_source_revision_check
      CHECK (source_environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    CONSTRAINT helm_application_continuation_current_revision_check
      CHECK (current_environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    CONSTRAINT helm_application_continuation_foundation_revisions_check
      CHECK (source_foundation_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$' AND
             current_foundation_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    CONSTRAINT helm_application_continuation_desired_revisions_check
      CHECK (source_desired_state_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$' AND
             current_desired_state_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    CONSTRAINT helm_application_continuation_base_check
      CHECK (planned_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    CONSTRAINT helm_application_continuation_digest_check
      CHECK (source_desired_state_content_digest ~ '^sha256:[0-9a-f]{64}$' AND
             current_desired_state_content_digest ~ '^sha256:[0-9a-f]{64}$' AND
             current_policy_digest ~ '^sha256:[0-9a-f]{64}$' AND
             current_chart_digest ~ '^sha256:[0-9a-f]{64}$' AND
             (application_content_digest='' OR application_content_digest ~ '^sha256:[0-9a-f]{64}$') AND
             application_intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT helm_application_continuation_generation_check
      CHECK (source_environment_generation>0 AND current_environment_generation>0),
    CONSTRAINT helm_application_continuation_runtime_check
      CHECK (current_chart_repository<>'' AND current_chart_name<>'' AND
             current_chart_version<>'' AND current_renderer_image<>'' AND
             current_chart_digest_enforcement='native-oci-digest-v1' AND
             octet_length(current_app_project_content)>0)
);

CREATE INDEX helm_application_continuation_payload_idx
ON public.helm_application_continuation_receipts(payload_intent_id,created_at DESC);

ALTER TABLE public.helm_application_continuation_receipts
ADD CONSTRAINT helm_application_continuation_application_fkey
  FOREIGN KEY(application_intent_id) REFERENCES public.helm_protected_application_intents(id)
  ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
ADD CONSTRAINT helm_application_continuation_payload_fkey
  FOREIGN KEY(payload_intent_id) REFERENCES public.helm_protected_payload_intents(id) ON DELETE RESTRICT,
ADD CONSTRAINT helm_application_continuation_materialization_fkey
  FOREIGN KEY(current_materialization_receipt_id)
  REFERENCES public.argo_desired_state_materialization_receipts(id) ON DELETE RESTRICT;

CREATE FUNCTION public.validate_helm_application_continuation_receipt()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
BEGIN
    IF TG_OP='DELETE' OR TG_OP='UPDATE' THEN
        RAISE EXCEPTION 'Helm Application continuation receipts are immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.created_at>pg_catalog.clock_timestamp()+INTERVAL '30 seconds' OR
       NEW.created_at<pg_catalog.clock_timestamp()-INTERVAL '5 minutes' OR NOT EXISTS (
        SELECT 1
        FROM public.helm_release_revisions release
        JOIN public.helm_release_heads head
          ON head.revision_id=release.id AND head.generation=release.generation
        JOIN public.helm_protected_payload_intents payload
          ON payload.id=NEW.payload_intent_id AND payload.release_revision_id=release.id
         AND payload.state='verified' AND payload.release_generation=release.generation
         AND payload.project_id=release.project_id AND payload.environment_id=release.environment_id
         AND payload.application_id=release.application_id
         AND payload.platform_binding_id=NEW.platform_binding_id
         AND payload.environment_binding_id=NEW.environment_binding_id
         AND payload.cluster_id=NEW.cluster_id
         AND payload.environment_revision=NEW.source_environment_revision
         AND payload.environment_generation=NEW.source_environment_generation
        JOIN public.helm_publication_prerequisite_receipts prerequisite
          ON prerequisite.release_revision_id=release.id
         AND prerequisite.project_id=release.project_id
         AND prerequisite.environment_id=release.environment_id
         AND prerequisite.application_id=release.application_id
         AND prerequisite.platform_binding_id=NEW.platform_binding_id
         AND prerequisite.environment_binding_id=NEW.environment_binding_id
         AND prerequisite.cluster_id=NEW.cluster_id
         AND prerequisite.environment_revision=NEW.source_environment_revision
         AND prerequisite.environment_generation=NEW.source_environment_generation
         AND prerequisite.foundation_intent_id=NEW.source_foundation_intent_id
         AND prerequisite.foundation_revision=NEW.source_foundation_revision
         AND prerequisite.desired_state_command_id=NEW.source_desired_state_command_id
         AND prerequisite.desired_state_revision=NEW.source_desired_state_revision
        JOIN public.argo_desired_state_commands source_command
          ON source_command.id=NEW.source_desired_state_command_id
         AND source_command.state='verified'
         AND source_command.committed_revision=NEW.source_desired_state_revision
         AND source_command.content_sha256=NEW.source_desired_state_content_digest
        JOIN public.git_repository_bindings platform
          ON platform.id=NEW.platform_binding_id AND platform.kind='platform'
         AND platform.credential_mode='github-app' AND platform.cluster_id=NEW.cluster_id
         AND platform.target_ref=payload.platform_target_ref
         AND platform.state IN ('ready','indexing')
         AND platform.target_head_revision=NEW.planned_base_revision
        JOIN public.git_repository_bindings environment
          ON environment.id=NEW.environment_binding_id AND environment.kind='environment'
         AND environment.credential_mode='github-app' AND environment.state='ready'
         AND environment.project_id=release.project_id AND environment.environment_id=release.environment_id
         AND environment.target_ref=payload.environment_target_ref
         AND environment.target_head_revision=NEW.current_environment_revision
         AND environment.indexed_revision=NEW.current_environment_revision
         AND environment.projection_generation=NEW.current_environment_generation
        JOIN public.git_projection_generations generation
          ON generation.binding_id=environment.id
         AND generation.generation=NEW.current_environment_generation
         AND generation.head_revision=NEW.current_environment_revision AND generation.state='active'
        JOIN public.environments desired_environment
          ON desired_environment.id=release.environment_id AND desired_environment.project_id=release.project_id
        JOIN public.environment_foundation_intents foundation
          ON foundation.id=NEW.current_foundation_intent_id
         AND foundation.environment_id=release.environment_id AND foundation.project_id=release.project_id
         AND foundation.active AND foundation.state='ready'
         AND foundation.platform_binding_id=NEW.platform_binding_id
         AND foundation.cluster_id=NEW.cluster_id AND foundation.target_ref=payload.platform_target_ref
         AND foundation.namespace=desired_environment.namespace
         AND foundation.argo_project=desired_environment.argo_project
         AND foundation.committed_revision=NEW.current_foundation_revision
         AND foundation.published_at IS NOT NULL AND foundation.completed_at=foundation.published_at
        JOIN public.argo_desired_state_materialization_receipts materialization
          ON materialization.id=NEW.current_materialization_receipt_id
         AND materialization.environment_binding_id=NEW.environment_binding_id
         AND materialization.environment_revision=NEW.current_environment_revision
         AND materialization.environment_generation=NEW.current_environment_generation
         AND materialization.desired_state_command_id=NEW.current_desired_state_command_id
         AND materialization.desired_state_revision=NEW.current_desired_state_revision
         AND materialization.desired_state_content_sha256=NEW.current_desired_state_content_digest
         AND materialization.policy_digest=NEW.current_policy_digest
         AND materialization.chart_repository=NEW.current_chart_repository
         AND materialization.chart_name=NEW.current_chart_name
         AND materialization.chart_version=NEW.current_chart_version
         AND materialization.chart_digest=NEW.current_chart_digest
         AND materialization.renderer_image=NEW.current_renderer_image
         AND materialization.chart_digest_enforcement=NEW.current_chart_digest_enforcement
         AND materialization.app_project_content=NEW.current_app_project_content
        JOIN public.argo_desired_state_commands current_command
          ON current_command.id=NEW.current_desired_state_command_id
         AND current_command.project_id=release.project_id
         AND current_command.environment_id=release.environment_id
         AND current_command.platform_binding_id=NEW.platform_binding_id
         AND current_command.environment_binding_id=NEW.environment_binding_id
         AND current_command.cluster_id=NEW.cluster_id
         AND current_command.platform_target_ref=payload.platform_target_ref
         AND current_command.environment_target_ref=payload.environment_target_ref
         AND current_command.environment_revision=NEW.current_environment_revision
         AND current_command.environment_generation=NEW.current_environment_generation
         AND current_command.state='verified'
         AND current_command.committed_revision=NEW.current_desired_state_revision
         AND current_command.content_sha256=NEW.current_desired_state_content_digest
         AND current_command.policy_digest=NEW.current_policy_digest
         AND current_command.chart_repository=NEW.current_chart_repository
         AND current_command.chart_name=NEW.current_chart_name
         AND current_command.chart_version=NEW.current_chart_version
         AND current_command.chart_digest=NEW.current_chart_digest
         AND current_command.renderer_image=NEW.current_renderer_image
         AND current_command.chart_digest_enforcement=NEW.current_chart_digest_enforcement
         AND current_command.app_project_content=NEW.current_app_project_content
         AND current_command.argo_project=desired_environment.argo_project
         AND current_command.destination_namespace=desired_environment.namespace
        WHERE release.id=NEW.release_revision_id AND release.project_id=NEW.project_id
          AND release.environment_id=NEW.environment_id AND release.application_id=NEW.application_id
          AND NOT EXISTS(SELECT 1 FROM public.git_projected_documents invalid
            WHERE invalid.binding_id=NEW.environment_binding_id
              AND invalid.generation=NEW.current_environment_generation AND NOT invalid.valid)
          AND NOT EXISTS(
            SELECT 1
            FROM public.argo_desired_state_materialization_receipts newer_materialization
            JOIN public.argo_desired_state_commands newer_command
              ON newer_command.id=newer_materialization.desired_state_command_id
             AND newer_command.state='verified'
             AND newer_command.committed_revision=newer_materialization.desired_state_revision
             AND newer_command.content_sha256=newer_materialization.desired_state_content_sha256
            WHERE newer_materialization.environment_binding_id=NEW.environment_binding_id
              AND newer_materialization.environment_revision=NEW.current_environment_revision
              AND newer_materialization.environment_generation=NEW.current_environment_generation
              AND (newer_materialization.desired_state_generation,
                   newer_materialization.created_at,newer_materialization.id)>
                  (materialization.desired_state_generation,
                   materialization.created_at,materialization.id)
          )
    ) THEN
        RAISE EXCEPTION 'Helm Application continuation receipt lacks exact current authority'
            USING ERRCODE='23514';
    END IF;
    IF EXISTS(SELECT 1 FROM public.helm_protected_application_intents live
        WHERE live.payload_intent_id=NEW.payload_intent_id AND live.state<>'superseded') OR
       (EXISTS(SELECT 1 FROM public.helm_protected_application_intents prior
          WHERE prior.payload_intent_id=NEW.payload_intent_id) AND NOT EXISTS(
        SELECT 1 FROM public.helm_protected_application_intents prior
        WHERE prior.payload_intent_id=NEW.payload_intent_id
          AND prior.state='superseded' AND prior.last_failure_code='projection-superseded'
          AND prior.lease_epoch=0 AND prior.attempts=0 AND prior.lease_owner IS NULL
          AND prior.lease_until IS NULL AND prior.write_base_revision=''
          AND prior.write_base_observed_at IS NULL AND prior.committed_revision=''
          AND prior.committed_parent_revision='' AND prior.committed_at IS NULL
          AND prior.verified_at IS NULL AND prior.verified_path_digest=''
          AND prior.provider_request='' AND prior.completed_at IS NOT NULL
          AND NOT EXISTS(SELECT 1 FROM public.helm_protected_application_intents newer
            WHERE newer.payload_intent_id=prior.payload_intent_id AND
              (newer.created_at,newer.id)>(prior.created_at,prior.id))
       )) THEN
        RAISE EXCEPTION 'Helm Application continuation replacement predecessor is not pristine'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_application_continuation_receipts_validate
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_application_continuation_receipts
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_continuation_receipt();

CREATE FUNCTION public.validate_helm_application_continuation_reference()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NOT NEW.continuation_required OR NEW.continuation_receipt_id<>NEW.id OR
           NEW.continuation_contract<>'helm-application-continuation.v1' OR NOT EXISTS(
            SELECT 1 FROM public.helm_application_continuation_receipts receipt
            WHERE receipt.application_intent_id=NEW.id
              AND receipt.release_revision_id=NEW.release_revision_id
              AND receipt.payload_intent_id=NEW.payload_intent_id
              AND receipt.project_id=NEW.project_id AND receipt.environment_id=NEW.environment_id
              AND receipt.application_id=NEW.application_id
              AND receipt.platform_binding_id=NEW.platform_binding_id
              AND receipt.environment_binding_id=NEW.environment_binding_id
              AND receipt.cluster_id=NEW.cluster_id
              AND receipt.source_environment_revision=NEW.environment_revision
              AND receipt.source_environment_generation=NEW.environment_generation
              AND receipt.planned_base_revision=NEW.planned_base_revision
              AND receipt.application_content_digest=NEW.content_digest
              AND receipt.application_intent_digest=NEW.intent_digest
        ) THEN
            RAISE EXCEPTION 'Helm protected Application lacks exact continuation authority'
                USING ERRCODE='23514';
        END IF;
    ELSIF ROW(NEW.continuation_required,NEW.continuation_receipt_id,NEW.continuation_contract)
        IS DISTINCT FROM ROW(OLD.continuation_required,OLD.continuation_receipt_id,OLD.continuation_contract) THEN
        RAISE EXCEPTION 'Helm protected Application continuation identity is immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_application_continuation_validate
BEFORE INSERT OR UPDATE ON public.helm_protected_application_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_continuation_reference();

CREATE FUNCTION public.helm_application_continuation_is_exact(candidate_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $$
    SELECT EXISTS(
        SELECT 1
        FROM public.helm_protected_application_intents intent
        JOIN public.helm_application_continuation_receipts receipt
          ON receipt.application_intent_id=intent.id
         AND receipt.release_revision_id=intent.release_revision_id
         AND receipt.payload_intent_id=intent.payload_intent_id
         AND receipt.application_content_digest=intent.content_digest
         AND receipt.application_intent_digest=intent.intent_digest
         AND receipt.planned_base_revision=intent.planned_base_revision
        JOIN public.helm_release_heads head
          ON head.revision_id=intent.release_revision_id AND head.generation=intent.release_generation
        JOIN public.helm_protected_payload_intents payload
          ON payload.id=intent.payload_intent_id AND payload.state='verified'
         AND payload.release_revision_id=intent.release_revision_id
         AND payload.committed_revision=intent.payload_revision AND payload.path=intent.payload_path
        JOIN public.argo_desired_state_commands source_command
          ON source_command.id=receipt.source_desired_state_command_id
         AND source_command.state='verified'
         AND source_command.committed_revision=receipt.source_desired_state_revision
         AND source_command.content_sha256=receipt.source_desired_state_content_digest
        JOIN public.argo_desired_state_materialization_receipts materialization
          ON materialization.id=receipt.current_materialization_receipt_id
         AND materialization.environment_binding_id=receipt.environment_binding_id
         AND materialization.environment_revision=receipt.current_environment_revision
         AND materialization.environment_generation=receipt.current_environment_generation
         AND materialization.desired_state_command_id=receipt.current_desired_state_command_id
         AND materialization.desired_state_revision=receipt.current_desired_state_revision
         AND materialization.desired_state_content_sha256=receipt.current_desired_state_content_digest
         AND materialization.policy_digest=receipt.current_policy_digest
         AND materialization.chart_repository=receipt.current_chart_repository
         AND materialization.chart_name=receipt.current_chart_name
         AND materialization.chart_version=receipt.current_chart_version
         AND materialization.chart_digest=receipt.current_chart_digest
         AND materialization.renderer_image=receipt.current_renderer_image
         AND materialization.chart_digest_enforcement=receipt.current_chart_digest_enforcement
         AND materialization.app_project_content=receipt.current_app_project_content
        JOIN public.git_repository_bindings current_environment_binding
          ON current_environment_binding.id=receipt.environment_binding_id
         AND current_environment_binding.kind='environment'
         AND current_environment_binding.credential_mode='github-app'
         AND current_environment_binding.state='ready'
         AND current_environment_binding.project_id=intent.project_id
         AND current_environment_binding.environment_id=intent.environment_id
         AND current_environment_binding.target_ref=intent.environment_target_ref
         AND current_environment_binding.target_head_revision=receipt.current_environment_revision
         AND current_environment_binding.indexed_revision=receipt.current_environment_revision
         AND current_environment_binding.projection_generation=receipt.current_environment_generation
        JOIN public.git_projection_generations current_generation
          ON current_generation.binding_id=current_environment_binding.id
         AND current_generation.generation=receipt.current_environment_generation
         AND current_generation.head_revision=receipt.current_environment_revision
         AND current_generation.state='active'
        JOIN public.environments desired_environment
          ON desired_environment.id=intent.environment_id AND desired_environment.project_id=intent.project_id
        JOIN public.argo_desired_state_commands current_command
          ON current_command.id=receipt.current_desired_state_command_id
         AND current_command.project_id=intent.project_id
         AND current_command.environment_id=intent.environment_id
         AND current_command.platform_binding_id=intent.platform_binding_id
         AND current_command.environment_binding_id=intent.environment_binding_id
         AND current_command.cluster_id=intent.cluster_id
         AND current_command.platform_target_ref=intent.platform_target_ref
         AND current_command.environment_target_ref=intent.environment_target_ref
         AND current_command.environment_revision=receipt.current_environment_revision
         AND current_command.environment_generation=receipt.current_environment_generation
         AND current_command.state='verified'
         AND current_command.committed_revision=receipt.current_desired_state_revision
         AND current_command.content_sha256=receipt.current_desired_state_content_digest
         AND current_command.policy_digest=receipt.current_policy_digest
         AND current_command.chart_repository=receipt.current_chart_repository
         AND current_command.chart_name=receipt.current_chart_name
         AND current_command.chart_version=receipt.current_chart_version
         AND current_command.chart_digest=receipt.current_chart_digest
         AND current_command.renderer_image=receipt.current_renderer_image
         AND current_command.chart_digest_enforcement=receipt.current_chart_digest_enforcement
         AND current_command.app_project_content=receipt.current_app_project_content
         AND current_command.argo_project=desired_environment.argo_project
         AND current_command.destination_namespace=desired_environment.namespace
        JOIN public.environment_foundation_intents foundation
          ON foundation.id=receipt.current_foundation_intent_id
         AND foundation.environment_id=intent.environment_id AND foundation.project_id=intent.project_id
         AND foundation.platform_binding_id=intent.platform_binding_id
         AND foundation.cluster_id=intent.cluster_id AND foundation.target_ref=intent.platform_target_ref
         AND foundation.namespace=desired_environment.namespace
         AND foundation.argo_project=desired_environment.argo_project
         AND foundation.committed_revision=receipt.current_foundation_revision
         AND foundation.state IN ('ready','superseded')
         AND foundation.published_at IS NOT NULL AND foundation.completed_at=foundation.published_at
        WHERE intent.id=candidate_id AND intent.continuation_required
          AND intent.continuation_receipt_id=receipt.application_intent_id
          AND intent.continuation_contract='helm-application-continuation.v1'
          AND NOT EXISTS(SELECT 1 FROM public.git_projected_documents invalid
            WHERE invalid.binding_id=receipt.environment_binding_id
              AND invalid.generation=receipt.current_environment_generation
              AND NOT invalid.valid)
          AND NOT EXISTS(
            SELECT 1
            FROM public.argo_desired_state_materialization_receipts newer_materialization
            JOIN public.argo_desired_state_commands newer_command
              ON newer_command.id=newer_materialization.desired_state_command_id
             AND newer_command.state='verified'
             AND newer_command.committed_revision=newer_materialization.desired_state_revision
             AND newer_command.content_sha256=newer_materialization.desired_state_content_sha256
            WHERE newer_materialization.environment_binding_id=receipt.environment_binding_id
              AND newer_materialization.environment_revision=receipt.current_environment_revision
              AND newer_materialization.environment_generation=receipt.current_environment_generation
              AND (newer_materialization.desired_state_generation,
                   newer_materialization.created_at,newer_materialization.id)>
                  (materialization.desired_state_generation,
                   materialization.created_at,materialization.id)
          )
    )
$$;

REVOKE ALL ON FUNCTION public.helm_application_continuation_is_exact(uuid) FROM PUBLIC;
REVOKE INSERT,UPDATE,DELETE ON public.helm_application_continuation_receipts FROM PUBLIC;

-- Extend the existing hardened Application identity validator without
-- duplicating or weakening any baseline transition rule.
DO $migration$
DECLARE
    definition text;
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_protected_application_intent()'::pg_catalog.regprocedure
    );
    IF pg_catalog.strpos(definition,
       'NEW.publisher_contract,NEW.original_publisher_config_digest,NEW.message')=0 OR
       pg_catalog.strpos(definition,
       'OLD.publisher_contract,OLD.original_publisher_config_digest,OLD.message')=0 THEN
        RAISE EXCEPTION 'unexpected protected Application continuation identity prerequisite';
    END IF;
    definition := pg_catalog.replace(definition,
       'NEW.publisher_contract,NEW.original_publisher_config_digest,NEW.message',
       'NEW.publisher_contract,NEW.original_publisher_config_digest,NEW.continuation_required,NEW.continuation_receipt_id,NEW.continuation_contract,NEW.message');
    definition := pg_catalog.replace(definition,
       'OLD.publisher_contract,OLD.original_publisher_config_digest,OLD.message',
       'OLD.publisher_contract,OLD.original_publisher_config_digest,OLD.continuation_required,OLD.continuation_receipt_id,OLD.continuation_contract,OLD.message');
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.validate_helm_protected_application_intent()
SET search_path=pg_catalog,pg_temp;

DROP TRIGGER helm_protected_application_publisher_authority
ON public.helm_protected_application_intents;

CREATE FUNCTION public.validate_helm_protected_application_publisher_authority_v009()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    exact_receipt boolean;
BEGIN
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(NEW.platform_binding_id::text,704215997)
    );
    IF TG_OP='INSERT' THEN
        IF NEW.publisher_config_digest<>NEW.original_publisher_config_digest OR
           NEW.publisher_adoption_epoch<>0 THEN
            RAISE EXCEPTION 'Helm protected Application publisher authority must start at its immutable origin'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.original_publisher_config_digest IS DISTINCT FROM OLD.original_publisher_config_digest THEN
        RAISE EXCEPTION 'Helm protected Application original publisher authority is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND NEW.lease_until>NEW.updated_at THEN
        IF OLD.lease_epoch=0 AND NEW.lease_epoch=1 AND NOT (
            (NEW.continuation_required AND public.helm_application_continuation_is_exact(NEW.id)) OR
            (NOT NEW.continuation_required AND public.helm_protected_adoption_projection_is_fresh(
                NEW.platform_binding_id,NEW.environment_binding_id,NEW.cluster_id,
                NEW.project_id,NEW.environment_id,NEW.platform_target_ref,
                NEW.environment_target_ref,NEW.environment_revision,NEW.environment_generation
            ))
        ) THEN
            RAISE EXCEPTION 'Helm protected Application authority claim lacks exact continuation'
                USING ERRCODE='23514';
        END IF;
        IF EXISTS(
            SELECT 1 FROM public.helm_protected_payload_intents held
            WHERE held.platform_binding_id=NEW.platform_binding_id
              AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.updated_at
        ) OR EXISTS(
            SELECT 1 FROM public.helm_protected_application_intents held
            WHERE held.platform_binding_id=NEW.platform_binding_id AND held.id<>NEW.id
              AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.updated_at
        ) THEN
            RAISE EXCEPTION 'Helm protected Application authority lane is already leased'
                USING ERRCODE='23514';
        END IF;
    END IF;
    IF NEW.publisher_config_digest=OLD.publisher_config_digest AND
       NEW.publisher_adoption_epoch=OLD.publisher_adoption_epoch THEN
        RETURN NEW;
    END IF;
    IF NEW.publisher_config_digest=OLD.publisher_config_digest OR
       NEW.publisher_adoption_epoch<>OLD.publisher_adoption_epoch+1 OR
       OLD.state NOT IN ('pending','claimed','git-committed') OR
       NEW.state<>(CASE WHEN OLD.state='pending' THEN 'claimed' ELSE OLD.state END) OR
       NEW.lease_owner IS NULL OR NEW.lease_epoch<>OLD.lease_epoch+1 OR
       NEW.lease_until IS NULL OR NEW.lease_until<=NEW.updated_at OR
       NEW.lease_until>NEW.updated_at+interval '5 minutes' OR
       NEW.attempts<>LEAST(OLD.attempts+1,30) OR
       NEW.next_attempt_at IS DISTINCT FROM OLD.next_attempt_at OR
       NEW.consecutive_failures<>OLD.consecutive_failures OR
       NEW.last_failure_code<>OLD.last_failure_code OR
       NEW.write_base_revision<>OLD.write_base_revision OR
       NEW.write_base_observed_at IS DISTINCT FROM OLD.write_base_observed_at OR
       NEW.committed_revision<>OLD.committed_revision OR
       NEW.committed_parent_revision<>OLD.committed_parent_revision OR
       NEW.committed_at IS DISTINCT FROM OLD.committed_at OR
       NEW.verified_at IS DISTINCT FROM OLD.verified_at OR
       NEW.verified_path_digest<>OLD.verified_path_digest OR
       NEW.provider_request<>OLD.provider_request OR
       NEW.completed_at IS DISTINCT FROM OLD.completed_at OR
       NEW.prerequisite_epoch<>OLD.prerequisite_epoch+1 THEN
        RAISE EXCEPTION 'Helm protected Application publisher adoption is not an exact atomic claim'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.runtime_readiness readiness
        WHERE readiness.runtime_kind='helm-protected-publisher' AND readiness.scope_key='global'
          AND readiness.worker_id=NEW.lease_owner
          AND readiness.contract_version=NEW.publisher_contract
          AND readiness.identity=pg_catalog.jsonb_build_object('policyVersion','helm-protected-git.v1')
          AND readiness.config_digest=NEW.publisher_config_digest
          AND readiness.updated_at=readiness.observed_at
          AND readiness.observed_at<=NEW.updated_at
          AND readiness.observed_at>=NEW.updated_at-interval '5 minutes'
          AND readiness.lease_until>NEW.updated_at
          AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
          AND readiness.lease_until<=NEW.updated_at+interval '5 minutes'
    ) THEN
        RAISE EXCEPTION 'Helm protected Application adoption lacks active exact readiness'
            USING ERRCODE='23514';
    END IF;
    SELECT EXISTS(
        SELECT 1 FROM public.helm_protected_publisher_adoption_receipts receipt
        WHERE receipt.intent_kind='application' AND receipt.application_intent_id=NEW.id
          AND receipt.adoption_epoch=NEW.publisher_adoption_epoch
          AND receipt.publisher_contract=NEW.publisher_contract
          AND receipt.original_config_digest=NEW.original_publisher_config_digest
          AND receipt.previous_config_digest=OLD.publisher_config_digest
          AND receipt.adopted_config_digest=NEW.publisher_config_digest
          AND receipt.previous_lease_epoch=OLD.lease_epoch
          AND receipt.adopted_lease_epoch=NEW.lease_epoch
          AND receipt.adopted_by_worker=NEW.lease_owner
          AND receipt.created_at=NEW.updated_at
    ) INTO exact_receipt;
    IF NOT exact_receipt THEN
        RAISE EXCEPTION 'Helm protected Application adoption lacks exact immutable receipt'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_application_publisher_authority
BEFORE INSERT OR UPDATE ON public.helm_protected_application_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_application_publisher_authority_v009();

CREATE OR REPLACE FUNCTION public.validate_helm_protected_publisher_adoption_receipt()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    binding_id uuid;
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'Helm protected publisher adoption receipts are immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.created_at>db_now OR NEW.created_at<db_now-interval '30 seconds' THEN
        RAISE EXCEPTION 'Helm publisher adoption receipt is outside bounded database time' USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.runtime_readiness readiness
        WHERE readiness.runtime_kind='helm-protected-publisher' AND readiness.scope_key='global'
          AND readiness.worker_id=NEW.adopted_by_worker AND readiness.worker_epoch=NEW.adopted_worker_epoch
          AND readiness.contract_version=NEW.publisher_contract
          AND readiness.identity=pg_catalog.jsonb_build_object('policyVersion',NEW.policy_version)
          AND readiness.config_digest=NEW.adopted_config_digest
          AND readiness.updated_at=readiness.observed_at AND readiness.observed_at<=NEW.created_at
          AND readiness.observed_at>=NEW.created_at-interval '5 minutes'
          AND readiness.lease_until>NEW.created_at
          AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
          AND readiness.lease_until<=NEW.created_at+interval '5 minutes'
    ) THEN
        RAISE EXCEPTION 'Helm publisher adoption lacks exact fresh worker readiness' USING ERRCODE='23514';
    END IF;
    IF NEW.intent_kind='payload' THEN
        SELECT intent.platform_binding_id INTO binding_id
        FROM public.helm_protected_payload_intents intent WHERE intent.id=NEW.payload_intent_id;
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(binding_id::text,704215997));
        IF NOT EXISTS (
            SELECT 1 FROM public.helm_protected_payload_intents intent
            WHERE intent.id=NEW.payload_intent_id
              AND intent.publisher_contract=NEW.publisher_contract
              AND intent.original_publisher_config_digest=NEW.original_config_digest
              AND intent.publisher_config_digest=NEW.previous_config_digest
              AND intent.publisher_adoption_epoch+1=NEW.adoption_epoch
              AND intent.intent_digest=NEW.intent_digest AND intent.content_digest=NEW.content_digest
              AND intent.path=NEW.protected_path AND intent.precondition=NEW.precondition
              AND intent.expected_etag=NEW.expected_etag AND intent.commit_trailer=NEW.commit_trailer
              AND intent.prerequisite_receipt_id=NEW.prerequisite_receipt_id
              AND intent.prerequisite_contract=NEW.prerequisite_contract
              AND intent.prerequisite_epoch=NEW.prerequisite_epoch AND intent.state=NEW.recovery_state
              AND intent.write_base_revision=NEW.write_base_revision
              AND intent.committed_revision=NEW.committed_revision
              AND intent.committed_parent_revision=NEW.committed_parent_revision
              AND intent.lease_epoch=NEW.previous_lease_epoch
              AND intent.next_attempt_at<=NEW.created_at AND intent.updated_at<=NEW.created_at
              AND (intent.lease_owner IS NULL OR intent.lease_until<=NEW.created_at)
              AND (intent.lease_epoch>0 OR public.helm_protected_adoption_projection_is_fresh(
                  intent.platform_binding_id,intent.environment_binding_id,intent.cluster_id,
                  intent.project_id,intent.environment_id,intent.platform_target_ref,
                  intent.environment_target_ref,intent.environment_revision,intent.environment_generation))
              AND NOT EXISTS(SELECT 1 FROM public.helm_protected_payload_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id AND held.id<>intent.id
                    AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at)
              AND NOT EXISTS(SELECT 1 FROM public.helm_protected_application_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id
                    AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at)
        ) THEN
            RAISE EXCEPTION 'Helm payload adoption receipt does not match exact recoverable intent'
                USING ERRCODE='23514';
        END IF;
    ELSE
        SELECT intent.platform_binding_id INTO binding_id
        FROM public.helm_protected_application_intents intent WHERE intent.id=NEW.application_intent_id;
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(binding_id::text,704215997));
        IF NOT EXISTS (
            SELECT 1 FROM public.helm_protected_application_intents intent
            WHERE intent.id=NEW.application_intent_id
              AND intent.publisher_contract=NEW.publisher_contract
              AND intent.original_publisher_config_digest=NEW.original_config_digest
              AND intent.publisher_config_digest=NEW.previous_config_digest
              AND intent.publisher_adoption_epoch+1=NEW.adoption_epoch
              AND intent.intent_digest=NEW.intent_digest AND intent.content_digest=NEW.content_digest
              AND intent.application_path=NEW.protected_path AND intent.precondition=NEW.precondition
              AND intent.expected_etag=NEW.expected_etag AND intent.commit_trailer=NEW.commit_trailer
              AND intent.prerequisite_receipt_id=NEW.prerequisite_receipt_id
              AND intent.prerequisite_contract=NEW.prerequisite_contract
              AND intent.prerequisite_epoch=NEW.prerequisite_epoch AND intent.state=NEW.recovery_state
              AND intent.write_base_revision=NEW.write_base_revision
              AND intent.committed_revision=NEW.committed_revision
              AND intent.committed_parent_revision=NEW.committed_parent_revision
              AND intent.lease_epoch=NEW.previous_lease_epoch
              AND intent.next_attempt_at<=NEW.created_at AND intent.updated_at<=NEW.created_at
              AND (intent.lease_owner IS NULL OR intent.lease_until<=NEW.created_at)
              AND (intent.lease_epoch>0 OR
                (intent.continuation_required AND public.helm_application_continuation_is_exact(intent.id)) OR
                (NOT intent.continuation_required AND public.helm_protected_adoption_projection_is_fresh(
                  intent.platform_binding_id,intent.environment_binding_id,intent.cluster_id,
                  intent.project_id,intent.environment_id,intent.platform_target_ref,
                  intent.environment_target_ref,intent.environment_revision,intent.environment_generation)))
              AND NOT EXISTS(SELECT 1 FROM public.helm_protected_payload_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id
                    AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at)
              AND NOT EXISTS(SELECT 1 FROM public.helm_protected_application_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id AND held.id<>intent.id
                    AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at)
        ) THEN
            RAISE EXCEPTION 'Helm Application adoption receipt does not match exact recoverable intent'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.adopt_helm_protected_application_intent(
    receipt_id uuid,adopting_worker text,adopting_worker_epoch bigint,
    adopting_publisher_contract text,adopting_policy_version text,
    adopting_config_digest text,lease_milliseconds bigint
) RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    candidate public.helm_protected_application_intents%ROWTYPE;
    affected bigint;
BEGIN
    IF receipt_id IS NULL OR adopting_worker !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$' OR
       adopting_worker_epoch<1 OR adopting_publisher_contract<>'helm-protected-publisher.v1' OR
       adopting_policy_version<>'helm-protected-git.v1' OR
       adopting_config_digest !~ '^sha256:[0-9a-f]{64}$' OR
       lease_milliseconds<15000 OR lease_milliseconds>300000 THEN
        RAISE EXCEPTION 'invalid protected Application publisher adoption request' USING ERRCODE='23514';
    END IF;
    SELECT intent.* INTO candidate
    FROM public.helm_protected_application_intents intent
    WHERE intent.state IN ('pending','claimed','git-committed')
      AND intent.next_attempt_at<=db_now AND intent.updated_at<=db_now
      AND (intent.lease_owner IS NULL OR intent.lease_until<=db_now)
      AND intent.publisher_contract=adopting_publisher_contract
      AND intent.publisher_config_digest<>adopting_config_digest
      AND intent.prerequisite_receipt_id=intent.release_revision_id
      AND intent.prerequisite_contract='helm-publication-prerequisite.v1'
      AND (intent.lease_epoch>0 OR
        (intent.continuation_required AND public.helm_application_continuation_is_exact(intent.id)) OR
        (NOT intent.continuation_required AND public.helm_protected_adoption_projection_is_fresh(
          intent.platform_binding_id,intent.environment_binding_id,intent.cluster_id,
          intent.project_id,intent.environment_id,intent.platform_target_ref,
          intent.environment_target_ref,intent.environment_revision,intent.environment_generation)))
      AND EXISTS(SELECT 1 FROM public.runtime_readiness readiness
        WHERE readiness.runtime_kind='helm-protected-publisher' AND readiness.scope_key='global'
          AND readiness.worker_id=adopting_worker AND readiness.worker_epoch=adopting_worker_epoch
          AND readiness.contract_version=adopting_publisher_contract
          AND readiness.identity=pg_catalog.jsonb_build_object('policyVersion',adopting_policy_version)
          AND readiness.config_digest=adopting_config_digest
          AND readiness.updated_at=readiness.observed_at AND readiness.observed_at<=db_now
          AND readiness.observed_at>=db_now-interval '5 minutes' AND readiness.lease_until>db_now
          AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
          AND readiness.lease_until<=db_now+interval '5 minutes')
      AND NOT EXISTS(SELECT 1 FROM public.helm_protected_payload_intents held
        WHERE held.platform_binding_id=intent.platform_binding_id
          AND held.lease_owner IS NOT NULL AND held.lease_until>db_now)
      AND NOT EXISTS(SELECT 1 FROM public.helm_protected_application_intents held
        WHERE held.platform_binding_id=intent.platform_binding_id AND held.id<>intent.id
          AND held.lease_owner IS NOT NULL AND held.lease_until>db_now)
    ORDER BY intent.next_attempt_at,intent.created_at,intent.id
    FOR UPDATE OF intent SKIP LOCKED LIMIT 1;
    IF NOT FOUND THEN RETURN NULL; END IF;

    INSERT INTO public.helm_protected_publisher_adoption_receipts(
        id,intent_kind,payload_intent_id,application_intent_id,adoption_epoch,
        publisher_contract,original_config_digest,previous_config_digest,adopted_config_digest,
        policy_version,intent_digest,content_digest,protected_path,precondition,expected_etag,
        commit_trailer,prerequisite_receipt_id,prerequisite_contract,prerequisite_epoch,
        recovery_state,write_base_revision,committed_revision,committed_parent_revision,
        previous_lease_epoch,adopted_lease_epoch,adopted_by_worker,adopted_worker_epoch,created_at
    ) VALUES(
        receipt_id,'application',NULL,candidate.id,candidate.publisher_adoption_epoch+1,
        candidate.publisher_contract,candidate.original_publisher_config_digest,
        candidate.publisher_config_digest,adopting_config_digest,adopting_policy_version,
        candidate.intent_digest,candidate.content_digest,candidate.application_path,
        candidate.precondition,candidate.expected_etag,candidate.commit_trailer,
        candidate.prerequisite_receipt_id,candidate.prerequisite_contract,candidate.prerequisite_epoch,
        candidate.state,candidate.write_base_revision,candidate.committed_revision,
        candidate.committed_parent_revision,candidate.lease_epoch,candidate.lease_epoch+1,
        adopting_worker,adopting_worker_epoch,db_now
    );
    UPDATE public.helm_protected_application_intents intent SET
        publisher_config_digest=adopting_config_digest,
        publisher_adoption_epoch=intent.publisher_adoption_epoch+1,
        state=CASE WHEN intent.state='pending' THEN 'claimed' ELSE intent.state END,
        lease_owner=adopting_worker,lease_epoch=intent.lease_epoch+1,
        lease_until=db_now+(lease_milliseconds*interval '1 millisecond'),
        attempts=LEAST(intent.attempts+1,30),updated_at=db_now,
        prerequisite_epoch=intent.prerequisite_epoch+1
    WHERE intent.id=candidate.id
      AND intent.publisher_config_digest=candidate.publisher_config_digest
      AND intent.publisher_adoption_epoch=candidate.publisher_adoption_epoch
      AND intent.lease_epoch=candidate.lease_epoch
      AND (intent.lease_owner IS NULL OR intent.lease_until<=db_now)
      AND intent.state=candidate.state;
    GET DIAGNOSTICS affected=ROW_COUNT;
    IF affected<>1 THEN
        RAISE EXCEPTION 'protected Application publisher adoption lost its exact lock' USING ERRCODE='40001';
    END IF;
    RETURN candidate.id;
END;
$$;

REVOKE ALL ON FUNCTION public.adopt_helm_protected_application_intent(
    uuid,text,bigint,text,text,text,bigint) FROM PUBLIC;
