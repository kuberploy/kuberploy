-- Terminalize a legacy cascade-finalizer adoption only when the exact
-- provider-pinned Application path is already absent and bounded Git recovery
-- proves that Kuberploy did not commit the adoption operation.  This is a
-- recovery-required failure, never permission to recreate historical desired
-- state or to reclaim/replace the failed preflight.

CREATE TABLE public.helm_application_cascade_absence_receipts (
    cascade_preflight_id uuid PRIMARY KEY,
    release_revision_id uuid NOT NULL,
    payload_intent_id uuid NOT NULL,
    base_application_intent_id uuid NOT NULL,
    release_generation bigint NOT NULL CHECK (release_generation>1),
    platform_binding_id uuid NOT NULL,
    platform_binding_state text NOT NULL CHECK (platform_binding_state IN ('ready','indexing')),
    platform_indexed_revision text NOT NULL CHECK (
        platform_indexed_revision='' OR platform_indexed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    application_path text NOT NULL CHECK (
        length(application_path) BETWEEN 1 AND 1024 AND
        application_path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'),
    source_content_digest text NOT NULL CHECK (source_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    adopted_content_digest text NOT NULL CHECK (adopted_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    expected_etag text NOT NULL CHECK (expected_etag ~ '^"sha256:[0-9a-f]{64}"$'),
    provider_head text NOT NULL CHECK (provider_head ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    provider_request text NOT NULL CHECK (
        length(provider_request) BETWEEN 1 AND 256 AND provider_request !~ '[[:cntrl:]]'),
    provider_observed_at timestamptz NOT NULL,
    operation_commit_absent boolean NOT NULL CHECK (operation_commit_absent),
    publisher_contract text NOT NULL CHECK (publisher_contract='helm-protected-publisher.v1'),
    publisher_policy_version text NOT NULL CHECK (publisher_policy_version='helm-protected-git.v1'),
    publisher_config_digest text NOT NULL CHECK (publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_worker_id text NOT NULL CHECK (
        length(publisher_worker_id) BETWEEN 16 AND 128 AND
        publisher_worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'),
    publisher_worker_epoch bigint NOT NULL CHECK (publisher_worker_epoch>0),
    lease_epoch bigint NOT NULL CHECK (lease_epoch>0),
    recorded_at timestamptz NOT NULL,
    FOREIGN KEY (cascade_preflight_id)
        REFERENCES public.helm_application_cascade_preflights(id) ON DELETE RESTRICT,
    FOREIGN KEY (release_revision_id)
        REFERENCES public.helm_release_revisions(id) ON DELETE RESTRICT,
    FOREIGN KEY (payload_intent_id)
        REFERENCES public.helm_protected_payload_intents(id) ON DELETE RESTRICT,
    FOREIGN KEY (base_application_intent_id)
        REFERENCES public.helm_protected_application_intents(id) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id)
        REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT
);

CREATE FUNCTION public.helm_application_cascade_absence_receipt_is_exact(candidate_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT EXISTS (
    SELECT 1
    FROM public.helm_application_cascade_preflights AS preflight
    JOIN public.helm_application_cascade_absence_receipts AS receipt
      ON receipt.cascade_preflight_id=preflight.id
     AND receipt.release_revision_id=preflight.release_revision_id
     AND receipt.payload_intent_id=preflight.payload_intent_id
     AND receipt.base_application_intent_id=preflight.base_application_intent_id
     AND receipt.release_generation=preflight.release_generation
     AND receipt.platform_binding_id=preflight.platform_binding_id
     AND receipt.application_path=preflight.application_path
     AND receipt.source_content_digest=preflight.source_content_digest
     AND receipt.adopted_content_digest=preflight.adopted_content_digest
     AND receipt.expected_etag=preflight.expected_etag
     AND receipt.publisher_contract=preflight.publisher_contract
     AND receipt.publisher_policy_version=preflight.publisher_policy_version
     AND receipt.publisher_config_digest=preflight.publisher_config_digest
     AND receipt.lease_epoch=preflight.lease_epoch
     AND receipt.recorded_at=preflight.updated_at
    JOIN public.helm_release_heads AS head
      ON head.environment_id=preflight.environment_id
     AND head.application_id=preflight.application_id
     AND head.revision_id=preflight.release_revision_id
     AND head.generation=preflight.release_generation
    JOIN public.helm_release_revisions AS release
      ON release.id=head.revision_id
     AND release.generation=head.generation
     AND release.project_id=preflight.project_id
     AND release.environment_id=preflight.environment_id
     AND release.application_id=preflight.application_id
     AND release.action='disable' AND NOT release.desired_enabled
     AND release.base_intent_id=preflight.base_application_intent_id
    JOIN public.helm_protected_payload_intents AS payload
      ON payload.id=preflight.payload_intent_id
     AND payload.release_revision_id=preflight.release_revision_id
     AND payload.release_generation=preflight.release_generation
     AND payload.project_id=preflight.project_id
     AND payload.environment_id=preflight.environment_id
     AND payload.application_id=preflight.application_id
     AND payload.platform_binding_id=preflight.platform_binding_id
     AND payload.environment_binding_id=preflight.environment_binding_id
     AND payload.cluster_id=preflight.cluster_id
     AND payload.state='verified' AND payload.action='disable-receipt'
     AND payload.committed_revision=preflight.payload_revision
    JOIN public.helm_protected_application_intents AS base
      ON base.id=preflight.base_application_intent_id
     AND base.state='verified' AND base.action='publish'
     AND base.project_id=preflight.project_id
     AND base.environment_id=preflight.environment_id
     AND base.application_id=preflight.application_id
     AND base.platform_binding_id=preflight.platform_binding_id
     AND base.cluster_id=preflight.cluster_id
     AND base.application_path=preflight.application_path
     AND base.content=preflight.source_content
     AND base.content_digest=preflight.source_content_digest
    JOIN public.git_repository_bindings AS binding
      ON binding.id=preflight.platform_binding_id
     AND binding.kind='platform' AND binding.credential_mode='github-app'
     AND binding.cluster_id=preflight.cluster_id
     AND binding.target_ref=preflight.platform_target_ref
     AND binding.path_prefix='clusters/'||preflight.cluster_id::text
     AND binding.state=receipt.platform_binding_state
     AND COALESCE(binding.indexed_revision,'')=receipt.platform_indexed_revision
     AND binding.target_head_revision=receipt.provider_head
    WHERE preflight.id=candidate_id
      AND preflight.state='failed'
      AND preflight.operation='update'
      AND preflight.last_failure_code='cascade-path-absent-recovery-required'
      AND preflight.committed_revision=''
      AND preflight.committed_parent_revision=''
      AND preflight.committed_at IS NULL
      AND preflight.verified_at IS NULL
      AND preflight.verified_path_digest=''
      AND preflight.provider_request=''
      AND preflight.lease_owner IS NULL
      AND preflight.lease_until IS NULL
      AND preflight.completed_at=receipt.recorded_at
      AND receipt.operation_commit_absent
);
$function$;

-- A later enabled release may recreate the stable Application path only when
-- its direct disabled parent terminalized under an exact immutable absence
-- receipt.  create-if-absent remains the final Git CAS; this authority never
-- turns the failed preflight itself back into mutable work.
CREATE FUNCTION public.helm_application_cascade_recovery_create_is_authorized(
    candidate_release_id uuid,candidate_base_intent_id uuid,candidate_application_path text)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT EXISTS (
    SELECT 1
    FROM public.helm_release_heads AS head
    JOIN public.helm_release_revisions AS release
      ON release.id=head.revision_id
     AND release.id=candidate_release_id
     AND release.generation=head.generation
     AND release.desired_enabled
     AND release.action IN ('update','rollback')
     AND release.base_intent_id=candidate_base_intent_id
    JOIN public.helm_release_revisions AS disabled
      ON disabled.id=release.parent_revision_id
     AND disabled.action='disable' AND NOT disabled.desired_enabled
    JOIN public.helm_application_cascade_preflights AS preflight
      ON preflight.release_revision_id=disabled.id
     AND preflight.base_application_intent_id=candidate_base_intent_id
     AND preflight.application_path=candidate_application_path
     AND preflight.state='failed'
     AND preflight.operation='update'
     AND preflight.last_failure_code='cascade-path-absent-recovery-required'
     AND preflight.committed_revision=''
     AND preflight.committed_parent_revision=''
     AND preflight.committed_at IS NULL
     AND preflight.verified_at IS NULL
     AND preflight.verified_path_digest=''
     AND preflight.provider_request=''
     AND preflight.lease_owner IS NULL
     AND preflight.lease_until IS NULL
    JOIN public.helm_application_cascade_absence_receipts AS receipt
      ON receipt.cascade_preflight_id=preflight.id
     AND receipt.release_revision_id=preflight.release_revision_id
     AND receipt.payload_intent_id=preflight.payload_intent_id
     AND receipt.base_application_intent_id=preflight.base_application_intent_id
     AND receipt.release_generation=preflight.release_generation
     AND receipt.platform_binding_id=preflight.platform_binding_id
     AND receipt.application_path=preflight.application_path
     AND receipt.source_content_digest=preflight.source_content_digest
     AND receipt.adopted_content_digest=preflight.adopted_content_digest
     AND receipt.expected_etag=preflight.expected_etag
     AND receipt.publisher_contract=preflight.publisher_contract
     AND receipt.publisher_policy_version=preflight.publisher_policy_version
     AND receipt.publisher_config_digest=preflight.publisher_config_digest
     AND receipt.lease_epoch=preflight.lease_epoch
     AND receipt.recorded_at=preflight.updated_at
     AND receipt.recorded_at=preflight.completed_at
     AND receipt.operation_commit_absent
    JOIN public.helm_protected_payload_intents AS disabled_payload
      ON disabled_payload.id=preflight.payload_intent_id
     AND disabled_payload.release_revision_id=disabled.id
     AND disabled_payload.state='verified'
     AND disabled_payload.action='disable-receipt'
     AND disabled_payload.committed_revision=preflight.payload_revision
    JOIN public.helm_protected_application_intents AS base
      ON base.id=preflight.base_application_intent_id
     AND base.id=candidate_base_intent_id
     AND base.state='verified' AND base.action='publish'
     AND base.application_path=preflight.application_path
     AND base.content=preflight.source_content
     AND base.content_digest=preflight.source_content_digest
    WHERE head.environment_id=release.environment_id
      AND head.application_id=release.application_id
      AND disabled.project_id=release.project_id
      AND disabled.environment_id=release.environment_id
      AND disabled.application_id=release.application_id
      AND preflight.project_id=release.project_id
      AND preflight.environment_id=release.environment_id
      AND preflight.application_id=release.application_id
);
$function$;

CREATE FUNCTION public.validate_helm_application_cascade_absence_receipt()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    candidate public.helm_application_cascade_preflights%ROWTYPE;
    active_worker_epoch bigint;
    binding_state text;
    binding_indexed_revision text;
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'cascade path-absence receipts are immutable' USING ERRCODE='23514';
    END IF;
    SELECT preflight.* INTO candidate
    FROM public.helm_application_cascade_preflights AS preflight
    WHERE preflight.id=NEW.cascade_preflight_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade path-absence preflight is absent' USING ERRCODE='23514';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(candidate.platform_binding_id::text,704215997));
    SELECT activation.publisher_worker_epoch INTO active_worker_epoch
    FROM public.helm_application_cascade_observer_activations AS activation
    WHERE activation.platform_binding_id=candidate.platform_binding_id
      AND activation.activation_epoch=(
          SELECT MAX(current.activation_epoch)
          FROM public.helm_application_cascade_observer_activations AS current
          WHERE current.platform_binding_id=candidate.platform_binding_id)
      AND activation.publisher_worker_id=candidate.lease_owner
      AND activation.publisher_contract=candidate.publisher_contract
      AND activation.publisher_policy_version=candidate.publisher_policy_version
      AND activation.publisher_config_digest=candidate.publisher_config_digest;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade path-absence receipt lacks active publisher activation'
            USING ERRCODE='23514';
    END IF;
    SELECT binding.state,COALESCE(binding.indexed_revision,'')
      INTO binding_state,binding_indexed_revision
    FROM public.helm_release_heads AS head
    JOIN public.helm_release_revisions AS release
      ON release.id=head.revision_id
    JOIN public.helm_protected_payload_intents AS payload
      ON payload.id=candidate.payload_intent_id
    JOIN public.helm_protected_application_intents AS base
      ON base.id=candidate.base_application_intent_id
    JOIN public.git_repository_bindings AS binding
      ON binding.id=candidate.platform_binding_id
    WHERE head.environment_id=candidate.environment_id
      AND head.application_id=candidate.application_id
      AND head.revision_id=candidate.release_revision_id
      AND head.generation=candidate.release_generation
      AND release.generation=candidate.release_generation
      AND release.project_id=candidate.project_id
      AND release.environment_id=candidate.environment_id
      AND release.application_id=candidate.application_id
      AND release.action='disable' AND NOT release.desired_enabled
      AND release.base_intent_id=candidate.base_application_intent_id
      AND payload.release_revision_id=candidate.release_revision_id
      AND payload.release_generation=candidate.release_generation
      AND payload.project_id=candidate.project_id
      AND payload.environment_id=candidate.environment_id
      AND payload.application_id=candidate.application_id
      AND payload.platform_binding_id=candidate.platform_binding_id
      AND payload.environment_binding_id=candidate.environment_binding_id
      AND payload.cluster_id=candidate.cluster_id
      AND payload.state='verified' AND payload.action='disable-receipt'
      AND payload.committed_revision=candidate.payload_revision
      AND base.state='verified' AND base.action='publish'
      AND base.project_id=candidate.project_id
      AND base.environment_id=candidate.environment_id
      AND base.application_id=candidate.application_id
      AND base.platform_binding_id=candidate.platform_binding_id
      AND base.cluster_id=candidate.cluster_id
      AND base.application_path=candidate.application_path
      AND base.content=candidate.source_content
      AND base.content_digest=candidate.source_content_digest
      AND binding.kind='platform' AND binding.credential_mode='github-app'
      AND binding.cluster_id=candidate.cluster_id
      AND binding.target_ref=candidate.platform_target_ref
      AND binding.path_prefix='clusters/'||candidate.cluster_id::text
      AND binding.state IN ('ready','indexing')
      AND binding.target_head_revision=NEW.provider_head
    FOR UPDATE OF head,binding FOR KEY SHARE OF release,payload,base;
    IF NOT FOUND OR candidate.state<>'claimed' OR candidate.operation<>'update' OR
       candidate.lease_owner IS NULL OR candidate.lease_until<=db_now OR
       candidate.committed_revision<>'' OR candidate.committed_parent_revision<>'' OR
       candidate.committed_at IS NOT NULL OR candidate.verified_at IS NOT NULL OR
       candidate.verified_path_digest<>'' OR candidate.provider_request<>'' OR
       NEW.provider_head IS NULL OR NEW.provider_request IS NULL OR
       NEW.provider_observed_at IS NULL OR NOT NEW.operation_commit_absent OR
       NEW.provider_head !~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$' OR
       length(NEW.provider_request) NOT BETWEEN 1 AND 256 OR
       NEW.provider_request ~ '[[:cntrl:]]' OR
       NEW.provider_observed_at>db_now OR
       NEW.provider_observed_at<db_now-interval '5 minutes' OR
       candidate.updated_at>db_now OR
       NOT public.helm_application_active_publisher_is_exact(
           candidate.platform_binding_id,candidate.lease_owner,
           candidate.publisher_contract,candidate.publisher_config_digest,db_now) THEN
        RAISE EXCEPTION 'cascade path-absence receipt lacks exact no-effect authority'
            USING ERRCODE='23514';
    END IF;
    NEW.release_revision_id := candidate.release_revision_id;
    NEW.payload_intent_id := candidate.payload_intent_id;
    NEW.base_application_intent_id := candidate.base_application_intent_id;
    NEW.release_generation := candidate.release_generation;
    NEW.platform_binding_id := candidate.platform_binding_id;
    NEW.platform_binding_state := binding_state;
    NEW.platform_indexed_revision := binding_indexed_revision;
    NEW.application_path := candidate.application_path;
    NEW.source_content_digest := candidate.source_content_digest;
    NEW.adopted_content_digest := candidate.adopted_content_digest;
    NEW.expected_etag := candidate.expected_etag;
    NEW.publisher_contract := candidate.publisher_contract;
    NEW.publisher_policy_version := candidate.publisher_policy_version;
    NEW.publisher_config_digest := candidate.publisher_config_digest;
    NEW.publisher_worker_id := candidate.lease_owner;
    NEW.publisher_worker_epoch := active_worker_epoch;
    NEW.lease_epoch := candidate.lease_epoch;
    NEW.recorded_at := db_now;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_absence_receipt_guard
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_application_cascade_absence_receipts
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_absence_receipt();

CREATE FUNCTION public.validate_helm_application_cascade_absence_transition()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
BEGIN
    IF TG_OP='UPDATE' AND NEW.state='failed' AND
       NEW.last_failure_code='cascade-path-absent-recovery-required' AND
       NOT EXISTS (
           SELECT 1
           FROM public.helm_application_cascade_absence_receipts AS receipt
           WHERE receipt.cascade_preflight_id=NEW.id
             AND receipt.release_revision_id=NEW.release_revision_id
             AND receipt.payload_intent_id=NEW.payload_intent_id
             AND receipt.base_application_intent_id=NEW.base_application_intent_id
             AND receipt.release_generation=NEW.release_generation
             AND receipt.platform_binding_id=NEW.platform_binding_id
             AND receipt.application_path=NEW.application_path
             AND receipt.source_content_digest=NEW.source_content_digest
             AND receipt.adopted_content_digest=NEW.adopted_content_digest
             AND receipt.expected_etag=NEW.expected_etag
             AND receipt.publisher_contract=NEW.publisher_contract
             AND receipt.publisher_policy_version=NEW.publisher_policy_version
             AND receipt.publisher_config_digest=NEW.publisher_config_digest
             AND receipt.publisher_worker_id=OLD.lease_owner
             AND receipt.lease_epoch=NEW.lease_epoch
             AND receipt.recorded_at=NEW.updated_at
             AND receipt.operation_commit_absent
       ) THEN
        RAISE EXCEPTION 'cascade path-absence failure lacks its exact receipt'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_absence_transition_guard
BEFORE UPDATE ON public.helm_application_cascade_preflights
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_absence_transition();

CREATE FUNCTION public.validate_helm_application_cascade_absence_postimage()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
BEGIN
    IF NOT public.helm_application_cascade_absence_receipt_is_exact(NEW.cascade_preflight_id) THEN
        RAISE EXCEPTION 'cascade path-absence receipt postimage is not exact'
            USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$function$;

CREATE FUNCTION public.validate_helm_application_cascade_absence_failure_postimage()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
BEGIN
    IF NOT public.helm_application_cascade_absence_receipt_is_exact(NEW.id) THEN
        RAISE EXCEPTION 'cascade path-absence failure postimage is not exact'
            USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$function$;

CREATE CONSTRAINT TRIGGER helm_application_cascade_absence_receipt_postimage
AFTER INSERT ON public.helm_application_cascade_absence_receipts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_absence_postimage();

CREATE CONSTRAINT TRIGGER helm_application_cascade_absence_failure_postimage
AFTER UPDATE ON public.helm_application_cascade_preflights
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
WHEN (NEW.state='failed' AND NEW.last_failure_code='cascade-path-absent-recovery-required')
EXECUTE FUNCTION public.validate_helm_application_cascade_absence_failure_postimage();

DO $migration$
DECLARE
    definition text;
    needle text := 'IF release_row.base_intent_id IS NULL THEN';
    replacement text := $replacement$IF release_row.base_intent_id IS NULL OR
       public.helm_application_cascade_recovery_create_is_authorized(
           NEW.release_revision_id,release_row.base_intent_id,NEW.application_path) THEN$replacement$;
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_protected_application_intent()'::pg_catalog.regprocedure);
    IF pg_catalog.strpos(definition,needle)=0 OR
       pg_catalog.strpos(pg_catalog.substr(
           definition,pg_catalog.strpos(definition,needle)+1),needle)>0 THEN
        RAISE EXCEPTION 'unexpected protected Application validator shape before absence recovery';
    END IF;
    definition := pg_catalog.replace(definition,needle,replacement);
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.validate_helm_protected_application_intent()
SET search_path=pg_catalog,pg_temp;

ALTER TABLE public.helm_application_cascade_absence_receipts ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON TABLE public.helm_application_cascade_absence_receipts FROM PUBLIC;
REVOKE ALL ON FUNCTION public.helm_application_cascade_absence_receipt_is_exact(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.helm_application_cascade_recovery_create_is_authorized(uuid,uuid,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.validate_helm_application_cascade_absence_receipt() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.validate_helm_application_cascade_absence_transition() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.validate_helm_application_cascade_absence_postimage() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.validate_helm_application_cascade_absence_failure_postimage() FROM PUBLIC;
