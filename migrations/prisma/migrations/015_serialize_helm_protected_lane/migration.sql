-- Preserve recovery of previously leased protected cascade work after its
-- diagnostic attempt counter saturates. A pristine row remains bounded by the
-- original cap; lease_epoch>0 is durable unknown-side-effect recovery state.
DO $migration$
DECLARE
    definition text;
    needle text := $needle$candidate.state NOT IN ('pending','claimed','git-committed') OR candidate.attempts>=30 OR$needle$;
    replacement text := $replacement$candidate.state NOT IN ('pending','claimed','git-committed') OR
       (candidate.attempts>=30 AND candidate.lease_epoch=0) OR$replacement$;
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_application_cascade_adoption_receipt()'::pg_catalog.regprocedure);
    IF pg_catalog.strpos(definition,needle)=0 OR
       pg_catalog.strpos(pg_catalog.substr(
           definition,pg_catalog.strpos(definition,needle)+1),needle)>0 THEN
        RAISE EXCEPTION 'unexpected cascade adoption receipt validator before retry recovery';
    END IF;
    EXECUTE pg_catalog.replace(definition,needle,replacement);
END;
$migration$;

DO $migration$
DECLARE
    definition text;
    needle text := 'AND preflight.attempts<30';
    replacement text := 'AND (preflight.attempts<30 OR preflight.lease_epoch>0)';
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.adopt_helm_application_cascade_preflight(uuid,text,bigint,text,text,text,bigint)'::pg_catalog.regprocedure);
    IF pg_catalog.strpos(definition,needle)=0 OR
       pg_catalog.strpos(pg_catalog.substr(
           definition,pg_catalog.strpos(definition,needle)+1),needle)>0 THEN
        RAISE EXCEPTION 'unexpected cascade adopter before retry recovery';
    END IF;
    EXECUTE pg_catalog.replace(definition,needle,replacement);
END;
$migration$;

-- The immutable v014 receipt guard acquired the preflight row before the
-- platform binding. Activation uses binding then advisory. Resolve the exact
-- binding/head authority first, then take the shared advisory lane and finally
-- lock the preflight row, eliminating the inverse wait cycle without weakening
-- the deferred receipt/failure postimage checks.
CREATE OR REPLACE FUNCTION public.validate_helm_application_cascade_absence_receipt()
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
    authority_found boolean := false;
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'cascade path-absence receipts are immutable' USING ERRCODE='23514';
    END IF;
    SELECT preflight.* INTO candidate
    FROM public.helm_application_cascade_preflights AS preflight
    WHERE preflight.id=NEW.cascade_preflight_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade path-absence preflight is absent' USING ERRCODE='23514';
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
    authority_found := FOUND;
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(candidate.platform_binding_id::text,704215997));
    SELECT preflight.* INTO candidate
    FROM public.helm_application_cascade_preflights AS preflight
    WHERE preflight.id=NEW.cascade_preflight_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade path-absence preflight is absent' USING ERRCODE='23514';
    END IF;
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
    IF NOT authority_found OR candidate.state<>'claimed' OR candidate.operation<>'update' OR
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

ALTER FUNCTION public.validate_helm_application_cascade_adoption_receipt()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.adopt_helm_application_cascade_preflight(
    uuid,text,bigint,text,text,text,bigint)
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_absence_receipt()
SET search_path=pg_catalog,pg_temp;
