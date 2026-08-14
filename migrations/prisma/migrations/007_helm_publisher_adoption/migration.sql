-- A protected Helm Git intent can outlive the exact worker runtime which first
-- claimed it. Preserve the immutable creation identity while allowing a fresh,
-- exact publisher runtime to recover that same trailer/commit receipt through
-- one durable, database-authorized adoption epoch.
LOCK TABLE public.helm_protected_payload_intents IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE public.helm_protected_application_intents IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE public.helm_protected_payload_intents
ADD COLUMN original_publisher_config_digest text,
ADD COLUMN publisher_adoption_epoch bigint NOT NULL DEFAULT 0;

ALTER TABLE public.helm_protected_application_intents
ADD COLUMN original_publisher_config_digest text,
ADD COLUMN publisher_adoption_epoch bigint NOT NULL DEFAULT 0;

ALTER TABLE public.helm_protected_payload_intents DISABLE TRIGGER USER;
ALTER TABLE public.helm_protected_application_intents DISABLE TRIGGER USER;

UPDATE public.helm_protected_payload_intents
SET original_publisher_config_digest=publisher_config_digest;

UPDATE public.helm_protected_application_intents
SET original_publisher_config_digest=publisher_config_digest;

ALTER TABLE public.helm_protected_payload_intents ENABLE TRIGGER USER;
ALTER TABLE public.helm_protected_application_intents ENABLE TRIGGER USER;

ALTER TABLE public.helm_protected_payload_intents
ALTER COLUMN original_publisher_config_digest SET NOT NULL,
ADD CONSTRAINT helm_protected_payload_original_publisher_digest_check
  CHECK (original_publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
ADD CONSTRAINT helm_protected_payload_publisher_adoption_epoch_check
  CHECK (publisher_adoption_epoch>=0);

ALTER TABLE public.helm_protected_application_intents
ALTER COLUMN original_publisher_config_digest SET NOT NULL,
ADD CONSTRAINT helm_protected_application_original_publisher_digest_check
  CHECK (original_publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
ADD CONSTRAINT helm_protected_application_publisher_adoption_epoch_check
  CHECK (publisher_adoption_epoch>=0);

-- The baseline validators deliberately made publisher_config_digest immutable.
-- It is now the active authority so an old binary's exact WHERE predicate stops
-- matching immediately after adoption. Move only that immutable identity slot
-- to original_publisher_config_digest; every other baseline invariant remains
-- byte-for-byte the already-applied validator body.
DO $migration$
DECLARE
    definition text;
BEGIN
    definition := pg_catalog.pg_get_functiondef('public.validate_helm_protected_payload_intent()'::regprocedure);
    IF position('NEW.publisher_contract,NEW.publisher_config_digest,NEW.message' in definition)=0 OR
       position('OLD.publisher_contract,OLD.publisher_config_digest,OLD.message' in definition)=0 THEN
        RAISE EXCEPTION 'unexpected protected payload validator prerequisite';
    END IF;
    definition := replace(definition,
        'NEW.publisher_contract,NEW.publisher_config_digest,NEW.message',
        'NEW.publisher_contract,NEW.original_publisher_config_digest,NEW.message');
    definition := replace(definition,
        'OLD.publisher_contract,OLD.publisher_config_digest,OLD.message',
        'OLD.publisher_contract,OLD.original_publisher_config_digest,OLD.message');
    EXECUTE definition;

    definition := pg_catalog.pg_get_functiondef('public.validate_helm_protected_application_intent()'::regprocedure);
    IF position('NEW.publisher_contract,NEW.publisher_config_digest,NEW.message' in definition)=0 OR
       position('OLD.publisher_contract,OLD.publisher_config_digest,OLD.message' in definition)=0 THEN
        RAISE EXCEPTION 'unexpected protected Application validator prerequisite';
    END IF;
    definition := replace(definition,
        'NEW.publisher_contract,NEW.publisher_config_digest,NEW.message',
        'NEW.publisher_contract,NEW.original_publisher_config_digest,NEW.message');
    definition := replace(definition,
        'OLD.publisher_contract,OLD.publisher_config_digest,OLD.message',
        'OLD.publisher_contract,OLD.original_publisher_config_digest,OLD.message');
    EXECUTE definition;

    definition := pg_catalog.pg_get_functiondef(
        'public.require_helm_publication_prerequisite_receipt()'::regprocedure
    );
    IF position('FROM helm_publication_prerequisite_receipts receipt' in definition)=0 THEN
        RAISE EXCEPTION 'unexpected Helm prerequisite validator relation prerequisite';
    END IF;
    definition := replace(definition,
        'FROM helm_publication_prerequisite_receipts receipt',
        'FROM public.helm_publication_prerequisite_receipts receipt');
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.require_helm_publication_prerequisite_receipt()
SET search_path=pg_catalog,pg_temp;

CREATE TABLE public.helm_protected_publisher_adoption_receipts (
    id uuid PRIMARY KEY,
    intent_kind text NOT NULL CHECK (intent_kind IN ('payload','application')),
    payload_intent_id uuid REFERENCES public.helm_protected_payload_intents(id),
    application_intent_id uuid REFERENCES public.helm_protected_application_intents(id),
    adoption_epoch bigint NOT NULL CHECK (adoption_epoch>0),
    publisher_contract text NOT NULL CHECK (publisher_contract='helm-protected-publisher.v1'),
    original_config_digest text NOT NULL CHECK (original_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    previous_config_digest text NOT NULL CHECK (previous_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    adopted_config_digest text NOT NULL CHECK (adopted_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    policy_version text NOT NULL CHECK (policy_version='helm-protected-git.v1'),
    intent_digest text NOT NULL CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    content_digest text NOT NULL CHECK (content_digest='' OR content_digest ~ '^sha256:[0-9a-f]{64}$'),
    protected_path text NOT NULL CHECK (
      length(protected_path)>=1 AND length(protected_path)<=1024 AND
      protected_path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'
    ),
    precondition text NOT NULL CHECK (precondition IN ('create-if-absent','match-etag')),
    expected_etag text NOT NULL CHECK (
      expected_etag='' OR expected_etag ~ '^"sha256:[0-9a-f]{64}"$'
    ),
    commit_trailer text NOT NULL CHECK (length(commit_trailer)>=40 AND length(commit_trailer)<=128),
    prerequisite_receipt_id uuid NOT NULL REFERENCES public.helm_publication_prerequisite_receipts(release_revision_id),
    prerequisite_contract text NOT NULL CHECK (prerequisite_contract='helm-publication-prerequisite.v1'),
    prerequisite_epoch bigint NOT NULL CHECK (prerequisite_epoch>=0),
    recovery_state text NOT NULL CHECK (recovery_state IN ('pending','claimed','git-committed')),
    write_base_revision text NOT NULL CHECK (
      write_base_revision='' OR write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_revision text NOT NULL CHECK (
      committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    committed_parent_revision text NOT NULL CHECK (
      committed_parent_revision='' OR committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'
    ),
    previous_lease_epoch bigint NOT NULL CHECK (previous_lease_epoch>=0),
    adopted_lease_epoch bigint NOT NULL CHECK (adopted_lease_epoch>0),
    adopted_by_worker text NOT NULL CHECK (
      length(adopted_by_worker)>=16 AND length(adopted_by_worker)<=128 AND
      adopted_by_worker ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'
    ),
    adopted_worker_epoch bigint NOT NULL CHECK (adopted_worker_epoch>0),
    created_at timestamptz NOT NULL,
    CONSTRAINT helm_protected_publisher_adoption_exact_kind CHECK (
      (intent_kind='payload' AND payload_intent_id IS NOT NULL AND application_intent_id IS NULL) OR
      (intent_kind='application' AND application_intent_id IS NOT NULL AND payload_intent_id IS NULL)
    ),
    CONSTRAINT helm_protected_publisher_adoption_changes_authority
      CHECK (previous_config_digest<>adopted_config_digest),
    CONSTRAINT helm_protected_publisher_adoption_lease_epoch_step
      CHECK (adopted_lease_epoch=previous_lease_epoch+1)
);

CREATE UNIQUE INDEX helm_protected_publisher_payload_adoption_epoch
ON public.helm_protected_publisher_adoption_receipts(payload_intent_id,adoption_epoch)
WHERE payload_intent_id IS NOT NULL;

CREATE UNIQUE INDEX helm_protected_publisher_application_adoption_epoch
ON public.helm_protected_publisher_adoption_receipts(application_intent_id,adoption_epoch)
WHERE application_intent_id IS NOT NULL;

-- Publisher readiness is admitted against bounded database time. This extra
-- trigger is deliberately symmetric: owner-issued direct DML and the normal
-- Go store must satisfy the same exact timestamp contract.
CREATE FUNCTION public.validate_helm_protected_publisher_readiness_bounds()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    value public.runtime_readiness%ROWTYPE;
BEGIN
    value := CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
    IF value.runtime_kind<>'helm-protected-publisher' THEN
        RETURN CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
    END IF;
    IF TG_OP='DELETE' THEN
        RETURN OLD;
    END IF;
    IF NEW.updated_at<>NEW.observed_at OR
       NEW.observed_at<db_now-interval '5 minutes' OR
       NEW.observed_at>db_now+interval '30 seconds' OR
       NEW.lease_until>NEW.observed_at+interval '5 minutes' OR
       NEW.lease_until>db_now+interval '5 minutes 30 seconds' THEN
        RAISE EXCEPTION 'Helm protected publisher readiness is outside bounded database time'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER runtime_readiness_helm_publisher_bounds
BEFORE INSERT OR UPDATE OR DELETE ON public.runtime_readiness
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_publisher_readiness_bounds();

CREATE FUNCTION public.validate_helm_protected_publisher_adoption_receipt()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    binding_id uuid;
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'Helm protected publisher adoption receipts are immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.created_at>db_now OR NEW.created_at<db_now-interval '30 seconds' THEN
        RAISE EXCEPTION 'Helm publisher adoption receipt is outside bounded database time'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.runtime_readiness readiness
        WHERE readiness.runtime_kind='helm-protected-publisher'
          AND readiness.scope_key='global'
          AND readiness.worker_id=NEW.adopted_by_worker
          AND readiness.worker_epoch=NEW.adopted_worker_epoch
          AND readiness.contract_version=NEW.publisher_contract
          AND readiness.identity=jsonb_build_object('policyVersion',NEW.policy_version)
          AND readiness.config_digest=NEW.adopted_config_digest
          AND readiness.updated_at=readiness.observed_at
          AND readiness.observed_at<=NEW.created_at
          AND readiness.observed_at>=NEW.created_at-interval '5 minutes'
          AND readiness.lease_until>NEW.created_at
          AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
          AND readiness.lease_until<=NEW.created_at+interval '5 minutes'
    ) THEN
        RAISE EXCEPTION 'Helm publisher adoption lacks exact fresh worker readiness'
            USING ERRCODE='23514';
    END IF;
    IF NEW.intent_kind='payload' THEN
        SELECT intent.platform_binding_id INTO binding_id
        FROM public.helm_protected_payload_intents intent
        WHERE intent.id=NEW.payload_intent_id;
        PERFORM pg_catalog.pg_advisory_xact_lock(
            pg_catalog.hashtextextended(binding_id::text,704215997)
        );
        IF NOT EXISTS (
            SELECT 1 FROM public.helm_protected_payload_intents intent
            WHERE intent.id=NEW.payload_intent_id
              AND intent.publisher_contract=NEW.publisher_contract
              AND intent.original_publisher_config_digest=NEW.original_config_digest
              AND intent.publisher_config_digest=NEW.previous_config_digest
              AND intent.publisher_adoption_epoch+1=NEW.adoption_epoch
              AND intent.intent_digest=NEW.intent_digest
              AND intent.content_digest=NEW.content_digest
              AND intent.path=NEW.protected_path
              AND intent.precondition=NEW.precondition
              AND intent.expected_etag=NEW.expected_etag
              AND intent.commit_trailer=NEW.commit_trailer
              AND intent.prerequisite_receipt_id=NEW.prerequisite_receipt_id
              AND intent.prerequisite_contract=NEW.prerequisite_contract
              AND intent.prerequisite_epoch=NEW.prerequisite_epoch
              AND intent.state=NEW.recovery_state
              AND intent.write_base_revision=NEW.write_base_revision
              AND intent.committed_revision=NEW.committed_revision
              AND intent.committed_parent_revision=NEW.committed_parent_revision
              AND intent.lease_epoch=NEW.previous_lease_epoch
              AND intent.next_attempt_at<=NEW.created_at
              AND intent.updated_at<=NEW.created_at
              AND (intent.lease_owner IS NULL OR intent.lease_until<=NEW.created_at)
              AND (intent.lease_epoch>0 OR public.helm_protected_adoption_projection_is_fresh(
                  intent.platform_binding_id,intent.environment_binding_id,intent.cluster_id,
                  intent.project_id,intent.environment_id,intent.platform_target_ref,
                  intent.environment_target_ref,intent.environment_revision,
                  intent.environment_generation
              ))
              AND NOT EXISTS(
                  SELECT 1 FROM public.helm_protected_payload_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id
                    AND held.id<>intent.id AND held.lease_owner IS NOT NULL
                    AND held.lease_until>NEW.created_at
              )
              AND NOT EXISTS(
                  SELECT 1 FROM public.helm_protected_application_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id
                    AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at
              )
        ) THEN
            RAISE EXCEPTION 'Helm payload adoption receipt does not match exact recoverable intent'
                USING ERRCODE='23514';
        END IF;
    ELSE
        SELECT intent.platform_binding_id INTO binding_id
        FROM public.helm_protected_application_intents intent
        WHERE intent.id=NEW.application_intent_id;
        PERFORM pg_catalog.pg_advisory_xact_lock(
            pg_catalog.hashtextextended(binding_id::text,704215997)
        );
        IF NOT EXISTS (
            SELECT 1 FROM public.helm_protected_application_intents intent
            WHERE intent.id=NEW.application_intent_id
              AND intent.publisher_contract=NEW.publisher_contract
              AND intent.original_publisher_config_digest=NEW.original_config_digest
              AND intent.publisher_config_digest=NEW.previous_config_digest
              AND intent.publisher_adoption_epoch+1=NEW.adoption_epoch
              AND intent.intent_digest=NEW.intent_digest
              AND intent.content_digest=NEW.content_digest
              AND intent.application_path=NEW.protected_path
              AND intent.precondition=NEW.precondition
              AND intent.expected_etag=NEW.expected_etag
              AND intent.commit_trailer=NEW.commit_trailer
              AND intent.prerequisite_receipt_id=NEW.prerequisite_receipt_id
              AND intent.prerequisite_contract=NEW.prerequisite_contract
              AND intent.prerequisite_epoch=NEW.prerequisite_epoch
              AND intent.state=NEW.recovery_state
              AND intent.write_base_revision=NEW.write_base_revision
              AND intent.committed_revision=NEW.committed_revision
              AND intent.committed_parent_revision=NEW.committed_parent_revision
              AND intent.lease_epoch=NEW.previous_lease_epoch
              AND intent.next_attempt_at<=NEW.created_at
              AND intent.updated_at<=NEW.created_at
              AND (intent.lease_owner IS NULL OR intent.lease_until<=NEW.created_at)
              AND (intent.lease_epoch>0 OR public.helm_protected_adoption_projection_is_fresh(
                  intent.platform_binding_id,intent.environment_binding_id,intent.cluster_id,
                  intent.project_id,intent.environment_id,intent.platform_target_ref,
                  intent.environment_target_ref,intent.environment_revision,
                  intent.environment_generation
              ))
              AND NOT EXISTS(
                  SELECT 1 FROM public.helm_protected_payload_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id
                    AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at
              )
              AND NOT EXISTS(
                  SELECT 1 FROM public.helm_protected_application_intents held
                  WHERE held.platform_binding_id=intent.platform_binding_id
                    AND held.id<>intent.id AND held.lease_owner IS NOT NULL
                    AND held.lease_until>NEW.created_at
              )
        ) THEN
            RAISE EXCEPTION 'Helm Application adoption receipt does not match exact recoverable intent'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_publisher_adoption_receipts_validate
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_protected_publisher_adoption_receipts
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_publisher_adoption_receipt();

CREATE FUNCTION public.validate_helm_protected_publisher_authority()
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
            RAISE EXCEPTION 'Helm protected publisher authority must start at its immutable origin'
                USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.original_publisher_config_digest IS DISTINCT FROM OLD.original_publisher_config_digest THEN
        RAISE EXCEPTION 'Helm protected original publisher authority is immutable'
            USING ERRCODE='23514';
    END IF;
    IF NEW.lease_owner IS NOT NULL AND NEW.lease_until>NEW.updated_at THEN
        IF OLD.lease_epoch=0 AND NEW.lease_epoch=1 AND
           NOT public.helm_protected_adoption_projection_is_fresh(
               NEW.platform_binding_id,NEW.environment_binding_id,NEW.cluster_id,
               NEW.project_id,NEW.environment_id,NEW.platform_target_ref,
               NEW.environment_target_ref,NEW.environment_revision,
               NEW.environment_generation
           ) THEN
            RAISE EXCEPTION 'Helm protected authority claim has a stale projection'
                USING ERRCODE='23514';
        END IF;
        IF TG_TABLE_NAME='helm_protected_payload_intents' THEN
            IF EXISTS(
                SELECT 1 FROM public.helm_protected_payload_intents held
                WHERE held.platform_binding_id=NEW.platform_binding_id
                  AND held.id<>NEW.id AND held.lease_owner IS NOT NULL
                  AND held.lease_until>NEW.updated_at
            ) OR EXISTS(
                SELECT 1 FROM public.helm_protected_application_intents held
                WHERE held.platform_binding_id=NEW.platform_binding_id
                  AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.updated_at
            ) THEN
                RAISE EXCEPTION 'Helm protected payload authority lane is already leased'
                    USING ERRCODE='23514';
            END IF;
        ELSE
            IF EXISTS(
                SELECT 1 FROM public.helm_protected_payload_intents held
                WHERE held.platform_binding_id=NEW.platform_binding_id
                  AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.updated_at
            ) OR EXISTS(
                SELECT 1 FROM public.helm_protected_application_intents held
                WHERE held.platform_binding_id=NEW.platform_binding_id
                  AND held.id<>NEW.id AND held.lease_owner IS NOT NULL
                  AND held.lease_until>NEW.updated_at
            ) THEN
                RAISE EXCEPTION 'Helm protected Application authority lane is already leased'
                    USING ERRCODE='23514';
            END IF;
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
        RAISE EXCEPTION 'Helm protected publisher adoption transition is not an exact atomic claim'
            USING ERRCODE='23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.runtime_readiness readiness
        WHERE readiness.runtime_kind='helm-protected-publisher'
          AND readiness.scope_key='global'
          AND readiness.worker_id=NEW.lease_owner
          AND readiness.contract_version=NEW.publisher_contract
          AND readiness.identity=jsonb_build_object('policyVersion','helm-protected-git.v1')
          AND readiness.config_digest=NEW.publisher_config_digest
          AND readiness.updated_at=readiness.observed_at
          AND readiness.observed_at<=NEW.updated_at
          AND readiness.observed_at>=NEW.updated_at-interval '5 minutes'
          AND readiness.lease_until>NEW.updated_at
          AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
          AND readiness.lease_until<=NEW.updated_at+interval '5 minutes'
    ) THEN
        RAISE EXCEPTION 'Helm protected publisher adoption claim lacks active exact readiness'
            USING ERRCODE='23514';
    END IF;
    IF TG_TABLE_NAME='helm_protected_payload_intents' THEN
        SELECT EXISTS(
            SELECT 1 FROM public.helm_protected_publisher_adoption_receipts receipt
            WHERE receipt.intent_kind='payload' AND receipt.payload_intent_id=NEW.id
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
    ELSE
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
    END IF;
    IF NOT exact_receipt THEN
        RAISE EXCEPTION 'Helm protected publisher adoption lacks its exact immutable receipt'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER helm_protected_payload_publisher_authority
BEFORE INSERT OR UPDATE ON public.helm_protected_payload_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_publisher_authority();

CREATE TRIGGER helm_protected_application_publisher_authority
BEFORE INSERT OR UPDATE ON public.helm_protected_application_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_publisher_authority();

CREATE FUNCTION public.verify_helm_protected_publisher_adoption_postimage()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    valid_postimage boolean;
BEGIN
    IF NEW.intent_kind='payload' THEN
        SELECT EXISTS(
            SELECT 1 FROM public.helm_protected_payload_intents intent
            WHERE intent.id=NEW.payload_intent_id
              AND intent.publisher_config_digest=NEW.adopted_config_digest
              AND intent.original_publisher_config_digest=NEW.original_config_digest
              AND intent.publisher_adoption_epoch=NEW.adoption_epoch
              AND intent.lease_owner=NEW.adopted_by_worker
              AND intent.lease_epoch=NEW.adopted_lease_epoch
              AND intent.lease_until>NEW.created_at
              AND intent.updated_at=NEW.created_at
              AND intent.prerequisite_epoch=NEW.prerequisite_epoch+1
              AND intent.state=CASE WHEN NEW.recovery_state='pending' THEN 'claimed' ELSE NEW.recovery_state END
              AND intent.intent_digest=NEW.intent_digest
              AND intent.content_digest=NEW.content_digest
              AND intent.path=NEW.protected_path
              AND intent.precondition=NEW.precondition
              AND intent.expected_etag=NEW.expected_etag
              AND intent.commit_trailer=NEW.commit_trailer
              AND intent.write_base_revision=NEW.write_base_revision
              AND intent.committed_revision=NEW.committed_revision
              AND intent.committed_parent_revision=NEW.committed_parent_revision
        ) INTO valid_postimage;
    ELSE
        SELECT EXISTS(
            SELECT 1 FROM public.helm_protected_application_intents intent
            WHERE intent.id=NEW.application_intent_id
              AND intent.publisher_config_digest=NEW.adopted_config_digest
              AND intent.original_publisher_config_digest=NEW.original_config_digest
              AND intent.publisher_adoption_epoch=NEW.adoption_epoch
              AND intent.lease_owner=NEW.adopted_by_worker
              AND intent.lease_epoch=NEW.adopted_lease_epoch
              AND intent.lease_until>NEW.created_at
              AND intent.updated_at=NEW.created_at
              AND intent.prerequisite_epoch=NEW.prerequisite_epoch+1
              AND intent.state=CASE WHEN NEW.recovery_state='pending' THEN 'claimed' ELSE NEW.recovery_state END
              AND intent.intent_digest=NEW.intent_digest
              AND intent.content_digest=NEW.content_digest
              AND intent.application_path=NEW.protected_path
              AND intent.precondition=NEW.precondition
              AND intent.expected_etag=NEW.expected_etag
              AND intent.commit_trailer=NEW.commit_trailer
              AND intent.write_base_revision=NEW.write_base_revision
              AND intent.committed_revision=NEW.committed_revision
              AND intent.committed_parent_revision=NEW.committed_parent_revision
        ) INTO valid_postimage;
    END IF;
    IF NOT valid_postimage THEN
        RAISE EXCEPTION 'Helm protected publisher adoption receipt lacks exact committed postimage'
            USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER helm_protected_publisher_adoption_postimage
AFTER INSERT ON public.helm_protected_publisher_adoption_receipts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION public.verify_helm_protected_publisher_adoption_postimage();

CREATE FUNCTION public.helm_protected_adoption_projection_is_fresh(
    candidate_platform_binding_id uuid,
    candidate_environment_binding_id uuid,
    candidate_cluster_id uuid,
    candidate_project_id uuid,
    candidate_environment_id uuid,
    candidate_platform_target_ref text,
    candidate_environment_target_ref text,
    candidate_environment_revision text,
    candidate_environment_generation bigint
) RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $$
    SELECT EXISTS(
        SELECT 1 FROM public.git_repository_bindings platform
        JOIN public.git_repository_bindings environment
          ON environment.id=candidate_environment_binding_id
        JOIN public.git_projection_generations generation
          ON generation.binding_id=environment.id
         AND generation.generation=candidate_environment_generation
        WHERE platform.id=candidate_platform_binding_id
          AND platform.kind='platform'
          AND platform.credential_mode='github-app'
          AND platform.cluster_id=candidate_cluster_id
          AND platform.target_ref=candidate_platform_target_ref
          AND platform.target_head_revision IS NOT NULL
          AND platform.state IN ('ready','indexing')
          AND environment.kind='environment'
          AND environment.project_id=candidate_project_id
          AND environment.environment_id=candidate_environment_id
          AND environment.target_ref=candidate_environment_target_ref
          AND environment.target_head_revision=candidate_environment_revision
          AND environment.indexed_revision=candidate_environment_revision
          AND environment.projection_generation=candidate_environment_generation
          AND environment.state='ready'
          AND generation.head_revision=candidate_environment_revision
          AND generation.state='active'
          AND NOT EXISTS(
              SELECT 1 FROM public.git_projected_documents invalid
              WHERE invalid.binding_id=environment.id
                AND invalid.generation=candidate_environment_generation
                AND NOT invalid.valid
          )
    )
$$;

CREATE FUNCTION public.adopt_helm_protected_payload_intent(
    receipt_id uuid,
    adopting_worker text,
    adopting_worker_epoch bigint,
    adopting_publisher_contract text,
    adopting_policy_version text,
    adopting_config_digest text,
    lease_milliseconds bigint
) RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    db_now timestamptz := clock_timestamp();
    candidate public.helm_protected_payload_intents%ROWTYPE;
    affected bigint;
BEGIN
    IF receipt_id IS NULL OR
       adopting_worker !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$' OR
       adopting_worker_epoch<1 OR
       adopting_publisher_contract<>'helm-protected-publisher.v1' OR
       adopting_policy_version<>'helm-protected-git.v1' OR
       adopting_config_digest !~ '^sha256:[0-9a-f]{64}$' OR
       lease_milliseconds<15000 OR lease_milliseconds>300000 THEN
        RAISE EXCEPTION 'invalid protected payload publisher adoption request'
            USING ERRCODE='23514';
    END IF;

    SELECT intent.* INTO candidate
    FROM public.helm_protected_payload_intents intent
    WHERE intent.state IN ('pending','claimed','git-committed')
      AND intent.next_attempt_at<=db_now
      AND intent.updated_at<=db_now
      AND (intent.lease_owner IS NULL OR intent.lease_until<=db_now)
      AND intent.publisher_contract=adopting_publisher_contract
      AND intent.publisher_config_digest<>adopting_config_digest
      AND intent.prerequisite_receipt_id=intent.release_revision_id
      AND intent.prerequisite_contract='helm-publication-prerequisite.v1'
      AND (intent.lease_epoch>0 OR public.helm_protected_adoption_projection_is_fresh(
          intent.platform_binding_id,intent.environment_binding_id,intent.cluster_id,
          intent.project_id,intent.environment_id,intent.platform_target_ref,
          intent.environment_target_ref,intent.environment_revision,
          intent.environment_generation
      ))
      AND EXISTS(
          SELECT 1 FROM public.runtime_readiness readiness
          WHERE readiness.runtime_kind='helm-protected-publisher'
            AND readiness.scope_key='global'
            AND readiness.worker_id=adopting_worker
            AND readiness.worker_epoch=adopting_worker_epoch
            AND readiness.contract_version=adopting_publisher_contract
            AND readiness.identity=jsonb_build_object('policyVersion',adopting_policy_version)
            AND readiness.config_digest=adopting_config_digest
            AND readiness.updated_at=readiness.observed_at
            AND readiness.observed_at<=db_now
            AND readiness.observed_at>=db_now-interval '5 minutes'
            AND readiness.lease_until>db_now
            AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
            AND readiness.lease_until<=db_now+interval '5 minutes'
      )
      AND NOT EXISTS(
          SELECT 1 FROM public.helm_protected_payload_intents held
          WHERE held.platform_binding_id=intent.platform_binding_id
            AND held.id<>intent.id
            AND held.lease_owner IS NOT NULL AND held.lease_until>db_now
      )
      AND NOT EXISTS(
          SELECT 1 FROM public.helm_protected_application_intents held
          WHERE held.platform_binding_id=intent.platform_binding_id
            AND held.lease_owner IS NOT NULL AND held.lease_until>db_now
      )
    ORDER BY intent.next_attempt_at,intent.created_at,intent.id
    FOR UPDATE OF intent SKIP LOCKED LIMIT 1;

    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    INSERT INTO public.helm_protected_publisher_adoption_receipts(
        id,intent_kind,payload_intent_id,application_intent_id,adoption_epoch,
        publisher_contract,original_config_digest,previous_config_digest,
        adopted_config_digest,policy_version,intent_digest,content_digest,
        protected_path,precondition,expected_etag,commit_trailer,
        prerequisite_receipt_id,prerequisite_contract,prerequisite_epoch,
        recovery_state,write_base_revision,committed_revision,
        committed_parent_revision,previous_lease_epoch,adopted_lease_epoch,
        adopted_by_worker,adopted_worker_epoch,created_at
    ) VALUES(
        receipt_id,'payload',candidate.id,NULL,candidate.publisher_adoption_epoch+1,
        candidate.publisher_contract,candidate.original_publisher_config_digest,
        candidate.publisher_config_digest,adopting_config_digest,adopting_policy_version,
        candidate.intent_digest,candidate.content_digest,candidate.path,
        candidate.precondition,candidate.expected_etag,candidate.commit_trailer,
        candidate.prerequisite_receipt_id,candidate.prerequisite_contract,
        candidate.prerequisite_epoch,candidate.state,candidate.write_base_revision,
        candidate.committed_revision,candidate.committed_parent_revision,
        candidate.lease_epoch,candidate.lease_epoch+1,adopting_worker,
        adopting_worker_epoch,db_now
    );

    UPDATE public.helm_protected_payload_intents intent SET
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
        RAISE EXCEPTION 'protected payload publisher adoption lost its exact lock'
            USING ERRCODE='40001';
    END IF;
    RETURN candidate.id;
END;
$$;

CREATE FUNCTION public.adopt_helm_protected_application_intent(
    receipt_id uuid,
    adopting_worker text,
    adopting_worker_epoch bigint,
    adopting_publisher_contract text,
    adopting_policy_version text,
    adopting_config_digest text,
    lease_milliseconds bigint
) RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path=pg_catalog,pg_temp
AS $$
DECLARE
    db_now timestamptz := clock_timestamp();
    candidate public.helm_protected_application_intents%ROWTYPE;
    affected bigint;
BEGIN
    IF receipt_id IS NULL OR
       adopting_worker !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$' OR
       adopting_worker_epoch<1 OR
       adopting_publisher_contract<>'helm-protected-publisher.v1' OR
       adopting_policy_version<>'helm-protected-git.v1' OR
       adopting_config_digest !~ '^sha256:[0-9a-f]{64}$' OR
       lease_milliseconds<15000 OR lease_milliseconds>300000 THEN
        RAISE EXCEPTION 'invalid protected Application publisher adoption request'
            USING ERRCODE='23514';
    END IF;

    SELECT intent.* INTO candidate
    FROM public.helm_protected_application_intents intent
    WHERE intent.state IN ('pending','claimed','git-committed')
      AND intent.next_attempt_at<=db_now
      AND intent.updated_at<=db_now
      AND (intent.lease_owner IS NULL OR intent.lease_until<=db_now)
      AND intent.publisher_contract=adopting_publisher_contract
      AND intent.publisher_config_digest<>adopting_config_digest
      AND intent.prerequisite_receipt_id=intent.release_revision_id
      AND intent.prerequisite_contract='helm-publication-prerequisite.v1'
      AND (intent.lease_epoch>0 OR public.helm_protected_adoption_projection_is_fresh(
          intent.platform_binding_id,intent.environment_binding_id,intent.cluster_id,
          intent.project_id,intent.environment_id,intent.platform_target_ref,
          intent.environment_target_ref,intent.environment_revision,
          intent.environment_generation
      ))
      AND EXISTS(
          SELECT 1 FROM public.runtime_readiness readiness
          WHERE readiness.runtime_kind='helm-protected-publisher'
            AND readiness.scope_key='global'
            AND readiness.worker_id=adopting_worker
            AND readiness.worker_epoch=adopting_worker_epoch
            AND readiness.contract_version=adopting_publisher_contract
            AND readiness.identity=jsonb_build_object('policyVersion',adopting_policy_version)
            AND readiness.config_digest=adopting_config_digest
            AND readiness.updated_at=readiness.observed_at
            AND readiness.observed_at<=db_now
            AND readiness.observed_at>=db_now-interval '5 minutes'
            AND readiness.lease_until>db_now
            AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
            AND readiness.lease_until<=db_now+interval '5 minutes'
      )
      AND NOT EXISTS(
          SELECT 1 FROM public.helm_protected_payload_intents held
          WHERE held.platform_binding_id=intent.platform_binding_id
            AND held.lease_owner IS NOT NULL AND held.lease_until>db_now
      )
      AND NOT EXISTS(
          SELECT 1 FROM public.helm_protected_application_intents held
          WHERE held.platform_binding_id=intent.platform_binding_id
            AND held.id<>intent.id
            AND held.lease_owner IS NOT NULL AND held.lease_until>db_now
      )
    ORDER BY intent.next_attempt_at,intent.created_at,intent.id
    FOR UPDATE OF intent SKIP LOCKED LIMIT 1;

    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    INSERT INTO public.helm_protected_publisher_adoption_receipts(
        id,intent_kind,payload_intent_id,application_intent_id,adoption_epoch,
        publisher_contract,original_config_digest,previous_config_digest,
        adopted_config_digest,policy_version,intent_digest,content_digest,
        protected_path,precondition,expected_etag,commit_trailer,
        prerequisite_receipt_id,prerequisite_contract,prerequisite_epoch,
        recovery_state,write_base_revision,committed_revision,
        committed_parent_revision,previous_lease_epoch,adopted_lease_epoch,
        adopted_by_worker,adopted_worker_epoch,created_at
    ) VALUES(
        receipt_id,'application',NULL,candidate.id,candidate.publisher_adoption_epoch+1,
        candidate.publisher_contract,candidate.original_publisher_config_digest,
        candidate.publisher_config_digest,adopting_config_digest,adopting_policy_version,
        candidate.intent_digest,candidate.content_digest,candidate.application_path,
        candidate.precondition,candidate.expected_etag,candidate.commit_trailer,
        candidate.prerequisite_receipt_id,candidate.prerequisite_contract,
        candidate.prerequisite_epoch,candidate.state,candidate.write_base_revision,
        candidate.committed_revision,candidate.committed_parent_revision,
        candidate.lease_epoch,candidate.lease_epoch+1,adopting_worker,
        adopting_worker_epoch,db_now
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
        RAISE EXCEPTION 'protected Application publisher adoption lost its exact lock'
            USING ERRCODE='40001';
    END IF;
    RETURN candidate.id;
END;
$$;

-- These revokes reduce accidental call/DML surface for non-owners. They are
-- not the authority boundary when migration and runtime share an owner role;
-- the symmetric readiness, projection, lane, receipt, and postimage triggers
-- above enforce the contract for direct DML as well as function calls.
REVOKE ALL ON FUNCTION public.helm_protected_adoption_projection_is_fresh(
    uuid,uuid,uuid,uuid,uuid,text,text,text,bigint
) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.adopt_helm_protected_payload_intent(
    uuid,text,bigint,text,text,text,bigint
) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.adopt_helm_protected_application_intent(
    uuid,text,bigint,text,text,text,bigint
) FROM PUBLIC;
REVOKE INSERT,UPDATE,DELETE ON public.helm_protected_publisher_adoption_receipts FROM PUBLIC;
