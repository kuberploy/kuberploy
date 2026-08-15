-- Protected Helm Application deletion needs a foreground Argo finalizer on the
-- exact live child before Git removes its Application manifest.  This migration
-- adds a durable Git-only preflight, a separately leased read-only observation,
-- and append-only proof epochs.  Kubernetes and Argo remain observation-only.

LOCK TABLE public.helm_protected_application_intents IN ACCESS EXCLUSIVE MODE;
LOCK TABLE public.helm_protected_payload_intents IN SHARE ROW EXCLUSIVE MODE;

-- An old worker may already have produced a side effect for non-pristine work.
-- Refuse the upgrade in that case.  Pristine legacy deletes are safe to retire;
-- the new planner will create a cascade preflight and a new live delete intent.
DO $migration$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM public.helm_protected_application_intents AS intent
        WHERE intent.action='delete'
          AND intent.state IN ('pending','claimed','git-committed')
          AND NOT (
              intent.state='pending' AND intent.attempts=0 AND intent.lease_epoch=0
              AND intent.lease_owner IS NULL AND intent.lease_until IS NULL
              AND intent.write_base_revision='' AND intent.write_base_observed_at IS NULL
              AND intent.committed_revision='' AND intent.committed_parent_revision=''
              AND intent.committed_at IS NULL AND intent.verified_at IS NULL
              AND intent.verified_path_digest='' AND intent.provider_request=''
              AND intent.completed_at IS NULL
          )
    ) THEN
        RAISE EXCEPTION 'cannot install cascade preflight while a legacy delete may have Git side effects';
    END IF;

    UPDATE public.helm_protected_application_intents AS intent
       SET state='superseded',consecutive_failures=1,
           last_failure_code='cascade-migration-replan-required',
           completed_at=pg_catalog.clock_timestamp(),
           updated_at=pg_catalog.clock_timestamp(),
           prerequisite_epoch=intent.prerequisite_epoch+1
     WHERE intent.action='delete' AND intent.state='pending'
       AND intent.attempts=0 AND intent.lease_epoch=0
       AND intent.lease_owner IS NULL AND intent.lease_until IS NULL
       AND intent.write_base_revision='' AND intent.write_base_observed_at IS NULL
       AND intent.committed_revision='' AND intent.committed_parent_revision=''
       AND intent.committed_at IS NULL AND intent.verified_at IS NULL
       AND intent.verified_path_digest='' AND intent.provider_request=''
       AND intent.completed_at IS NULL;
END;
$migration$;

ALTER TABLE public.helm_protected_application_intents
    ADD COLUMN cascade_required boolean NOT NULL DEFAULT false,
    ADD COLUMN cascade_receipt_id uuid,
    ADD COLUMN cascade_contract text NOT NULL DEFAULT '';

-- Existing rows predate cascade authority.  Terminal history stays readable.
-- Old publish writers remain compatible. Old delete writers receive false and
-- are rejected by the cascade gate because every delete requires proof.
UPDATE public.helm_protected_application_intents
   SET cascade_required=false,cascade_receipt_id=NULL,cascade_contract='',
       prerequisite_epoch=prerequisite_epoch+CASE
           WHEN state IN ('pending','claimed','git-committed') THEN 1 ELSE 0 END;

-- Migration 009 permits a replacement continuation only after the latest
-- Application intent was superseded before acquiring a lease or producing a
-- Git effect.  A pristine legacy delete retired by this migration has the same
-- authority shape, with the additional requirement that it has no cascade
-- receipt.  Extend that exact predecessor whitelist without weakening it for
-- any other terminal reason.
DO $migration$
DECLARE
    definition text;
    needle text := $needle$prior.state='superseded' AND prior.last_failure_code='projection-superseded'
          AND prior.lease_epoch=0$needle$;
    replacement text := $replacement$prior.state='superseded' AND (
            prior.last_failure_code='projection-superseded' OR
            (prior.last_failure_code='cascade-migration-replan-required'
             AND NOT prior.cascade_required AND prior.cascade_receipt_id IS NULL
             AND prior.cascade_contract='')
          )
          AND prior.lease_epoch=0$replacement$;
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_application_continuation_receipt()'::pg_catalog.regprocedure);
    IF pg_catalog.strpos(definition,needle)=0 OR
       pg_catalog.strpos(pg_catalog.substr(
           definition,pg_catalog.strpos(definition,needle)+1),needle)>0 THEN
        RAISE EXCEPTION 'unexpected continuation replacement validator shape before cascade bridge';
    END IF;
    definition := pg_catalog.replace(definition,needle,replacement);
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.validate_helm_application_continuation_receipt()
SET search_path=pg_catalog,pg_temp;

ALTER TABLE public.helm_protected_application_intents
    ADD CONSTRAINT helm_protected_application_cascade_shape CHECK (
        (NOT cascade_required AND cascade_receipt_id IS NULL AND cascade_contract='') OR
        (cascade_required AND action='delete' AND cascade_receipt_id IS NOT NULL AND
         cascade_contract='helm-application-cascade-preflight.v1')
    );

CREATE TABLE public.helm_application_cascade_preflights (
    id uuid PRIMARY KEY,
    delete_intent_id uuid NOT NULL,
    release_revision_id uuid NOT NULL,
    payload_intent_id uuid NOT NULL,
    base_application_intent_id uuid NOT NULL,
    release_generation bigint NOT NULL CHECK (release_generation>1),
    payload_revision text NOT NULL CHECK (payload_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    platform_binding_id uuid NOT NULL,
    environment_binding_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    platform_target_ref text NOT NULL CHECK (
        length(platform_target_ref) BETWEEN 1 AND 255 AND platform_target_ref !~ '[[:cntrl:]]'),
    environment_target_ref text NOT NULL CHECK (
        length(environment_target_ref) BETWEEN 1 AND 255 AND environment_target_ref !~ '[[:cntrl:]]'),
    environment_revision text NOT NULL CHECK (environment_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    environment_generation bigint NOT NULL CHECK (environment_generation>0),
    catalog_digest text NOT NULL CHECK (catalog_digest ~ '^sha256:[0-9a-f]{64}$'),
    planned_base_revision text NOT NULL CHECK (planned_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    argo_namespace text NOT NULL CHECK (argo_namespace ~ '^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$'),
    application_path text NOT NULL CHECK (
        length(application_path) BETWEEN 1 AND 1024 AND
        application_path !~ '(^/|/\.\.?(/|$)|//|\\|[[:cntrl:]])'),
    source_content bytea NOT NULL CHECK (octet_length(source_content) BETWEEN 1 AND 32768),
    source_content_digest text NOT NULL CHECK (source_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    adopted_content bytea NOT NULL CHECK (octet_length(adopted_content) BETWEEN 1 AND 32768),
    adopted_content_digest text NOT NULL CHECK (adopted_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    content_digest text NOT NULL CHECK (
        content_digest=adopted_content_digest AND content_digest ~ '^sha256:[0-9a-f]{64}$'),
    operation text NOT NULL CHECK (operation IN ('observe','update')),
    precondition text NOT NULL CHECK (precondition='match-etag'),
    expected_etag text NOT NULL CHECK (expected_etag ~ '^"sha256:[0-9a-f]{64}"$'),
    intent_digest text NOT NULL CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    commit_trailer text NOT NULL,
    contract text NOT NULL CHECK (contract='helm-application-cascade-preflight.v1'),
    publisher_contract text NOT NULL CHECK (publisher_contract='helm-protected-publisher.v1'),
    publisher_policy_version text NOT NULL CHECK (publisher_policy_version='helm-protected-git.v1'),
    publisher_config_digest text NOT NULL CHECK (publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    original_publisher_config_digest text NOT NULL CHECK (original_publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_adoption_epoch bigint NOT NULL DEFAULT 0 CHECK (publisher_adoption_epoch>=0),
    prerequisite_epoch bigint NOT NULL DEFAULT 0 CHECK (prerequisite_epoch>=0),
    state text NOT NULL CHECK (state IN ('pending','claimed','git-committed','verified','failed','superseded')),
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    write_base_revision text NOT NULL DEFAULT '' CHECK (
        write_base_revision='' OR write_base_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    write_base_observed_at timestamptz,
    committed_revision text NOT NULL DEFAULT '' CHECK (
        committed_revision='' OR committed_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    committed_parent_revision text NOT NULL DEFAULT '' CHECK (
        committed_parent_revision='' OR committed_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    committed_at timestamptz,
    verified_at timestamptz,
    verified_path_digest text NOT NULL DEFAULT '' CHECK (
        verified_path_digest='' OR verified_path_digest ~ '^sha256:[0-9a-f]{64}$'),
    provider_request text NOT NULL DEFAULT '' CHECK (
        length(provider_request)<=256 AND provider_request !~ '[[:cntrl:]]'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    FOREIGN KEY (release_revision_id) REFERENCES public.helm_release_revisions(id) ON DELETE RESTRICT,
    FOREIGN KEY (payload_intent_id) REFERENCES public.helm_protected_payload_intents(id) ON DELETE RESTRICT,
    FOREIGN KEY (base_application_intent_id) REFERENCES public.helm_protected_application_intents(id) ON DELETE RESTRICT,
    FOREIGN KEY (platform_binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT,
    FOREIGN KEY (environment_binding_id) REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND next_attempt_at>=created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK ((lease_owner IS NULL)=(lease_until IS NULL)),
    CHECK ((write_base_revision='')=(write_base_observed_at IS NULL)),
    CHECK ((committed_revision='')=(committed_parent_revision='' AND committed_at IS NULL))
);

CREATE UNIQUE INDEX helm_application_cascade_preflight_delete_live_key
ON public.helm_application_cascade_preflights(delete_intent_id)
WHERE state<>'superseded';
CREATE UNIQUE INDEX helm_application_cascade_preflight_release_live_key
ON public.helm_application_cascade_preflights(release_revision_id)
WHERE state<>'superseded';
CREATE UNIQUE INDEX helm_application_cascade_preflight_payload_live_key
ON public.helm_application_cascade_preflights(payload_intent_id)
WHERE state<>'superseded';
CREATE UNIQUE INDEX helm_application_cascade_preflight_generation_live_key
ON public.helm_application_cascade_preflights(environment_id,application_id,release_generation)
WHERE state<>'superseded';
CREATE INDEX helm_application_cascade_preflights_due_idx
ON public.helm_application_cascade_preflights(next_attempt_at,created_at,id)
WHERE state IN ('pending','claimed','git-committed');
CREATE INDEX helm_application_cascade_preflights_verified_idx
ON public.helm_application_cascade_preflights(verified_at,id)
WHERE state='verified';

CREATE TABLE public.helm_application_cascade_adoption_receipts (
    id uuid PRIMARY KEY,
    cascade_preflight_id uuid NOT NULL REFERENCES public.helm_application_cascade_preflights(id) ON DELETE RESTRICT,
    adoption_epoch bigint NOT NULL CHECK (adoption_epoch>0),
    publisher_contract text NOT NULL CHECK (publisher_contract='helm-protected-publisher.v1'),
    policy_version text NOT NULL CHECK (policy_version='helm-protected-git.v1'),
    original_config_digest text NOT NULL CHECK (original_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    previous_config_digest text NOT NULL CHECK (previous_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    adopted_config_digest text NOT NULL CHECK (adopted_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    intent_digest text NOT NULL CHECK (intent_digest ~ '^sha256:[0-9a-f]{64}$'),
    source_content_digest text NOT NULL CHECK (source_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    adopted_content_digest text NOT NULL CHECK (adopted_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    application_path text NOT NULL,
    precondition text NOT NULL CHECK (precondition='match-etag'),
    expected_etag text NOT NULL,
    commit_trailer text NOT NULL,
    recovery_state text NOT NULL CHECK (recovery_state IN ('pending','claimed','git-committed')),
    write_base_revision text NOT NULL,
    committed_revision text NOT NULL,
    committed_parent_revision text NOT NULL,
    previous_lease_epoch bigint NOT NULL CHECK (previous_lease_epoch>=0),
    adopted_lease_epoch bigint NOT NULL CHECK (adopted_lease_epoch=previous_lease_epoch+1),
    adopted_by_worker text NOT NULL,
    adopted_worker_epoch bigint NOT NULL CHECK (adopted_worker_epoch>0),
    created_at timestamptz NOT NULL,
    UNIQUE (cascade_preflight_id,adoption_epoch),
    CHECK (previous_config_digest<>adopted_config_digest)
);

CREATE TABLE public.helm_application_cascade_observer_activations (
    platform_binding_id uuid NOT NULL REFERENCES public.git_repository_bindings(id) ON DELETE RESTRICT,
    activation_epoch bigint NOT NULL CHECK (activation_epoch>0),
    publisher_contract text NOT NULL CHECK (publisher_contract='helm-protected-publisher.v1'),
    publisher_policy_version text NOT NULL CHECK (publisher_policy_version='helm-protected-git.v1'),
    publisher_config_digest text NOT NULL CHECK (publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_worker_id text NOT NULL CHECK (
        length(publisher_worker_id) BETWEEN 16 AND 128 AND
        publisher_worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'),
    publisher_worker_epoch bigint NOT NULL CHECK (publisher_worker_epoch>0),
    publisher_started_at timestamptz NOT NULL,
    publisher_readiness_observed_at timestamptz NOT NULL,
    publisher_readiness_lease_until timestamptz NOT NULL,
    argo_contract text NOT NULL CHECK (argo_contract='argo-desired-state-runtime-v1'),
    argo_config_digest text NOT NULL CHECK (argo_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    argo_worker_id text NOT NULL CHECK (
        length(argo_worker_id) BETWEEN 1 AND 256 AND
        argo_worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    argo_worker_epoch bigint NOT NULL CHECK (argo_worker_epoch>0),
    argo_identity jsonb NOT NULL CHECK (
        pg_catalog.jsonb_typeof(argo_identity)='object' AND octet_length(argo_identity::text)<=8192),
    argo_started_at timestamptz NOT NULL,
    argo_readiness_observed_at timestamptz NOT NULL,
    argo_readiness_lease_until timestamptz NOT NULL,
    activated_at timestamptz NOT NULL,
    PRIMARY KEY (platform_binding_id,activation_epoch),
    CONSTRAINT helm_application_cascade_observer_process_key UNIQUE (
        platform_binding_id,publisher_worker_id,publisher_worker_epoch,
        argo_worker_id,argo_worker_epoch),
    CHECK (publisher_started_at<=publisher_readiness_observed_at AND
           publisher_readiness_observed_at<=activated_at AND
           publisher_readiness_lease_until>activated_at AND
           publisher_readiness_lease_until<=publisher_readiness_observed_at+interval '5 minutes' AND
           publisher_readiness_lease_until<=activated_at+interval '5 minutes'),
    CHECK (argo_started_at<=argo_readiness_observed_at AND
           argo_readiness_observed_at<=activated_at AND
           argo_readiness_lease_until>activated_at AND
           argo_readiness_lease_until<=argo_readiness_observed_at+interval '5 minutes' AND
           argo_readiness_lease_until<=activated_at+interval '5 minutes')
);

CREATE TABLE public.helm_application_cascade_observation_jobs (
    cascade_preflight_id uuid NOT NULL REFERENCES public.helm_application_cascade_preflights(id) ON DELETE RESTRICT,
    platform_binding_id uuid NOT NULL,
    activation_epoch bigint NOT NULL CHECK (activation_epoch>0),
    publisher_config_digest text NOT NULL CHECK (publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_contract text NOT NULL CHECK (publisher_contract='helm-protected-publisher.v1'),
    publisher_policy_version text NOT NULL CHECK (publisher_policy_version='helm-protected-git.v1'),
    state text NOT NULL CHECK (state IN ('pending','claimed','verified','failed','superseded')),
    next_attempt_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 30),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures BETWEEN 0 AND 30),
    last_failure_code text NOT NULL DEFAULT '' CHECK (
        last_failure_code='' OR last_failure_code ~ '^[a-z][a-z0-9.-]{0,62}$'),
    lease_owner text CHECK (
        lease_owner IS NULL OR
        (length(lease_owner) BETWEEN 16 AND 128 AND lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$')),
    worker_epoch bigint CHECK (worker_epoch IS NULL OR worker_epoch>0),
    lease_epoch bigint NOT NULL DEFAULT 0 CHECK (lease_epoch>=0),
    lease_until timestamptz,
    superseded_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    PRIMARY KEY (cascade_preflight_id,activation_epoch),
    FOREIGN KEY (platform_binding_id,activation_epoch)
        REFERENCES public.helm_application_cascade_observer_activations(platform_binding_id,activation_epoch)
        ON DELETE RESTRICT,
    CHECK (updated_at>=created_at AND next_attempt_at>=created_at),
    CHECK ((last_failure_code='')=(consecutive_failures=0)),
    CHECK ((lease_owner IS NULL)=(lease_until IS NULL)),
    CHECK ((state='superseded')=(superseded_at IS NOT NULL))
);
CREATE UNIQUE INDEX helm_application_cascade_observer_active_key
ON public.helm_application_cascade_observation_jobs(cascade_preflight_id)
WHERE state<>'superseded';
CREATE INDEX helm_application_cascade_observation_jobs_due_idx
ON public.helm_application_cascade_observation_jobs(next_attempt_at,created_at,cascade_preflight_id)
WHERE state IN ('pending','claimed');

CREATE TABLE public.helm_application_cascade_receipts (
    id uuid PRIMARY KEY,
    delete_intent_id uuid NOT NULL,
    cascade_preflight_id uuid NOT NULL REFERENCES public.helm_application_cascade_preflights(id) ON DELETE RESTRICT,
    observation_epoch bigint NOT NULL CHECK (observation_epoch>0),
    observation_lease_epoch bigint NOT NULL CHECK (observation_lease_epoch>0),
    observer_activation_epoch bigint NOT NULL CHECK (observer_activation_epoch>0),
    release_revision_id uuid NOT NULL,
    payload_intent_id uuid NOT NULL,
    base_application_intent_id uuid NOT NULL,
    project_id uuid NOT NULL,
    environment_id uuid NOT NULL,
    application_id uuid NOT NULL,
    cluster_id uuid NOT NULL,
    application_path text NOT NULL,
    source_content_digest text NOT NULL CHECK (source_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    adopted_content_digest text NOT NULL CHECK (adopted_content_digest ~ '^sha256:[0-9a-f]{64}$'),
    adoption_revision text NOT NULL CHECK (adoption_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    adoption_parent_revision text NOT NULL CHECK (adoption_parent_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    provider_head text NOT NULL CHECK (provider_head ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    root_observed_revision text NOT NULL,
    root_sync_status text NOT NULL CHECK (root_sync_status='Synced'),
    root_uid uuid NOT NULL,
    root_resource_version text NOT NULL CHECK (
        length(root_resource_version) BETWEEN 1 AND 128 AND root_resource_version !~ '[[:cntrl:]]'),
    root_spec_digest text NOT NULL CHECK (root_spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    child_uid uuid NOT NULL,
    child_resource_version text NOT NULL CHECK (
        length(child_resource_version) BETWEEN 1 AND 128 AND child_resource_version !~ '[[:cntrl:]]'),
    child_spec_digest text NOT NULL CHECK (child_spec_digest ~ '^sha256:[0-9a-f]{64}$'),
    finalizer_digest text NOT NULL CHECK (
        finalizer_digest='sha256:4a33b93a0b2d591421d38cedd7660abbfffcb3fc10be2cbbe9e4d8525ce17f48'),
    child_release_revision_id uuid NOT NULL,
    child_payload_revision text NOT NULL CHECK (child_payload_revision ~ '^(?:[0-9a-f]{40}|[0-9a-f]{64})$'),
    child_payload_path text NOT NULL,
    child_payload_digest text NOT NULL CHECK (child_payload_digest ~ '^sha256:[0-9a-f]{64}$'),
    publisher_contract text NOT NULL CHECK (publisher_contract='helm-protected-publisher.v1'),
    publisher_policy_version text NOT NULL CHECK (publisher_policy_version='helm-protected-git.v1'),
    publisher_config_digest text NOT NULL CHECK (publisher_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    worker_id text NOT NULL CHECK (
        length(worker_id) BETWEEN 16 AND 128 AND worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$'),
    worker_epoch bigint NOT NULL CHECK (worker_epoch>0),
    argo_contract text NOT NULL CHECK (argo_contract='argo-desired-state-runtime-v1'),
    argo_config_digest text NOT NULL CHECK (argo_config_digest ~ '^sha256:[0-9a-f]{64}$'),
    argo_worker_id text NOT NULL CHECK (
        length(argo_worker_id) BETWEEN 1 AND 256 AND
        argo_worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$'),
    argo_worker_epoch bigint NOT NULL CHECK (argo_worker_epoch>0),
    argo_started_at timestamptz NOT NULL,
    argo_readiness_observed_at timestamptz NOT NULL,
    argo_readiness_lease_until timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    UNIQUE (cascade_preflight_id,observation_epoch),
    CHECK (root_observed_revision=provider_head),
    CHECK (argo_started_at<=argo_readiness_observed_at AND
           argo_readiness_observed_at<=observed_at AND
           argo_readiness_lease_until>observed_at AND
           argo_readiness_lease_until<=argo_readiness_observed_at+interval '5 minutes')
);
CREATE INDEX helm_application_cascade_receipts_latest_idx
ON public.helm_application_cascade_receipts(cascade_preflight_id,observation_epoch DESC);

CREATE FUNCTION public.validate_helm_application_cascade_observer_activation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    publisher_readiness public.runtime_readiness%ROWTYPE;
    argo_readiness public.runtime_readiness%ROWTYPE;
    current_activation public.helm_application_cascade_observer_activations%ROWTYPE;
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'cascade observer activations are immutable' USING ERRCODE='23514';
    END IF;
    PERFORM 1 FROM public.git_repository_bindings AS binding
    WHERE binding.id=NEW.platform_binding_id AND binding.kind='platform'
      AND binding.credential_mode='github-app'
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade observer activation lacks protected platform binding'
            USING ERRCODE='23514';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(NEW.platform_binding_id::text,704215997));

    SELECT readiness.* INTO publisher_readiness
    FROM public.runtime_readiness AS readiness
    WHERE readiness.runtime_kind='helm-protected-publisher' AND readiness.scope_key='global'
      AND readiness.worker_id=NEW.publisher_worker_id
      AND readiness.worker_epoch=NEW.publisher_worker_epoch
      AND readiness.contract_version=NEW.publisher_contract
      AND readiness.config_digest=NEW.publisher_config_digest
      AND readiness.identity=pg_catalog.jsonb_build_object(
          'policyVersion',NEW.publisher_policy_version)
      AND readiness.updated_at=readiness.observed_at
      AND readiness.observed_at<=db_now AND readiness.observed_at>=db_now-interval '5 minutes'
      AND readiness.lease_until>db_now
      AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
      AND readiness.lease_until<=db_now+interval '5 minutes'
    FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade observer activation lacks current publisher readiness'
            USING ERRCODE='23514';
    END IF;

    SELECT readiness.* INTO argo_readiness
    FROM public.runtime_readiness AS readiness
    WHERE readiness.runtime_kind='argo-desired-state' AND readiness.scope_key='global'
      AND readiness.platform_binding_id=NEW.platform_binding_id
      AND readiness.worker_id=NEW.argo_worker_id
      AND readiness.worker_epoch=NEW.argo_worker_epoch
      AND readiness.contract_version=NEW.argo_contract
      AND readiness.config_digest=NEW.argo_config_digest
      AND readiness.updated_at=readiness.observed_at
      AND readiness.observed_at<=db_now AND readiness.observed_at>=db_now-interval '5 minutes'
      AND readiness.lease_until>db_now
      AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
      AND readiness.lease_until<=db_now+interval '5 minutes'
    FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade observer activation lacks current Argo readiness'
            USING ERRCODE='23514';
    END IF;
    IF EXISTS (
           SELECT 1 FROM public.helm_protected_payload_intents AS intent
           WHERE intent.platform_binding_id=NEW.platform_binding_id
             AND intent.lease_owner IS NOT NULL AND intent.lease_until>db_now
       ) OR EXISTS (
           SELECT 1 FROM public.helm_protected_application_intents AS intent
           WHERE intent.platform_binding_id=NEW.platform_binding_id
             AND intent.lease_owner IS NOT NULL AND intent.lease_until>db_now
       ) OR EXISTS (
           SELECT 1 FROM public.helm_application_cascade_preflights AS intent
           WHERE intent.platform_binding_id=NEW.platform_binding_id
             AND intent.lease_owner IS NOT NULL AND intent.lease_until>db_now
       ) THEN
        RAISE EXCEPTION 'cascade observer activation cannot replace a live protected Git lease'
            USING ERRCODE='23514';
    END IF;

    SELECT activation.* INTO current_activation
    FROM public.helm_application_cascade_observer_activations AS activation
    WHERE activation.platform_binding_id=NEW.platform_binding_id
    ORDER BY activation.activation_epoch DESC
    LIMIT 1
    FOR UPDATE;
    IF FOUND THEN
        IF publisher_readiness.started_at<=current_activation.publisher_started_at THEN
            RAISE EXCEPTION 'cascade observer activation publisher process did not advance'
                USING ERRCODE='23514';
        END IF;
        NEW.activation_epoch := current_activation.activation_epoch+1;
    ELSE
        -- Bootstrap is deterministic across every currently-live publisher.
        -- A delayed old process cannot win merely because it inserts first.
        IF EXISTS (
            SELECT 1 FROM public.runtime_readiness AS newer
            WHERE newer.runtime_kind='helm-protected-publisher' AND newer.scope_key='global'
              AND newer.contract_version='helm-protected-publisher.v1'
              AND newer.identity=pg_catalog.jsonb_build_object(
                  'policyVersion','helm-protected-git.v1')
              AND newer.updated_at=newer.observed_at
              AND newer.observed_at<=db_now AND newer.observed_at>=db_now-interval '5 minutes'
              AND newer.lease_until>db_now
              AND newer.lease_until<=newer.observed_at+interval '5 minutes'
              AND newer.lease_until<=db_now+interval '5 minutes'
              AND (newer.started_at,newer.worker_id)>
                  (publisher_readiness.started_at,publisher_readiness.worker_id)
        ) THEN
            RAISE EXCEPTION 'cascade observer activation is not newest live publisher bootstrap'
                USING ERRCODE='23514';
        END IF;
        NEW.activation_epoch := 1;
    END IF;

    NEW.publisher_started_at := publisher_readiness.started_at;
    NEW.publisher_readiness_observed_at := publisher_readiness.observed_at;
    NEW.publisher_readiness_lease_until := publisher_readiness.lease_until;
    NEW.argo_identity := argo_readiness.identity;
    NEW.argo_started_at := argo_readiness.started_at;
    NEW.argo_readiness_observed_at := argo_readiness.observed_at;
    NEW.argo_readiness_lease_until := argo_readiness.lease_until;
    NEW.activated_at := db_now;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_observer_activation_guard
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_application_cascade_observer_activations
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_observer_activation();

CREATE FUNCTION public.helm_application_cascade_active_observer_is_exact(
    binding_id uuid,publisher_worker text,publisher_epoch bigint,
    publisher_contract text,publisher_policy text,publisher_config text,
    argo_worker text,argo_epoch bigint,argo_contract text,argo_config text,
    authority_time timestamptz
)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT EXISTS (
    SELECT 1
    FROM public.helm_application_cascade_observer_activations AS activation
    JOIN public.runtime_readiness AS publisher
      ON publisher.runtime_kind='helm-protected-publisher' AND publisher.scope_key='global'
     AND publisher.worker_id=activation.publisher_worker_id
     AND publisher.worker_epoch=activation.publisher_worker_epoch
     AND publisher.contract_version=activation.publisher_contract
     AND publisher.config_digest=activation.publisher_config_digest
     AND publisher.identity=pg_catalog.jsonb_build_object(
         'policyVersion',activation.publisher_policy_version)
     AND publisher.started_at=activation.publisher_started_at
     AND publisher.updated_at=publisher.observed_at
     AND publisher.observed_at>=activation.publisher_readiness_observed_at
     AND publisher.lease_until>=activation.publisher_readiness_lease_until
     AND publisher.observed_at<=authority_time
     AND publisher.observed_at>=authority_time-interval '5 minutes'
     AND publisher.lease_until>authority_time
     AND publisher.lease_until<=publisher.observed_at+interval '5 minutes'
     AND publisher.lease_until<=authority_time+interval '5 minutes'
    JOIN public.runtime_readiness AS argo
      ON argo.runtime_kind='argo-desired-state' AND argo.scope_key='global'
     AND argo.platform_binding_id=activation.platform_binding_id
     AND argo.worker_id=activation.argo_worker_id
     AND argo.worker_epoch=activation.argo_worker_epoch
     AND argo.contract_version=activation.argo_contract
     AND argo.config_digest=activation.argo_config_digest
     AND argo.identity=activation.argo_identity
     AND argo.started_at=activation.argo_started_at
     AND argo.updated_at=argo.observed_at
     AND argo.observed_at>=activation.argo_readiness_observed_at
     AND argo.lease_until>=activation.argo_readiness_lease_until
     AND argo.observed_at<=authority_time
     AND argo.observed_at>=authority_time-interval '5 minutes'
     AND argo.lease_until>authority_time
     AND argo.lease_until<=argo.observed_at+interval '5 minutes'
     AND argo.lease_until<=authority_time+interval '5 minutes'
    WHERE activation.platform_binding_id=binding_id
      AND activation.activation_epoch=(
          SELECT MAX(current.activation_epoch)
          FROM public.helm_application_cascade_observer_activations AS current
          WHERE current.platform_binding_id=binding_id)
      AND activation.publisher_worker_id=publisher_worker
      AND activation.publisher_worker_epoch=publisher_epoch
      AND activation.publisher_contract=publisher_contract
      AND activation.publisher_policy_version=publisher_policy
      AND activation.publisher_config_digest=publisher_config
      AND activation.argo_worker_id=argo_worker
      AND activation.argo_worker_epoch=argo_epoch
      AND activation.argo_contract=argo_contract
      AND activation.argo_config_digest=argo_config
);
$function$;

CREATE FUNCTION public.activate_helm_application_cascade_observer(
    binding_id uuid,publisher_contract text,publisher_policy text,publisher_config text,
    publisher_worker text,publisher_epoch bigint,
    argo_contract text,argo_config text,argo_worker text,argo_epoch bigint
)
RETURNS bigint
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    existing_epoch bigint;
    active_epoch bigint;
    created_epoch bigint;
    db_now timestamptz := pg_catalog.clock_timestamp();
BEGIN
    PERFORM 1 FROM public.git_repository_bindings AS binding
    WHERE binding.id=binding_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade observer activation binding is absent' USING ERRCODE='23514';
    END IF;
    SELECT activation.activation_epoch INTO existing_epoch
    FROM public.helm_application_cascade_observer_activations AS activation
    WHERE activation.platform_binding_id=binding_id
      AND activation.publisher_worker_id=publisher_worker
      AND activation.publisher_worker_epoch=publisher_epoch
      AND activation.argo_worker_id=argo_worker
      AND activation.argo_worker_epoch=argo_epoch;
    SELECT MAX(activation.activation_epoch) INTO active_epoch
    FROM public.helm_application_cascade_observer_activations AS activation
    WHERE activation.platform_binding_id=binding_id;
    IF existing_epoch IS NOT NULL THEN
        IF existing_epoch IS DISTINCT FROM active_epoch OR
           NOT public.helm_application_cascade_active_observer_is_exact(
               binding_id,publisher_worker,publisher_epoch,publisher_contract,publisher_policy,
               publisher_config,argo_worker,argo_epoch,argo_contract,argo_config,db_now) THEN
            RAISE EXCEPTION 'cascade observer process is no longer active' USING ERRCODE='23514';
        END IF;
        RETURN existing_epoch;
    END IF;
    INSERT INTO public.helm_application_cascade_observer_activations(
        platform_binding_id,activation_epoch,publisher_contract,publisher_policy_version,
        publisher_config_digest,publisher_worker_id,publisher_worker_epoch,
        publisher_started_at,publisher_readiness_observed_at,publisher_readiness_lease_until,
        argo_contract,argo_config_digest,argo_worker_id,argo_worker_epoch,argo_identity,
        argo_started_at,argo_readiness_observed_at,argo_readiness_lease_until,activated_at
    ) VALUES (
        binding_id,1,publisher_contract,publisher_policy,publisher_config,publisher_worker,
        publisher_epoch,db_now,db_now,db_now+interval '1 second',argo_contract,argo_config,
        argo_worker,argo_epoch,'{}'::jsonb,db_now,db_now,db_now+interval '1 second',db_now
    ) RETURNING activation_epoch INTO created_epoch;
    RETURN created_epoch;
END;
$function$;

REVOKE ALL ON FUNCTION public.activate_helm_application_cascade_observer(
    uuid,text,text,text,text,bigint,text,text,text,bigint) FROM PUBLIC;

CREATE FUNCTION public.helm_application_active_publisher_is_exact(
    binding_id uuid,publisher_worker text,publisher_contract text,
    publisher_config text,authority_time timestamptz
)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT COALESCE((
    SELECT public.helm_application_cascade_active_observer_is_exact(
        activation.platform_binding_id,activation.publisher_worker_id,
        activation.publisher_worker_epoch,activation.publisher_contract,
        activation.publisher_policy_version,activation.publisher_config_digest,
        activation.argo_worker_id,activation.argo_worker_epoch,activation.argo_contract,
        activation.argo_config_digest,authority_time)
    FROM public.helm_application_cascade_observer_activations AS activation
    WHERE activation.platform_binding_id=binding_id
      AND activation.activation_epoch=(
          SELECT MAX(current.activation_epoch)
          FROM public.helm_application_cascade_observer_activations AS current
          WHERE current.platform_binding_id=binding_id)
      AND activation.publisher_worker_id=publisher_worker
      AND activation.publisher_contract=publisher_contract
      AND activation.publisher_config_digest=publisher_config
),false);
$function$;

CREATE FUNCTION public.validate_helm_application_active_publisher_claim()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
BEGIN
    IF TG_OP='UPDATE' AND NEW.lease_epoch=OLD.lease_epoch+1 AND
       NEW.lease_owner IS NOT NULL AND NEW.lease_until>NEW.updated_at THEN
        PERFORM pg_catalog.pg_advisory_xact_lock(
            pg_catalog.hashtextextended(NEW.platform_binding_id::text,704215997));
        IF NOT public.helm_application_active_publisher_is_exact(
            NEW.platform_binding_id,NEW.lease_owner,NEW.publisher_contract,
            NEW.publisher_config_digest,NEW.updated_at) THEN
            RAISE EXCEPTION 'protected Helm claim lacks active monotonic publisher authority'
                USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_protected_payload_active_publisher_claim
BEFORE UPDATE ON public.helm_protected_payload_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_active_publisher_claim();
CREATE TRIGGER helm_protected_application_active_publisher_claim
BEFORE UPDATE ON public.helm_protected_application_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_active_publisher_claim();
CREATE TRIGGER helm_application_cascade_active_publisher_claim
BEFORE UPDATE ON public.helm_application_cascade_preflights
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_active_publisher_claim();

ALTER TABLE public.helm_protected_application_intents
    ADD CONSTRAINT helm_protected_application_cascade_preflight_fk
    FOREIGN KEY (cascade_receipt_id)
    REFERENCES public.helm_application_cascade_preflights(id) ON DELETE RESTRICT;

-- Dedicated freshness intentionally does not inspect current environment Git
-- projection/materialization.  Disable payload, prior Application bytes,
-- current release head, and current platform Git authority are sufficient.
CREATE FUNCTION public.helm_application_cascade_preflight_is_fresh(candidate_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT EXISTS (
    SELECT 1
    FROM public.helm_application_cascade_preflights AS preflight
    JOIN public.helm_release_heads AS head
      ON head.environment_id=preflight.environment_id
     AND head.application_id=preflight.application_id
     AND head.revision_id=preflight.release_revision_id
     AND head.generation=preflight.release_generation
    JOIN public.helm_release_revisions AS release
      ON release.id=preflight.release_revision_id
     AND release.generation=preflight.release_generation
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
     AND payload.platform_target_ref=preflight.platform_target_ref
     AND payload.environment_target_ref=preflight.environment_target_ref
     AND payload.environment_revision=preflight.environment_revision
     AND payload.environment_generation=preflight.environment_generation
     AND payload.catalog_digest=preflight.catalog_digest
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
    JOIN public.git_repository_bindings AS platform
      ON platform.id=preflight.platform_binding_id
     AND platform.kind='platform' AND platform.credential_mode='github-app'
     AND platform.cluster_id=preflight.cluster_id
     AND platform.target_ref=preflight.platform_target_ref
     AND platform.path_prefix='clusters/'||preflight.cluster_id::text
     AND platform.state IN ('ready','indexing')
     AND platform.target_head_revision IS NOT NULL
    WHERE preflight.id=candidate_id
      AND preflight.state<>'superseded'
);
$function$;

CREATE FUNCTION public.helm_application_cascade_expected_child_spec_digest(candidate_id uuid)
RETURNS text
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT 'sha256:'||pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(
    '{"Project":'||pg_catalog.to_json(foundation.argo_project)::text||
    ',"Source":{"RepoURL":'||pg_catalog.to_json(
        'https://github.com/'||platform.repository_owner||'/'||platform.repository_name||'.git')::text||
    ',"TargetRevision":'||pg_catalog.to_json(base.payload_revision)::text||
    ',"Path":'||pg_catalog.to_json(
        'clusters/'||preflight.cluster_id::text||'/helm-manifests/environments/'||
        preflight.environment_id::text||'/applications/'||preflight.application_id::text||
        '/revisions/'||base.release_revision_id::text)::text||
    ',"Directory":{"recurse":false,"include":"release.yaml"}},' ||
    '"Destination":{"Server":"https://kubernetes.default.svc","Namespace":'||
        pg_catalog.to_json(foundation.namespace)::text||'},' ||
    '"SyncPolicy":{"Automated":{"Prune":true,"SelfHeal":true,"AllowEmpty":false},' ||
        '"SyncOptions":["CreateNamespace=false","ServerSideApply=true"]}}',
    'UTF8')),'hex')
FROM public.helm_application_cascade_preflights AS preflight
JOIN public.helm_protected_application_intents AS base
  ON base.id=preflight.base_application_intent_id
LEFT JOIN public.helm_application_continuation_receipts AS continuation
  ON base.continuation_required
 AND continuation.application_intent_id=base.id
 AND continuation.release_revision_id=base.release_revision_id
 AND continuation.payload_intent_id=base.payload_intent_id
 AND continuation.project_id=base.project_id
 AND continuation.environment_id=base.environment_id
 AND continuation.application_id=base.application_id
 AND continuation.platform_binding_id=base.platform_binding_id
 AND continuation.environment_binding_id=base.environment_binding_id
 AND continuation.cluster_id=base.cluster_id
 AND continuation.source_environment_revision=base.environment_revision
 AND continuation.source_environment_generation=base.environment_generation
 AND continuation.planned_base_revision=base.planned_base_revision
 AND continuation.application_content_digest=base.content_digest
 AND continuation.application_intent_digest=base.intent_digest
LEFT JOIN public.helm_publication_prerequisite_receipts AS prerequisite
  ON NOT base.continuation_required
 AND prerequisite.release_revision_id=base.prerequisite_receipt_id
 AND prerequisite.release_revision_id=base.release_revision_id
 AND prerequisite.project_id=base.project_id
 AND prerequisite.environment_id=base.environment_id
 AND prerequisite.application_id=base.application_id
 AND prerequisite.platform_binding_id=base.platform_binding_id
 AND prerequisite.environment_binding_id=base.environment_binding_id
 AND prerequisite.cluster_id=base.cluster_id
 AND prerequisite.environment_revision=base.environment_revision
 AND prerequisite.environment_generation=base.environment_generation
JOIN public.environment_foundation_intents AS foundation
  ON foundation.id=CASE WHEN base.continuation_required
       THEN continuation.current_foundation_intent_id
       ELSE prerequisite.foundation_intent_id END
 AND foundation.project_id=base.project_id
 AND foundation.environment_id=base.environment_id
 AND foundation.platform_binding_id=base.platform_binding_id
 AND foundation.cluster_id=base.cluster_id
 AND foundation.target_ref=base.platform_target_ref
JOIN public.git_repository_bindings AS platform
  ON platform.id=preflight.platform_binding_id
WHERE preflight.id=candidate_id;
$function$;

CREATE FUNCTION public.helm_application_cascade_expected_root_spec_digest(candidate_id uuid)
RETURNS text
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT 'sha256:'||pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(
    '{"project":"kuberploy-platform-bootstrap",' ||
    '"source":{"repoURL":'||pg_catalog.to_json(
        'https://github.com/'||platform.repository_owner||'/'||platform.repository_name||'.git')::text||
    ',"targetRevision":'||pg_catalog.to_json(platform.target_ref)::text||
    ',"path":'||pg_catalog.to_json(platform.path_prefix||'/argocd')::text||
    ',"directory":{"recurse":true}},' ||
    '"destination":{"server":"https://kubernetes.default.svc","namespace":'||
        pg_catalog.to_json(preflight.argo_namespace)::text||'},' ||
    '"syncPolicy":{"automated":{"allowEmpty":false,"prune":true,"selfHeal":true},' ||
        '"syncOptions":["CreateNamespace=false","PrunePropagationPolicy=foreground",' ||
        '"RespectIgnoreDifferences=true","ServerSideApply=true"]}}',
    'UTF8')),'hex')
FROM public.helm_application_cascade_preflights AS preflight
JOIN public.git_repository_bindings AS platform
  ON platform.id=preflight.platform_binding_id
WHERE preflight.id=candidate_id;
$function$;

-- All three Git publishers share one serialized authority lane per platform
-- binding.  Existing v009 guards cover payload/Application pairs; this trigger
-- makes the new cascade lane symmetric under concurrent transactions.
CREATE FUNCTION public.validate_helm_protected_cascade_lane()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    authority_time timestamptz := NEW.updated_at;
BEGIN
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(NEW.platform_binding_id::text,704215997));
    IF NEW.lease_owner IS NULL OR NEW.lease_until<=authority_time THEN
        RETURN NEW;
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.helm_protected_payload_intents AS held
        WHERE held.platform_binding_id=NEW.platform_binding_id
          AND NOT (TG_TABLE_NAME='helm_protected_payload_intents' AND held.id=NEW.id)
          AND held.lease_owner IS NOT NULL AND held.lease_until>authority_time
    ) OR EXISTS (
        SELECT 1 FROM public.helm_protected_application_intents AS held
        WHERE held.platform_binding_id=NEW.platform_binding_id
          AND NOT (TG_TABLE_NAME='helm_protected_application_intents' AND held.id=NEW.id)
          AND held.lease_owner IS NOT NULL AND held.lease_until>authority_time
    ) OR EXISTS (
        SELECT 1 FROM public.helm_application_cascade_preflights AS held
        WHERE held.platform_binding_id=NEW.platform_binding_id
          AND NOT (TG_TABLE_NAME='helm_application_cascade_preflights' AND held.id=NEW.id)
          AND held.lease_owner IS NOT NULL AND held.lease_until>authority_time
    ) THEN
        RAISE EXCEPTION 'protected Git authority lane is already leased' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_protected_payload_cascade_lane_guard
BEFORE INSERT OR UPDATE ON public.helm_protected_payload_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_cascade_lane();
CREATE TRIGGER helm_protected_application_cascade_lane_guard
BEFORE INSERT OR UPDATE ON public.helm_protected_application_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_cascade_lane();
CREATE TRIGGER helm_application_cascade_lane_guard
BEFORE INSERT OR UPDATE ON public.helm_application_cascade_preflights
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_protected_cascade_lane();

-- Baseline delete validation expected the prior Application digest.  Cascade
-- delete CAS must instead target exact adopted (finalizer-bearing) postimage.
DO $migration$
DECLARE
    definition text;
    needle text := 'NEW.expected_etag<>''"''||base_row.content_digest||''"'' OR';
    replacement text := $replacement$((NEW.action='delete' AND (
        NOT NEW.cascade_required OR NEW.cascade_receipt_id IS NULL OR
        NEW.expected_etag<>COALESCE((
            SELECT '"'||cascade.adopted_content_digest||'"'
            FROM public.helm_application_cascade_preflights AS cascade
            WHERE cascade.id=NEW.cascade_receipt_id
        ),''))) OR (NEW.action<>'delete' AND
        NEW.expected_etag<>'"'||base_row.content_digest||'"')) OR$replacement$;
BEGIN
    definition := pg_catalog.pg_get_functiondef(
        'public.validate_helm_protected_application_intent()'::pg_catalog.regprocedure);
    IF pg_catalog.strpos(definition,needle)=0 OR
       pg_catalog.strpos(pg_catalog.substr(
           definition,pg_catalog.strpos(definition,needle)+1),needle)>0 THEN
        RAISE EXCEPTION 'unexpected protected Application validator shape before cascade bridge';
    END IF;
    definition := pg_catalog.replace(definition,needle,replacement);
    EXECUTE definition;
END;
$migration$;

ALTER FUNCTION public.validate_helm_protected_application_intent()
SET search_path=pg_catalog,pg_temp;

CREATE FUNCTION public.validate_helm_application_cascade_gate()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
BEGIN
    IF TG_OP='UPDATE' AND
       ROW(NEW.cascade_required,NEW.cascade_receipt_id,NEW.cascade_contract)
       IS DISTINCT FROM
       ROW(OLD.cascade_required,OLD.cascade_receipt_id,OLD.cascade_contract) THEN
        RAISE EXCEPTION 'cascade delete authority is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.action='publish' THEN
        IF NEW.cascade_required OR NEW.cascade_receipt_id IS NOT NULL OR NEW.cascade_contract<>'' THEN
            RAISE EXCEPTION 'publish cannot carry cascade delete authority' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT NEW.cascade_required OR NEW.cascade_receipt_id IS NULL OR
       NEW.cascade_contract<>'helm-application-cascade-preflight.v1' OR
       NOT EXISTS (
           SELECT 1
           FROM public.helm_application_cascade_preflights AS preflight
           WHERE preflight.id=NEW.cascade_receipt_id
             AND preflight.release_revision_id=NEW.release_revision_id
             AND preflight.payload_intent_id=NEW.payload_intent_id
             AND preflight.release_generation=NEW.release_generation
             AND preflight.project_id=NEW.project_id
             AND preflight.environment_id=NEW.environment_id
             AND preflight.application_id=NEW.application_id
             AND preflight.platform_binding_id=NEW.platform_binding_id
             AND preflight.environment_binding_id=NEW.environment_binding_id
             AND preflight.cluster_id=NEW.cluster_id
             AND preflight.platform_target_ref=NEW.platform_target_ref
             AND preflight.application_path=NEW.application_path
             AND NEW.expected_etag='"'||preflight.adopted_content_digest||'"'
             AND preflight.state='verified'
       ) THEN
        RAISE EXCEPTION 'delete lacks stable cascade preflight authority' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_gate
BEFORE INSERT OR UPDATE ON public.helm_protected_application_intents
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_gate();

CREATE FUNCTION public.validate_helm_application_cascade_exact_gate()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
BEGIN
    IF NEW.action='delete' AND (
        TG_OP='INSERT' OR (OLD.lease_epoch=0 AND NEW.lease_epoch=1)
    ) AND NOT public.helm_application_cascade_is_exact(
        NEW.id,NEW.publisher_config_digest,pg_catalog.clock_timestamp()) THEN
        RAISE EXCEPTION 'delete cascade observation is absent or stale' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$function$;

CREATE CONSTRAINT TRIGGER helm_application_cascade_exact_gate
AFTER INSERT OR UPDATE ON public.helm_protected_application_intents
DEFERRABLE INITIALLY IMMEDIATE
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_exact_gate();

CREATE FUNCTION public.validate_helm_application_cascade_observation_job()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    activation public.helm_application_cascade_observer_activations%ROWTYPE;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'cascade observation jobs are durable' USING ERRCODE='23514';
    END IF;
    PERFORM 1
    FROM public.helm_application_cascade_preflights AS preflight
    WHERE preflight.id=NEW.cascade_preflight_id
      AND preflight.platform_binding_id=NEW.platform_binding_id
    FOR UPDATE;
    IF NOT FOUND OR NOT public.helm_application_cascade_preflight_is_fresh(NEW.cascade_preflight_id) OR
       NOT EXISTS (
           SELECT 1 FROM public.helm_application_cascade_preflights AS preflight
           WHERE preflight.id=NEW.cascade_preflight_id AND preflight.state='verified'
             AND preflight.verified_path_digest=preflight.adopted_content_digest
       ) THEN
        RAISE EXCEPTION 'cascade observation job lacks current verified preflight' USING ERRCODE='23514';
    END IF;
    SELECT value.* INTO activation
    FROM public.helm_application_cascade_observer_activations AS value
    WHERE value.platform_binding_id=NEW.platform_binding_id
      AND value.activation_epoch=NEW.activation_epoch;
    IF NOT FOUND OR NEW.publisher_contract<>activation.publisher_contract OR
       NEW.publisher_policy_version<>activation.publisher_policy_version OR
       NEW.publisher_config_digest<>activation.publisher_config_digest THEN
        RAISE EXCEPTION 'cascade observation job lacks exact observer activation'
            USING ERRCODE='23514';
    END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.state<>'pending' OR NEW.next_attempt_at<>NEW.created_at OR
           NEW.updated_at<>NEW.created_at OR NEW.attempts<>0 OR
           NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
           NEW.lease_owner IS NOT NULL OR NEW.worker_epoch IS NOT NULL OR NEW.lease_epoch<>0 OR
           NEW.lease_until IS NOT NULL OR NEW.completed_at IS NOT NULL OR
           NEW.superseded_at IS NOT NULL OR
           NEW.created_at>db_now+interval '30 seconds' OR NEW.created_at<db_now-interval '30 seconds' THEN
            RAISE EXCEPTION 'cascade observation job must start pristine' USING ERRCODE='23514';
        END IF;
        IF activation.activation_epoch<>(
               SELECT MAX(current.activation_epoch)
               FROM public.helm_application_cascade_observer_activations AS current
               WHERE current.platform_binding_id=NEW.platform_binding_id) OR
           NOT public.helm_application_cascade_active_observer_is_exact(
               activation.platform_binding_id,activation.publisher_worker_id,
               activation.publisher_worker_epoch,activation.publisher_contract,
               activation.publisher_policy_version,activation.publisher_config_digest,
               activation.argo_worker_id,activation.argo_worker_epoch,
               activation.argo_contract,activation.argo_config_digest,db_now) THEN
            RAISE EXCEPTION 'cascade observation job activation is not current'
                USING ERRCODE='23514';
        END IF;
        UPDATE public.helm_application_cascade_observation_jobs AS older
           SET state='superseded',lease_owner=NULL,worker_epoch=NULL,lease_until=NULL,
               consecutive_failures=GREATEST(older.consecutive_failures,1),
               last_failure_code='observer-activation-superseded',
               completed_at=COALESCE(older.completed_at,db_now),superseded_at=db_now,
               updated_at=db_now
         WHERE older.cascade_preflight_id=NEW.cascade_preflight_id
           AND older.activation_epoch<NEW.activation_epoch
           AND older.state<>'superseded';
    ELSE
        IF ROW(NEW.cascade_preflight_id,NEW.platform_binding_id,NEW.activation_epoch,
               NEW.publisher_config_digest,NEW.publisher_contract,
               NEW.publisher_policy_version,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.cascade_preflight_id,OLD.platform_binding_id,OLD.activation_epoch,
               OLD.publisher_config_digest,OLD.publisher_contract,
               OLD.publisher_policy_version,OLD.created_at) OR
           OLD.state='superseded' OR NEW.updated_at<OLD.updated_at OR
           NEW.updated_at>db_now+interval '30 seconds' THEN
            RAISE EXCEPTION 'cascade observation job transition invalid' USING ERRCODE='23514';
        END IF;
        IF NEW.state='superseded' THEN
            IF pg_catalog.pg_trigger_depth()<=1 OR NEW.activation_epoch>=
                   (SELECT MAX(current.activation_epoch)
                    FROM public.helm_application_cascade_observer_activations AS current
                    WHERE current.platform_binding_id=NEW.platform_binding_id) OR
               NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR
               NEW.superseded_at IS NULL OR NEW.superseded_at<>NEW.updated_at OR
               NEW.lease_owner IS NOT NULL OR NEW.worker_epoch IS NOT NULL OR
               NEW.lease_until IS NOT NULL OR NEW.completed_at IS NULL OR
               NEW.last_failure_code<>'observer-activation-superseded' OR
               NEW.consecutive_failures<>GREATEST(OLD.consecutive_failures,1) THEN
                RAISE EXCEPTION 'cascade observer activation supersede is not internal and exact'
                    USING ERRCODE='23514';
            END IF;
            RETURN NEW;
        END IF;
        IF activation.activation_epoch<>(
               SELECT MAX(current.activation_epoch)
               FROM public.helm_application_cascade_observer_activations AS current
               WHERE current.platform_binding_id=NEW.platform_binding_id) OR
           NOT public.helm_application_cascade_active_observer_is_exact(
               activation.platform_binding_id,activation.publisher_worker_id,
               activation.publisher_worker_epoch,activation.publisher_contract,
               activation.publisher_policy_version,activation.publisher_config_digest,
               activation.argo_worker_id,activation.argo_worker_epoch,
               activation.argo_contract,activation.argo_config_digest,db_now) THEN
            RAISE EXCEPTION 'cascade observation job lost active observer authority'
                USING ERRCODE='23514';
        END IF;
        IF OLD.state='pending' AND NEW.state='claimed' THEN
            IF NEW.attempts<>OLD.attempts+1 OR NEW.lease_epoch<>OLD.lease_epoch+1 OR
               NEW.lease_owner<>activation.publisher_worker_id OR
               NEW.worker_epoch<>activation.publisher_worker_epoch OR
               NEW.lease_until<=NEW.updated_at OR
               NEW.lease_until>NEW.updated_at+interval '5 minutes' OR
               NEW.consecutive_failures<>OLD.consecutive_failures OR
               NEW.last_failure_code<>OLD.last_failure_code OR NEW.completed_at IS NOT NULL THEN
                RAISE EXCEPTION 'cascade observation initial claim is not exact' USING ERRCODE='23514';
            END IF;
        ELSIF OLD.state='claimed' AND NEW.state='claimed' THEN
            IF OLD.lease_until>NEW.updated_at OR NEW.attempts<>OLD.attempts+1 OR
               NEW.lease_epoch<>OLD.lease_epoch+1 OR
               NEW.lease_owner<>activation.publisher_worker_id OR
               NEW.worker_epoch<>activation.publisher_worker_epoch OR
               NEW.lease_until<=NEW.updated_at OR
               NEW.lease_until>NEW.updated_at+interval '5 minutes' OR
               NEW.consecutive_failures<>OLD.consecutive_failures OR
               NEW.last_failure_code<>OLD.last_failure_code OR NEW.completed_at IS NOT NULL THEN
                RAISE EXCEPTION 'cascade observation recovery claim is not exact' USING ERRCODE='23514';
            END IF;
        ELSIF OLD.state='claimed' AND NEW.state IN ('pending','failed') THEN
            IF NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR
               NEW.consecutive_failures<>OLD.consecutive_failures+1 OR
               NEW.last_failure_code='' OR NEW.lease_owner IS NOT NULL OR
               NEW.worker_epoch IS NOT NULL OR NEW.lease_until IS NOT NULL OR
               ((NEW.state='pending')<>(NEW.completed_at IS NULL)) THEN
                RAISE EXCEPTION 'cascade observation retry is not exact' USING ERRCODE='23514';
            END IF;
        ELSIF OLD.state='claimed' AND NEW.state='verified' THEN
            IF NEW.attempts<>OLD.attempts OR NEW.lease_epoch<>OLD.lease_epoch OR
               NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
               NEW.lease_owner IS NOT NULL OR NEW.worker_epoch IS NOT NULL OR
               NEW.lease_until IS NOT NULL OR NEW.completed_at IS NULL THEN
                RAISE EXCEPTION 'cascade observation completion is not exact' USING ERRCODE='23514';
            END IF;
        ELSIF OLD.state='verified' AND NEW.state='pending' THEN
            IF NEW.attempts<>0 OR NEW.lease_epoch<>OLD.lease_epoch OR
               NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
               NEW.lease_owner IS NOT NULL OR NEW.worker_epoch IS NOT NULL OR
               NEW.lease_until IS NOT NULL OR NEW.completed_at IS NOT NULL OR
               public.helm_application_cascade_observation_is_exact(
                   NEW.cascade_preflight_id,NEW.publisher_config_digest,NEW.updated_at) THEN
                RAISE EXCEPTION 'cascade observation refresh is not exact' USING ERRCODE='23514';
            END IF;
        ELSE
            RAISE EXCEPTION 'cascade observation job transition is not permitted' USING ERRCODE='23514';
        END IF;
    END IF;
    IF NOT (
        (NEW.state='pending' AND NEW.lease_owner IS NULL AND NEW.worker_epoch IS NULL AND
         NEW.completed_at IS NULL) OR
        (NEW.state='claimed' AND NEW.lease_owner IS NOT NULL AND NEW.worker_epoch>0 AND
         NEW.lease_epoch>0 AND NEW.attempts>0 AND NEW.lease_until>NEW.updated_at AND
         NEW.lease_until<=NEW.updated_at+interval '5 minutes' AND NEW.completed_at IS NULL) OR
        (NEW.state IN ('verified','failed','superseded') AND NEW.lease_owner IS NULL AND
         NEW.worker_epoch IS NULL AND NEW.completed_at IS NOT NULL)
    ) THEN
        RAISE EXCEPTION 'cascade observation job runtime shape invalid' USING ERRCODE='23514';
    END IF;
    IF NEW.state='claimed' AND public.helm_application_cascade_observation_is_exact(
           NEW.cascade_preflight_id,NEW.publisher_config_digest,NEW.updated_at) THEN
            RAISE EXCEPTION 'cascade observation claim lacks current publisher authority' USING ERRCODE='23514';
    END IF;
    IF TG_OP='UPDATE' AND OLD.state='claimed' AND NEW.state='verified' AND
       NOT EXISTS (
           SELECT 1 FROM public.helm_application_cascade_receipts AS receipt
           WHERE receipt.cascade_preflight_id=NEW.cascade_preflight_id
             AND receipt.publisher_config_digest=NEW.publisher_config_digest
             AND receipt.observer_activation_epoch=NEW.activation_epoch
             AND receipt.publisher_contract=NEW.publisher_contract
             AND receipt.publisher_policy_version=NEW.publisher_policy_version
             AND receipt.worker_id=OLD.lease_owner
             AND receipt.worker_epoch=OLD.worker_epoch
             AND receipt.observation_lease_epoch=OLD.lease_epoch
             AND receipt.observed_at=NEW.completed_at
       ) THEN
        RAISE EXCEPTION 'cascade observation completion lacks exact receipt' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_observation_job_guard
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_application_cascade_observation_jobs
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_observation_job();

CREATE FUNCTION public.validate_helm_application_cascade_receipt()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    preflight public.helm_application_cascade_preflights%ROWTYPE;
    activation public.helm_application_cascade_observer_activations%ROWTYPE;
    expected_epoch bigint;
    active_activation_epoch bigint;
    argo_readiness public.runtime_readiness%ROWTYPE;
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'cascade observation receipts are immutable' USING ERRCODE='23514';
    END IF;
    SELECT value.* INTO preflight
    FROM public.helm_application_cascade_preflights AS value
    WHERE value.id=NEW.cascade_preflight_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade receipt preflight is absent' USING ERRCODE='23514';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(preflight.platform_binding_id::text,704215997));
    -- Observation authority time is database-owned. Ignore both caller fields;
    -- the locked preflight serializes an append-only epoch for this cascade.
    NEW.observed_at := db_now;
    SELECT COALESCE(MAX(receipt.observation_epoch),0)+1 INTO expected_epoch
    FROM public.helm_application_cascade_receipts AS receipt
    WHERE receipt.cascade_preflight_id=preflight.id;
    NEW.observation_epoch := expected_epoch;

    SELECT job.activation_epoch INTO active_activation_epoch
    FROM public.helm_application_cascade_observation_jobs AS job
    WHERE job.cascade_preflight_id=preflight.id
      AND job.platform_binding_id=preflight.platform_binding_id
      AND job.publisher_config_digest=NEW.publisher_config_digest
      AND job.publisher_contract=NEW.publisher_contract
      AND job.publisher_policy_version=NEW.publisher_policy_version
      AND job.state='claimed' AND job.lease_owner=NEW.worker_id
      AND job.worker_epoch=NEW.worker_epoch
      AND job.lease_epoch=NEW.observation_lease_epoch
      AND job.superseded_at IS NULL
      AND job.lease_until>db_now
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade receipt lacks active observer authority' USING ERRCODE='23514';
    END IF;
    NEW.observer_activation_epoch := active_activation_epoch;
    SELECT value.* INTO activation
    FROM public.helm_application_cascade_observer_activations AS value
    WHERE value.platform_binding_id=preflight.platform_binding_id
      AND value.activation_epoch=NEW.observer_activation_epoch;
    IF NOT FOUND OR activation.activation_epoch<>(
           SELECT MAX(current.activation_epoch)
           FROM public.helm_application_cascade_observer_activations AS current
           WHERE current.platform_binding_id=preflight.platform_binding_id) OR
       activation.publisher_worker_id<>NEW.worker_id OR
       activation.publisher_worker_epoch<>NEW.worker_epoch OR
       activation.publisher_contract<>NEW.publisher_contract OR
       activation.publisher_policy_version<>NEW.publisher_policy_version OR
       activation.publisher_config_digest<>NEW.publisher_config_digest OR
       NOT public.helm_application_cascade_active_observer_is_exact(
           activation.platform_binding_id,activation.publisher_worker_id,
           activation.publisher_worker_epoch,activation.publisher_contract,
           activation.publisher_policy_version,activation.publisher_config_digest,
           activation.argo_worker_id,activation.argo_worker_epoch,
           activation.argo_contract,activation.argo_config_digest,db_now) THEN
        RAISE EXCEPTION 'cascade receipt observer activation is stale' USING ERRCODE='23514';
    END IF;

    SELECT readiness.* INTO argo_readiness
    FROM public.runtime_readiness AS readiness
    WHERE readiness.runtime_kind='argo-desired-state'
      AND readiness.scope_key='global'
      AND readiness.platform_binding_id=preflight.platform_binding_id
      AND readiness.contract_version=activation.argo_contract
      AND readiness.config_digest=activation.argo_config_digest
      AND readiness.worker_id=activation.argo_worker_id
      AND readiness.worker_epoch=activation.argo_worker_epoch
      AND readiness.identity=activation.argo_identity
      AND readiness.started_at=activation.argo_started_at
      AND readiness.updated_at=readiness.observed_at
      AND readiness.observed_at>=activation.argo_readiness_observed_at
      AND readiness.lease_until>=activation.argo_readiness_lease_until
      AND readiness.observed_at<=db_now AND readiness.observed_at>=db_now-interval '5 minutes'
      AND readiness.lease_until>db_now
      AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
      AND readiness.lease_until<=db_now+interval '5 minutes'
    FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade receipt lacks current Argo runtime authority' USING ERRCODE='23514';
    END IF;
    NEW.argo_contract := argo_readiness.contract_version;
    NEW.argo_config_digest := argo_readiness.config_digest;
    NEW.argo_worker_id := argo_readiness.worker_id;
    NEW.argo_worker_epoch := argo_readiness.worker_epoch;
    NEW.argo_started_at := argo_readiness.started_at;
    NEW.argo_readiness_observed_at := argo_readiness.observed_at;
    NEW.argo_readiness_lease_until := argo_readiness.lease_until;

    IF preflight.state<>'verified' OR preflight.verified_at IS NULL OR
       preflight.verified_at>db_now OR preflight.updated_at>db_now OR
       preflight.verified_path_digest<>preflight.adopted_content_digest OR
       NOT public.helm_application_cascade_preflight_is_fresh(preflight.id) OR
       NEW.delete_intent_id<>preflight.delete_intent_id OR
       NEW.release_revision_id<>preflight.release_revision_id OR
       NEW.payload_intent_id<>preflight.payload_intent_id OR
       NEW.base_application_intent_id<>preflight.base_application_intent_id OR
       NEW.project_id<>preflight.project_id OR NEW.environment_id<>preflight.environment_id OR
       NEW.application_id<>preflight.application_id OR NEW.cluster_id<>preflight.cluster_id OR
       NEW.application_path<>preflight.application_path OR
       NEW.source_content_digest<>preflight.source_content_digest OR
       NEW.adopted_content_digest<>preflight.adopted_content_digest OR
       activation.argo_identity->>'clusterId'<>preflight.cluster_id::text OR
       activation.argo_identity->>'argoNamespace'<>preflight.argo_namespace OR
       activation.argo_identity->>'rootApplicationName'<>'kuberploy-platform-root' OR
       NEW.adoption_revision<>(CASE WHEN preflight.operation='update'
           THEN preflight.committed_revision ELSE preflight.write_base_revision END) OR
       NEW.adoption_parent_revision<>(CASE WHEN preflight.operation='update'
           THEN preflight.committed_parent_revision ELSE preflight.write_base_revision END) OR
       NEW.root_observed_revision<>NEW.provider_head OR NEW.root_sync_status<>'Synced' OR
       NEW.root_spec_digest IS DISTINCT FROM
           public.helm_application_cascade_expected_root_spec_digest(preflight.id) OR
       NEW.child_spec_digest IS DISTINCT FROM
           public.helm_application_cascade_expected_child_spec_digest(preflight.id) OR
       NOT EXISTS (
           SELECT 1 FROM public.git_repository_bindings AS platform
           WHERE platform.id=preflight.platform_binding_id AND platform.kind='platform'
             AND platform.credential_mode='github-app' AND platform.cluster_id=preflight.cluster_id
             AND platform.target_ref=preflight.platform_target_ref
             AND platform.target_head_revision=NEW.provider_head
           FOR KEY SHARE
       ) OR
       NOT EXISTS (
           SELECT 1
           FROM public.helm_protected_application_intents AS base
           JOIN public.helm_protected_payload_intents AS child_payload
             ON child_payload.id=base.payload_intent_id
           WHERE base.id=preflight.base_application_intent_id
             AND base.state='verified' AND base.action='publish'
             AND child_payload.state='verified' AND child_payload.action='publish'
             AND NEW.child_release_revision_id=base.release_revision_id
             AND NEW.child_release_revision_id=child_payload.release_revision_id
             AND NEW.child_payload_revision=base.payload_revision
             AND NEW.child_payload_revision=child_payload.committed_revision
             AND NEW.child_payload_path=base.payload_path
             AND NEW.child_payload_path=child_payload.path
             AND NEW.child_payload_digest=child_payload.content_digest
       ) OR
       NOT EXISTS (
           SELECT 1 FROM public.runtime_readiness AS readiness
           WHERE readiness.runtime_kind='helm-protected-publisher'
             AND readiness.scope_key='global' AND readiness.worker_id=NEW.worker_id
             AND readiness.worker_epoch=NEW.worker_epoch
             AND readiness.contract_version=NEW.publisher_contract
             AND readiness.config_digest=NEW.publisher_config_digest
             AND readiness.identity=pg_catalog.jsonb_build_object(
                 'policyVersion',NEW.publisher_policy_version)
             AND readiness.updated_at=readiness.observed_at
             AND readiness.observed_at<=db_now AND readiness.observed_at>=db_now-interval '5 minutes'
             AND readiness.lease_until>db_now
             AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
             AND readiness.lease_until<=db_now+interval '5 minutes'
       ) THEN
        RAISE EXCEPTION 'cascade receipt lacks exact current authority' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_receipt_guard
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_application_cascade_receipts
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_receipt();

CREATE FUNCTION public.validate_helm_application_cascade_preflight()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    source_text text;
    adopted_text text;
    namespace_line text;
    finalizer_block text := E'    finalizers:\n        - resources-finalizer.argocd.argoproj.io\n';
    namespace_position integer;
    replacement public.helm_application_cascade_preflights%ROWTYPE;
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'cascade preflights are immutable history' USING ERRCODE='23514';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(NEW.platform_binding_id::text,704215997));

    IF TG_OP='UPDATE' THEN
        IF OLD.state IN ('verified','failed','superseded') THEN
            RAISE EXCEPTION 'terminal cascade preflight is immutable' USING ERRCODE='23514';
        END IF;
        IF ROW(NEW.id,NEW.delete_intent_id,NEW.release_revision_id,NEW.payload_intent_id,
               NEW.base_application_intent_id,NEW.release_generation,NEW.payload_revision,
               NEW.project_id,NEW.environment_id,NEW.application_id,NEW.platform_binding_id,
               NEW.environment_binding_id,NEW.cluster_id,NEW.platform_target_ref,
               NEW.environment_target_ref,NEW.environment_revision,NEW.environment_generation,
               NEW.catalog_digest,NEW.planned_base_revision,NEW.argo_namespace,
               NEW.application_path,NEW.source_content,NEW.source_content_digest,
               NEW.adopted_content,NEW.adopted_content_digest,NEW.operation,NEW.precondition,
               NEW.expected_etag,NEW.intent_digest,NEW.commit_trailer,NEW.contract,
               NEW.publisher_contract,NEW.publisher_policy_version,
               NEW.original_publisher_config_digest,NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id,OLD.delete_intent_id,OLD.release_revision_id,OLD.payload_intent_id,
               OLD.base_application_intent_id,OLD.release_generation,OLD.payload_revision,
               OLD.project_id,OLD.environment_id,OLD.application_id,OLD.platform_binding_id,
               OLD.environment_binding_id,OLD.cluster_id,OLD.platform_target_ref,
               OLD.environment_target_ref,OLD.environment_revision,OLD.environment_generation,
               OLD.catalog_digest,OLD.planned_base_revision,OLD.argo_namespace,
               OLD.application_path,OLD.source_content,OLD.source_content_digest,
               OLD.adopted_content,OLD.adopted_content_digest,OLD.operation,OLD.precondition,
               OLD.expected_etag,OLD.intent_digest,OLD.commit_trailer,OLD.contract,
               OLD.publisher_contract,OLD.publisher_policy_version,
               OLD.original_publisher_config_digest,OLD.created_at) THEN
            RAISE EXCEPTION 'cascade preflight immutable identity changed' USING ERRCODE='23514';
        END IF;
        IF NEW.updated_at<OLD.updated_at OR NEW.updated_at>db_now+interval '30 seconds' OR
           NEW.next_attempt_at<NEW.created_at OR NEW.attempts<OLD.attempts OR
           NEW.attempts>OLD.attempts+1 OR NEW.lease_epoch<OLD.lease_epoch OR
           NEW.lease_epoch>OLD.lease_epoch+1 OR
           NEW.consecutive_failures<OLD.consecutive_failures OR
           NEW.consecutive_failures>OLD.consecutive_failures+1 OR
           NEW.prerequisite_epoch<>OLD.prerequisite_epoch+1 OR
           (NEW.lease_epoch=OLD.lease_epoch+1 AND
            NEW.attempts<>LEAST(OLD.attempts+1,30)) OR
           (NEW.lease_epoch=OLD.lease_epoch AND NEW.attempts<>OLD.attempts) THEN
            RAISE EXCEPTION 'cascade preflight fencing or time regressed' USING ERRCODE='23514';
        END IF;
        IF OLD.committed_revision<>'' AND
           ROW(NEW.committed_revision,NEW.committed_parent_revision,NEW.committed_at)
           IS DISTINCT FROM ROW(OLD.committed_revision,OLD.committed_parent_revision,OLD.committed_at) THEN
            RAISE EXCEPTION 'cascade preflight commit receipt is immutable' USING ERRCODE='23514';
        END IF;
        IF OLD.verified_at IS NOT NULL AND
           ROW(NEW.verified_at,NEW.verified_path_digest,NEW.provider_request)
           IS DISTINCT FROM ROW(OLD.verified_at,OLD.verified_path_digest,OLD.provider_request) THEN
            RAISE EXCEPTION 'cascade preflight verification is immutable' USING ERRCODE='23514';
        END IF;
        IF NOT (
            (OLD.state='pending' AND NEW.state IN ('pending','claimed','failed','superseded')) OR
            (OLD.state='claimed' AND NEW.state IN ('pending','claimed','git-committed','verified','failed','superseded')) OR
            (OLD.state='git-committed' AND NEW.state IN ('git-committed','verified'))
        ) THEN
            RAISE EXCEPTION 'cascade preflight state transition invalid' USING ERRCODE='23514';
        END IF;
        IF NEW.publisher_config_digest=OLD.publisher_config_digest THEN
            IF NEW.publisher_adoption_epoch<>OLD.publisher_adoption_epoch THEN
                RAISE EXCEPTION 'cascade publisher epoch changed without authority transfer' USING ERRCODE='23514';
            END IF;
        ELSIF NEW.publisher_adoption_epoch<>OLD.publisher_adoption_epoch+1 OR
              NOT EXISTS (
                  SELECT 1
                  FROM public.helm_application_cascade_adoption_receipts AS receipt
                  WHERE receipt.cascade_preflight_id=OLD.id
                    AND receipt.adoption_epoch=NEW.publisher_adoption_epoch
                    AND receipt.original_config_digest=OLD.original_publisher_config_digest
                    AND receipt.previous_config_digest=OLD.publisher_config_digest
                    AND receipt.adopted_config_digest=NEW.publisher_config_digest
                    AND receipt.previous_lease_epoch=OLD.lease_epoch
                    AND receipt.adopted_lease_epoch=NEW.lease_epoch
                    AND receipt.adopted_by_worker=NEW.lease_owner
                    AND receipt.created_at=NEW.updated_at
              ) THEN
            RAISE EXCEPTION 'cascade publisher adoption lacks exact receipt' USING ERRCODE='23514';
        END IF;
        IF OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NOT NULL AND
           NEW.lease_epoch=OLD.lease_epoch AND NEW.lease_owner<>OLD.lease_owner THEN
            RAISE EXCEPTION 'cascade lease owner changed without new epoch' USING ERRCODE='23514';
        END IF;
    ELSE
        IF NEW.state<>'pending' OR NEW.next_attempt_at<>NEW.created_at OR
           NEW.updated_at<>NEW.created_at OR NEW.attempts<>0 OR
           NEW.consecutive_failures<>0 OR NEW.last_failure_code<>'' OR
           NEW.lease_owner IS NOT NULL OR NEW.lease_epoch<>0 OR NEW.lease_until IS NOT NULL OR
           NEW.write_base_revision<>'' OR NEW.write_base_observed_at IS NOT NULL OR
           NEW.committed_revision<>'' OR NEW.committed_parent_revision<>'' OR
           NEW.committed_at IS NOT NULL OR NEW.verified_at IS NOT NULL OR
           NEW.verified_path_digest<>'' OR NEW.provider_request<>'' OR NEW.completed_at IS NOT NULL OR
           NEW.publisher_adoption_epoch<>0 OR NEW.prerequisite_epoch<>0 OR
           NEW.publisher_config_digest<>NEW.original_publisher_config_digest OR
           NEW.created_at>db_now+interval '30 seconds' OR NEW.created_at<db_now-interval '30 seconds' THEN
            RAISE EXCEPTION 'cascade preflight must start pristine' USING ERRCODE='23514';
        END IF;

        SELECT prior.* INTO replacement
        FROM public.helm_application_cascade_preflights AS prior
        WHERE prior.payload_intent_id=NEW.payload_intent_id
        ORDER BY prior.created_at DESC,prior.id DESC
        LIMIT 1
        FOR KEY SHARE;
        IF FOUND AND NOT (
            replacement.state='superseded' AND
            replacement.last_failure_code='cascade-projection-superseded' AND
            replacement.lease_epoch=0 AND replacement.attempts=0 AND
            replacement.lease_owner IS NULL AND replacement.lease_until IS NULL AND
            replacement.write_base_revision='' AND replacement.write_base_observed_at IS NULL AND
            replacement.committed_revision='' AND replacement.committed_parent_revision='' AND
            replacement.committed_at IS NULL AND replacement.verified_at IS NULL AND
            replacement.verified_path_digest='' AND replacement.provider_request='' AND
            replacement.completed_at IS NOT NULL
        ) THEN
            RAISE EXCEPTION 'cascade preflight replacement is not pristine' USING ERRCODE='23514';
        END IF;

        IF NOT public.helm_application_cascade_preflight_is_fresh(NEW.id) THEN
            -- INSERT row is not yet visible through the SQL helper.  Validate its
            -- exact durable tuple directly without environment projection state.
            IF NOT EXISTS (
                SELECT 1
                FROM public.helm_release_heads AS head
                JOIN public.helm_release_revisions AS release ON release.id=head.revision_id
                JOIN public.helm_protected_payload_intents AS payload ON payload.id=NEW.payload_intent_id
                JOIN public.helm_protected_application_intents AS base ON base.id=NEW.base_application_intent_id
                JOIN public.git_repository_bindings AS platform ON platform.id=NEW.platform_binding_id
                WHERE head.environment_id=NEW.environment_id AND head.application_id=NEW.application_id
                  AND head.revision_id=NEW.release_revision_id AND head.generation=NEW.release_generation
                  AND release.id=NEW.release_revision_id AND release.generation=NEW.release_generation
                  AND release.project_id=NEW.project_id AND release.environment_id=NEW.environment_id
                  AND release.application_id=NEW.application_id AND release.action='disable'
                  AND NOT release.desired_enabled AND release.base_intent_id=NEW.base_application_intent_id
                  AND payload.release_revision_id=NEW.release_revision_id
                  AND payload.release_generation=NEW.release_generation
                  AND payload.project_id=NEW.project_id AND payload.environment_id=NEW.environment_id
                  AND payload.application_id=NEW.application_id
                  AND payload.platform_binding_id=NEW.platform_binding_id
                  AND payload.environment_binding_id=NEW.environment_binding_id
                  AND payload.cluster_id=NEW.cluster_id
                  AND payload.platform_target_ref=NEW.platform_target_ref
                  AND payload.environment_target_ref=NEW.environment_target_ref
                  AND payload.environment_revision=NEW.environment_revision
                  AND payload.environment_generation=NEW.environment_generation
                  AND payload.catalog_digest=NEW.catalog_digest
                  AND payload.state='verified' AND payload.action='disable-receipt'
                  AND payload.committed_revision=NEW.payload_revision
                  AND base.state='verified' AND base.action='publish'
                  AND base.project_id=NEW.project_id AND base.environment_id=NEW.environment_id
                  AND base.application_id=NEW.application_id
                  AND base.platform_binding_id=NEW.platform_binding_id AND base.cluster_id=NEW.cluster_id
                  AND base.application_path=NEW.application_path
                  AND base.content=NEW.source_content AND base.content_digest=NEW.source_content_digest
                  AND platform.kind='platform' AND platform.credential_mode='github-app'
                  AND platform.cluster_id=NEW.cluster_id AND platform.target_ref=NEW.platform_target_ref
                  AND platform.path_prefix='clusters/'||NEW.cluster_id::text
                  AND platform.state IN ('ready','indexing')
                  AND platform.target_head_revision=NEW.planned_base_revision
                FOR KEY SHARE OF release,payload,base,platform
            ) THEN
                RAISE EXCEPTION 'cascade preflight authority is stale' USING ERRCODE='23514';
            END IF;
        END IF;

        source_text := pg_catalog.convert_from(NEW.source_content,'UTF8');
        adopted_text := pg_catalog.convert_from(NEW.adopted_content,'UTF8');
        namespace_line := '    namespace: '||NEW.argo_namespace||E'\n';
        namespace_position := pg_catalog.strpos(source_text,namespace_line);
        IF NEW.expected_etag<>'"'||NEW.source_content_digest||'"' OR
           NEW.commit_trailer<>'Kuberploy-Helm-Cascade-Preflight: '||NEW.id::text OR
           namespace_position=0 OR
           pg_catalog.strpos(pg_catalog.substr(source_text,namespace_position+1),namespace_line)>0 THEN
            RAISE EXCEPTION 'cascade preflight framing is invalid' USING ERRCODE='23514';
        END IF;
        IF NEW.operation='observe' THEN
            IF source_text<>adopted_text OR
               pg_catalog.strpos(source_text,finalizer_block)=0 OR
               pg_catalog.strpos(pg_catalog.substr(
                   source_text,pg_catalog.strpos(source_text,finalizer_block)+1),finalizer_block)>0 THEN
                RAISE EXCEPTION 'cascade observe lacks sole foreground finalizer' USING ERRCODE='23514';
            END IF;
        ELSIF NEW.operation='update' THEN
            IF pg_catalog.strpos(source_text,E'    finalizers:\n')<>0 OR
               adopted_text<>pg_catalog.substr(source_text,1,
                   namespace_position+pg_catalog.length(namespace_line)-1)||finalizer_block||
                   pg_catalog.substr(source_text,namespace_position+pg_catalog.length(namespace_line)) THEN
                RAISE EXCEPTION 'cascade update changed more than foreground finalizer' USING ERRCODE='23514';
            END IF;
        ELSE
            RAISE EXCEPTION 'cascade operation is invalid' USING ERRCODE='23514';
        END IF;
    END IF;

    IF NEW.attempts=0 AND (
           NEW.lease_epoch<>0 OR NEW.lease_owner IS NOT NULL OR NEW.write_base_revision<>'' OR
           NEW.committed_revision<>'' OR NEW.verified_at IS NOT NULL) OR
       NEW.lease_epoch<NEW.attempts OR
       (NEW.consecutive_failures>NEW.attempts AND NOT (
          NEW.state='superseded' AND NEW.attempts=0 AND NEW.lease_epoch=0 AND
          NEW.consecutive_failures=1 AND
          NEW.last_failure_code='cascade-projection-superseded')) OR
       (NEW.consecutive_failures=30 AND NEW.state<>'failed') OR
       (NEW.write_base_observed_at IS NOT NULL AND NEW.write_base_observed_at>NEW.updated_at) OR
       (NEW.committed_at IS NOT NULL AND NEW.committed_at<NEW.write_base_observed_at) OR
       (NEW.verified_at IS NOT NULL AND NEW.committed_at IS NOT NULL AND NEW.verified_at<NEW.committed_at) THEN
        RAISE EXCEPTION 'cascade runtime counters or time invalid' USING ERRCODE='23514';
    END IF;
    IF NOT (
        (NEW.state='pending' AND NEW.lease_owner IS NULL AND NEW.committed_revision='' AND
         NEW.verified_at IS NULL AND NEW.completed_at IS NULL) OR
        (NEW.state='claimed' AND NEW.lease_owner IS NOT NULL AND NEW.lease_until>NEW.updated_at AND
         NEW.attempts>0 AND NEW.committed_revision='' AND NEW.verified_at IS NULL AND NEW.completed_at IS NULL) OR
        (NEW.state='git-committed' AND NEW.operation='update' AND NEW.lease_owner IS NOT NULL AND
         NEW.lease_until>NEW.updated_at AND NEW.write_base_revision<>'' AND
         NEW.committed_revision<>'' AND NEW.committed_parent_revision=NEW.write_base_revision AND
         NEW.committed_at IS NOT NULL AND NEW.verified_at IS NULL AND NEW.completed_at IS NULL) OR
        (NEW.state='verified' AND NEW.lease_owner IS NULL AND NEW.write_base_revision<>'' AND
         NEW.verified_at IS NOT NULL AND NEW.verified_path_digest=NEW.adopted_content_digest AND
         NEW.provider_request<>'' AND NEW.completed_at=NEW.verified_at AND
         ((NEW.operation='update' AND NEW.committed_revision<>'' AND
           NEW.committed_parent_revision=NEW.write_base_revision AND NEW.committed_at IS NOT NULL) OR
          (NEW.operation='observe' AND NEW.committed_revision='' AND NEW.committed_at IS NULL))) OR
        (NEW.state IN ('failed','superseded') AND NEW.lease_owner IS NULL AND
         NEW.committed_revision='' AND NEW.verified_at IS NULL AND NEW.completed_at IS NOT NULL)
    ) THEN
        RAISE EXCEPTION 'cascade preflight runtime shape invalid' USING ERRCODE='23514';
    END IF;

    IF TG_OP='UPDATE' AND NEW.lease_owner IS NOT NULL AND
       (OLD.lease_owner IS NULL OR OLD.lease_until<=NEW.updated_at OR NEW.lease_epoch>OLD.lease_epoch) THEN
        IF NEW.lease_until<=NEW.updated_at OR NEW.lease_until>NEW.updated_at+interval '5 minutes' OR
           NOT EXISTS (
               SELECT 1 FROM public.runtime_readiness AS readiness
               WHERE readiness.runtime_kind='helm-protected-publisher'
                 AND readiness.scope_key='global' AND readiness.worker_id=NEW.lease_owner
                 AND readiness.contract_version=NEW.publisher_contract
                 AND readiness.config_digest=NEW.publisher_config_digest
                 AND readiness.identity=pg_catalog.jsonb_build_object(
                     'policyVersion',NEW.publisher_policy_version)
                 AND readiness.updated_at=readiness.observed_at
                 AND readiness.observed_at<=NEW.updated_at
                 AND readiness.observed_at>=NEW.updated_at-interval '5 minutes'
                 AND readiness.lease_until>NEW.updated_at
                 AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
                 AND readiness.lease_until<=NEW.updated_at+interval '5 minutes'
           ) OR
           (OLD.lease_epoch=0 AND NEW.lease_epoch=1 AND
            NOT public.helm_application_cascade_preflight_is_fresh(NEW.id)) THEN
            RAISE EXCEPTION 'cascade initial claim lacks current authority' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_preflight_guard
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_application_cascade_preflights
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_preflight();

CREATE FUNCTION public.validate_helm_application_cascade_adoption_receipt()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    candidate public.helm_application_cascade_preflights%ROWTYPE;
BEGIN
    IF TG_OP<>'INSERT' THEN
        RAISE EXCEPTION 'cascade adoption receipts are immutable' USING ERRCODE='23514';
    END IF;
    SELECT preflight.* INTO candidate
    FROM public.helm_application_cascade_preflights AS preflight
    WHERE preflight.id=NEW.cascade_preflight_id
    FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cascade adoption preflight is absent' USING ERRCODE='23514';
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(candidate.platform_binding_id::text,704215997));
    IF NEW.created_at>db_now OR NEW.created_at<db_now-interval '30 seconds' OR
       candidate.state NOT IN ('pending','claimed','git-committed') OR candidate.attempts>=30 OR
       candidate.next_attempt_at>NEW.created_at OR candidate.updated_at>NEW.created_at OR
       (candidate.lease_owner IS NOT NULL AND candidate.lease_until>NEW.created_at) OR
       candidate.publisher_contract<>NEW.publisher_contract OR
       candidate.publisher_policy_version<>NEW.policy_version OR
       candidate.original_publisher_config_digest<>NEW.original_config_digest OR
       candidate.publisher_config_digest<>NEW.previous_config_digest OR
       candidate.publisher_adoption_epoch+1<>NEW.adoption_epoch OR
       candidate.intent_digest<>NEW.intent_digest OR
       candidate.source_content_digest<>NEW.source_content_digest OR
       candidate.adopted_content_digest<>NEW.adopted_content_digest OR
       candidate.application_path<>NEW.application_path OR
       candidate.precondition<>NEW.precondition OR candidate.expected_etag<>NEW.expected_etag OR
       candidate.commit_trailer<>NEW.commit_trailer OR candidate.state<>NEW.recovery_state OR
       candidate.write_base_revision<>NEW.write_base_revision OR
       candidate.committed_revision<>NEW.committed_revision OR
       candidate.committed_parent_revision<>NEW.committed_parent_revision OR
       candidate.lease_epoch<>NEW.previous_lease_epoch OR
       NEW.adopted_lease_epoch<>candidate.lease_epoch+1 OR
       (candidate.lease_epoch=0 AND
        NOT public.helm_application_cascade_preflight_is_fresh(candidate.id)) OR
       NOT EXISTS (
           SELECT 1 FROM public.runtime_readiness AS readiness
           WHERE readiness.runtime_kind='helm-protected-publisher'
             AND readiness.scope_key='global' AND readiness.worker_id=NEW.adopted_by_worker
             AND readiness.worker_epoch=NEW.adopted_worker_epoch
             AND readiness.contract_version=NEW.publisher_contract
             AND readiness.config_digest=NEW.adopted_config_digest
             AND readiness.identity=pg_catalog.jsonb_build_object('policyVersion',NEW.policy_version)
             AND readiness.updated_at=readiness.observed_at
             AND readiness.observed_at<=NEW.created_at
             AND readiness.observed_at>=NEW.created_at-interval '5 minutes'
             AND readiness.lease_until>NEW.created_at
             AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
             AND readiness.lease_until<=NEW.created_at+interval '5 minutes'
       ) OR
       EXISTS (
           SELECT 1 FROM public.helm_protected_payload_intents AS held
           WHERE held.platform_binding_id=candidate.platform_binding_id
             AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at
       ) OR
       EXISTS (
           SELECT 1 FROM public.helm_protected_application_intents AS held
           WHERE held.platform_binding_id=candidate.platform_binding_id
             AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at
       ) OR
       EXISTS (
           SELECT 1 FROM public.helm_application_cascade_preflights AS held
           WHERE held.platform_binding_id=candidate.platform_binding_id AND held.id<>candidate.id
             AND held.lease_owner IS NOT NULL AND held.lease_until>NEW.created_at
       ) THEN
        RAISE EXCEPTION 'cascade adoption receipt lacks exact recoverable authority' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER helm_application_cascade_adoption_receipt_guard
BEFORE INSERT OR UPDATE OR DELETE ON public.helm_application_cascade_adoption_receipts
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_adoption_receipt();

CREATE FUNCTION public.validate_helm_application_cascade_adoption_postimage()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM public.helm_application_cascade_preflights AS preflight
        WHERE preflight.id=NEW.cascade_preflight_id
          AND preflight.publisher_contract=NEW.publisher_contract
          AND preflight.publisher_policy_version=NEW.policy_version
          AND preflight.original_publisher_config_digest=NEW.original_config_digest
          AND preflight.publisher_config_digest=NEW.adopted_config_digest
          AND preflight.publisher_adoption_epoch=NEW.adoption_epoch
          AND preflight.intent_digest=NEW.intent_digest
          AND preflight.source_content_digest=NEW.source_content_digest
          AND preflight.adopted_content_digest=NEW.adopted_content_digest
          AND preflight.application_path=NEW.application_path
          AND preflight.precondition=NEW.precondition
          AND preflight.expected_etag=NEW.expected_etag
          AND preflight.commit_trailer=NEW.commit_trailer
          AND preflight.state=CASE WHEN NEW.recovery_state='pending' THEN 'claimed' ELSE NEW.recovery_state END
          AND preflight.write_base_revision=NEW.write_base_revision
          AND preflight.committed_revision=NEW.committed_revision
          AND preflight.committed_parent_revision=NEW.committed_parent_revision
          AND preflight.lease_owner=NEW.adopted_by_worker
          AND preflight.lease_epoch=NEW.adopted_lease_epoch
          AND preflight.updated_at=NEW.created_at
    ) THEN
        RAISE EXCEPTION 'cascade adoption postimage is not exact' USING ERRCODE='23514';
    END IF;
    RETURN NULL;
END;
$function$;

CREATE CONSTRAINT TRIGGER helm_application_cascade_adoption_postimage
AFTER INSERT ON public.helm_application_cascade_adoption_receipts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION public.validate_helm_application_cascade_adoption_postimage();

CREATE FUNCTION public.adopt_helm_application_cascade_preflight(
    receipt_id uuid,adopting_worker text,adopting_worker_epoch bigint,
    adopting_publisher_contract text,adopting_policy_version text,
    adopting_config_digest text,lease_milliseconds bigint
)
RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    candidate public.helm_application_cascade_preflights%ROWTYPE;
    affected bigint;
BEGIN
    IF receipt_id IS NULL OR
       adopting_worker !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$' OR
       length(adopting_worker) NOT BETWEEN 16 AND 128 OR adopting_worker_epoch<1 OR
       adopting_publisher_contract<>'helm-protected-publisher.v1' OR
       adopting_policy_version<>'helm-protected-git.v1' OR
       adopting_config_digest !~ '^sha256:[0-9a-f]{64}$' OR
       lease_milliseconds<15000 OR lease_milliseconds>300000 THEN
        RAISE EXCEPTION 'invalid cascade publisher adoption request' USING ERRCODE='23514';
    END IF;

    SELECT preflight.* INTO candidate
    FROM public.helm_application_cascade_preflights AS preflight
    WHERE preflight.state IN ('pending','claimed','git-committed')
      AND preflight.next_attempt_at<=db_now AND preflight.updated_at<=db_now
      AND preflight.attempts<30
      AND (preflight.lease_owner IS NULL OR preflight.lease_until<=db_now)
      AND preflight.publisher_contract=adopting_publisher_contract
      AND preflight.publisher_config_digest<>adopting_config_digest
      AND (preflight.lease_epoch>0 OR
           public.helm_application_cascade_preflight_is_fresh(preflight.id))
      AND EXISTS (
          SELECT 1 FROM public.runtime_readiness AS readiness
          WHERE readiness.runtime_kind='helm-protected-publisher'
            AND readiness.scope_key='global' AND readiness.worker_id=adopting_worker
            AND readiness.worker_epoch=adopting_worker_epoch
            AND readiness.contract_version=adopting_publisher_contract
            AND readiness.config_digest=adopting_config_digest
            AND readiness.identity=pg_catalog.jsonb_build_object('policyVersion',adopting_policy_version)
            AND readiness.updated_at=readiness.observed_at
            AND readiness.observed_at<=db_now AND readiness.observed_at>=db_now-interval '5 minutes'
            AND readiness.lease_until>db_now
            AND readiness.lease_until<=readiness.observed_at+interval '5 minutes'
            AND readiness.lease_until<=db_now+interval '5 minutes'
      )
      AND NOT EXISTS (
          SELECT 1 FROM public.helm_protected_payload_intents AS held
          WHERE held.platform_binding_id=preflight.platform_binding_id
            AND held.lease_owner IS NOT NULL AND held.lease_until>db_now)
      AND NOT EXISTS (
          SELECT 1 FROM public.helm_protected_application_intents AS held
          WHERE held.platform_binding_id=preflight.platform_binding_id
            AND held.lease_owner IS NOT NULL AND held.lease_until>db_now)
      AND NOT EXISTS (
          SELECT 1 FROM public.helm_application_cascade_preflights AS held
          WHERE held.platform_binding_id=preflight.platform_binding_id AND held.id<>preflight.id
            AND held.lease_owner IS NOT NULL AND held.lease_until>db_now)
    ORDER BY preflight.next_attempt_at,preflight.created_at,preflight.id
    FOR UPDATE OF preflight SKIP LOCKED
    LIMIT 1;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended(candidate.platform_binding_id::text,704215997));
    INSERT INTO public.helm_application_cascade_adoption_receipts(
        id,cascade_preflight_id,adoption_epoch,publisher_contract,policy_version,
        original_config_digest,previous_config_digest,adopted_config_digest,
        intent_digest,source_content_digest,adopted_content_digest,application_path,
        precondition,expected_etag,commit_trailer,recovery_state,write_base_revision,
        committed_revision,committed_parent_revision,previous_lease_epoch,
        adopted_lease_epoch,adopted_by_worker,adopted_worker_epoch,created_at
    ) VALUES (
        receipt_id,candidate.id,candidate.publisher_adoption_epoch+1,
        candidate.publisher_contract,candidate.publisher_policy_version,
        candidate.original_publisher_config_digest,candidate.publisher_config_digest,
        adopting_config_digest,candidate.intent_digest,candidate.source_content_digest,
        candidate.adopted_content_digest,candidate.application_path,candidate.precondition,
        candidate.expected_etag,candidate.commit_trailer,candidate.state,
        candidate.write_base_revision,candidate.committed_revision,
        candidate.committed_parent_revision,candidate.lease_epoch,candidate.lease_epoch+1,
        adopting_worker,adopting_worker_epoch,db_now
    );

    UPDATE public.helm_application_cascade_preflights AS preflight
       SET publisher_config_digest=adopting_config_digest,
           publisher_adoption_epoch=preflight.publisher_adoption_epoch+1,
           state=CASE WHEN preflight.state='pending' THEN 'claimed' ELSE preflight.state END,
           lease_owner=adopting_worker,lease_epoch=preflight.lease_epoch+1,
           lease_until=db_now+(lease_milliseconds*interval '1 millisecond'),
           attempts=LEAST(preflight.attempts+1,30),updated_at=db_now,
           prerequisite_epoch=preflight.prerequisite_epoch+1
     WHERE preflight.id=candidate.id
       AND preflight.publisher_config_digest=candidate.publisher_config_digest
       AND preflight.publisher_adoption_epoch=candidate.publisher_adoption_epoch
       AND preflight.lease_epoch=candidate.lease_epoch
       AND (preflight.lease_owner IS NULL OR preflight.lease_until<=db_now)
       AND preflight.state=candidate.state;
    GET DIAGNOSTICS affected=ROW_COUNT;
    IF affected<>1 THEN
        RAISE EXCEPTION 'cascade publisher adoption lost exact lock' USING ERRCODE='40001';
    END IF;
    RETURN candidate.id;
END;
$function$;

REVOKE ALL ON FUNCTION public.adopt_helm_application_cascade_preflight(
    uuid,text,bigint,text,text,text,bigint) FROM PUBLIC;

CREATE FUNCTION public.helm_application_cascade_observation_is_exact(
    candidate_id uuid,observer_config_digest text,authority_time timestamptz
)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT EXISTS (
    SELECT 1
    FROM public.helm_application_cascade_preflights AS preflight
    JOIN public.helm_application_cascade_observer_activations AS activation
      ON activation.platform_binding_id=preflight.platform_binding_id
     AND activation.activation_epoch=(
         SELECT MAX(current.activation_epoch)
         FROM public.helm_application_cascade_observer_activations AS current
         WHERE current.platform_binding_id=preflight.platform_binding_id)
     AND activation.publisher_config_digest=observer_config_digest
    JOIN public.helm_application_cascade_observation_jobs AS job
      ON job.cascade_preflight_id=preflight.id
     AND job.platform_binding_id=activation.platform_binding_id
     AND job.activation_epoch=activation.activation_epoch
     AND job.publisher_contract=activation.publisher_contract
     AND job.publisher_policy_version=activation.publisher_policy_version
     AND job.publisher_config_digest=activation.publisher_config_digest
     AND job.state='verified' AND job.superseded_at IS NULL
    JOIN LATERAL (
        SELECT value.*
        FROM public.helm_application_cascade_receipts AS value
        WHERE value.cascade_preflight_id=preflight.id
        ORDER BY value.observation_epoch DESC
        LIMIT 1
    ) AS receipt ON true
    JOIN public.helm_protected_application_intents AS base
      ON base.id=preflight.base_application_intent_id
     AND base.state='verified' AND base.action='publish'
    JOIN public.helm_protected_payload_intents AS child_payload
      ON child_payload.id=base.payload_intent_id
     AND child_payload.state='verified' AND child_payload.action='publish'
    JOIN public.git_repository_bindings AS platform
      ON platform.id=preflight.platform_binding_id
     AND platform.kind='platform' AND platform.credential_mode='github-app'
     AND platform.cluster_id=preflight.cluster_id
     AND platform.target_ref=preflight.platform_target_ref
     AND platform.target_head_revision=receipt.provider_head
    JOIN public.runtime_readiness AS publisher
      ON publisher.runtime_kind='helm-protected-publisher' AND publisher.scope_key='global'
     AND publisher.worker_id=activation.publisher_worker_id
     AND publisher.worker_epoch=activation.publisher_worker_epoch
     AND publisher.contract_version=activation.publisher_contract
     AND publisher.config_digest=activation.publisher_config_digest
     AND publisher.identity=pg_catalog.jsonb_build_object(
         'policyVersion',activation.publisher_policy_version)
     AND publisher.started_at=activation.publisher_started_at
     AND publisher.updated_at=publisher.observed_at
     AND publisher.observed_at>=activation.publisher_readiness_observed_at
     AND publisher.lease_until>=activation.publisher_readiness_lease_until
     AND publisher.observed_at<=authority_time
     AND publisher.observed_at>=authority_time-interval '5 minutes'
     AND publisher.lease_until>authority_time
     AND publisher.lease_until<=publisher.observed_at+interval '5 minutes'
     AND publisher.lease_until<=authority_time+interval '5 minutes'
    JOIN public.runtime_readiness AS argo
      ON argo.runtime_kind='argo-desired-state' AND argo.scope_key='global'
     AND argo.platform_binding_id=activation.platform_binding_id
     AND argo.worker_id=receipt.argo_worker_id
     AND argo.worker_epoch=receipt.argo_worker_epoch
     AND argo.contract_version=receipt.argo_contract
     AND argo.config_digest=receipt.argo_config_digest
     AND argo.identity=activation.argo_identity
     AND argo.started_at=receipt.argo_started_at
     AND argo.updated_at=argo.observed_at
     AND argo.observed_at>=receipt.argo_readiness_observed_at
     AND argo.lease_until>=receipt.argo_readiness_lease_until
     AND argo.observed_at<=authority_time
     AND argo.observed_at>=authority_time-interval '5 minutes'
     AND argo.lease_until>authority_time
     AND argo.lease_until<=argo.observed_at+interval '5 minutes'
     AND argo.lease_until<=authority_time+interval '5 minutes'
    WHERE preflight.id=candidate_id
      AND preflight.state='verified'
      AND preflight.verified_path_digest=preflight.adopted_content_digest
      AND public.helm_application_cascade_preflight_is_fresh(preflight.id)
      AND receipt.observer_activation_epoch=activation.activation_epoch
      AND receipt.publisher_contract=activation.publisher_contract
      AND receipt.publisher_policy_version=activation.publisher_policy_version
      AND receipt.publisher_config_digest=activation.publisher_config_digest
      AND receipt.worker_id=activation.publisher_worker_id
      AND receipt.worker_epoch=activation.publisher_worker_epoch
      AND receipt.argo_contract=activation.argo_contract
      AND receipt.argo_config_digest=activation.argo_config_digest
      AND receipt.argo_worker_id=activation.argo_worker_id
      AND receipt.argo_worker_epoch=activation.argo_worker_epoch
      AND receipt.argo_started_at=activation.argo_started_at
      AND activation.argo_identity->>'clusterId'=preflight.cluster_id::text
      AND activation.argo_identity->>'argoNamespace'=preflight.argo_namespace
      AND activation.argo_identity->>'rootApplicationName'='kuberploy-platform-root'
      AND receipt.delete_intent_id=preflight.delete_intent_id
      AND receipt.release_revision_id=preflight.release_revision_id
      AND receipt.payload_intent_id=preflight.payload_intent_id
      AND receipt.base_application_intent_id=preflight.base_application_intent_id
      AND receipt.project_id=preflight.project_id
      AND receipt.environment_id=preflight.environment_id
      AND receipt.application_id=preflight.application_id
      AND receipt.cluster_id=preflight.cluster_id
      AND receipt.application_path=preflight.application_path
      AND receipt.source_content_digest=preflight.source_content_digest
      AND receipt.adopted_content_digest=preflight.adopted_content_digest
      AND receipt.adoption_revision=CASE WHEN preflight.operation='update'
          THEN preflight.committed_revision ELSE preflight.write_base_revision END
      AND receipt.adoption_parent_revision=CASE WHEN preflight.operation='update'
          THEN preflight.committed_parent_revision ELSE preflight.write_base_revision END
      AND receipt.root_observed_revision=receipt.provider_head
      AND receipt.root_sync_status='Synced'
      AND receipt.root_spec_digest=
          public.helm_application_cascade_expected_root_spec_digest(preflight.id)
      AND receipt.child_spec_digest=
          public.helm_application_cascade_expected_child_spec_digest(preflight.id)
      AND receipt.child_release_revision_id=base.release_revision_id
      AND receipt.child_release_revision_id=child_payload.release_revision_id
      AND receipt.child_payload_revision=base.payload_revision
      AND receipt.child_payload_revision=child_payload.committed_revision
      AND receipt.child_payload_path=base.payload_path
      AND receipt.child_payload_path=child_payload.path
      AND receipt.child_payload_digest=child_payload.content_digest
      AND receipt.finalizer_digest=
          'sha256:4a33b93a0b2d591421d38cedd7660abbfffcb3fc10be2cbbe9e4d8525ce17f48'
      AND receipt.observed_at>=preflight.verified_at
      AND receipt.observed_at<=authority_time
      AND receipt.observed_at>=authority_time-interval '5 minutes'
);
$function$;

CREATE FUNCTION public.helm_application_cascade_is_exact(
    candidate_id uuid,observer_config_digest text,authority_time timestamptz
)
RETURNS boolean
LANGUAGE sql
STABLE
SET search_path=pg_catalog,pg_temp
AS $function$
SELECT EXISTS (
    SELECT 1
    FROM public.helm_protected_application_intents AS intent
    JOIN public.helm_application_cascade_preflights AS preflight
      ON preflight.id=intent.cascade_receipt_id
     AND preflight.release_revision_id=intent.release_revision_id
     AND preflight.payload_intent_id=intent.payload_intent_id
     AND preflight.release_generation=intent.release_generation
     AND preflight.project_id=intent.project_id
     AND preflight.environment_id=intent.environment_id
     AND preflight.application_id=intent.application_id
     AND preflight.platform_binding_id=intent.platform_binding_id
     AND preflight.environment_binding_id=intent.environment_binding_id
     AND preflight.cluster_id=intent.cluster_id
     AND preflight.platform_target_ref=intent.platform_target_ref
     AND preflight.application_path=intent.application_path
     AND intent.expected_etag='"'||preflight.adopted_content_digest||'"'
    JOIN LATERAL (
        SELECT receipt.publisher_config_digest
        FROM public.helm_application_cascade_receipts AS receipt
        WHERE receipt.cascade_preflight_id=preflight.id
        ORDER BY receipt.observation_epoch DESC
        LIMIT 1
    ) AS latest ON true
    WHERE intent.id=candidate_id
      AND intent.action='delete' AND intent.cascade_required
      AND intent.cascade_contract='helm-application-cascade-preflight.v1'
      AND latest.publisher_config_digest=observer_config_digest
      AND public.helm_application_cascade_observation_is_exact(
          preflight.id,observer_config_digest,authority_time)
);
$function$;

ALTER FUNCTION public.helm_application_cascade_preflight_is_fresh(uuid)
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.helm_application_cascade_expected_child_spec_digest(uuid)
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.helm_application_cascade_expected_root_spec_digest(uuid)
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.helm_application_cascade_observation_is_exact(uuid,text,timestamptz)
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.helm_application_cascade_is_exact(uuid,text,timestamptz)
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_preflight()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_adoption_receipt()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_adoption_postimage()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_observation_job()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_receipt()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_protected_cascade_lane()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_gate()
SET search_path=pg_catalog,pg_temp;
ALTER FUNCTION public.validate_helm_application_cascade_exact_gate()
SET search_path=pg_catalog,pg_temp;
