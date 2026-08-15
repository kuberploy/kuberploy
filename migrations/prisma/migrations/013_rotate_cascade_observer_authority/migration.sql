-- Keep the cascade observer activation monotonic across independently renewed
-- publisher and Argo readiness leases. Production Argo intentionally acquires
-- a new readiness epoch after a transient prerequisite loss without restarting
-- its process; that renewal must not strand the co-resident protected Git lane.
CREATE OR REPLACE FUNCTION public.validate_helm_application_cascade_observer_activation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path=pg_catalog,pg_temp
AS $function$
DECLARE
    db_now timestamptz := pg_catalog.clock_timestamp();
    publisher_readiness public.runtime_readiness%ROWTYPE;
    argo_readiness public.runtime_readiness%ROWTYPE;
    current_activation public.helm_application_cascade_observer_activations%ROWTYPE;
    publisher_same boolean := false;
    publisher_advanced boolean := false;
    argo_same boolean := false;
    argo_advanced boolean := false;
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
        publisher_same :=
            NEW.publisher_worker_id=current_activation.publisher_worker_id AND
            NEW.publisher_worker_epoch=current_activation.publisher_worker_epoch AND
            NEW.publisher_contract=current_activation.publisher_contract AND
            NEW.publisher_policy_version=current_activation.publisher_policy_version AND
            NEW.publisher_config_digest=current_activation.publisher_config_digest AND
            publisher_readiness.started_at=current_activation.publisher_started_at;
        publisher_advanced := (
            NEW.publisher_worker_id=current_activation.publisher_worker_id AND
            NEW.publisher_worker_epoch>current_activation.publisher_worker_epoch AND
            NEW.publisher_contract=current_activation.publisher_contract AND
            NEW.publisher_policy_version=current_activation.publisher_policy_version AND
            NEW.publisher_config_digest=current_activation.publisher_config_digest AND
            publisher_readiness.started_at=current_activation.publisher_started_at
        ) OR (
            publisher_readiness.started_at>current_activation.publisher_started_at AND
            (NEW.publisher_worker_id<>current_activation.publisher_worker_id OR
             NEW.publisher_worker_epoch>current_activation.publisher_worker_epoch)
        );

        argo_same :=
            NEW.argo_worker_id=current_activation.argo_worker_id AND
            NEW.argo_worker_epoch=current_activation.argo_worker_epoch AND
            NEW.argo_contract=current_activation.argo_contract AND
            NEW.argo_config_digest=current_activation.argo_config_digest AND
            argo_readiness.identity=current_activation.argo_identity AND
            argo_readiness.started_at=current_activation.argo_started_at;
        argo_advanced := (
            NEW.argo_worker_id=current_activation.argo_worker_id AND
            NEW.argo_worker_epoch>current_activation.argo_worker_epoch AND
            NEW.argo_contract=current_activation.argo_contract AND
            NEW.argo_config_digest=current_activation.argo_config_digest AND
            argo_readiness.identity=current_activation.argo_identity AND
            argo_readiness.started_at=current_activation.argo_started_at
        ) OR (
            argo_readiness.started_at>current_activation.argo_started_at AND
            (NEW.argo_worker_id<>current_activation.argo_worker_id OR
             NEW.argo_worker_epoch>current_activation.argo_worker_epoch)
        );

        IF NOT (publisher_same OR publisher_advanced) OR
           NOT (argo_same OR argo_advanced) OR
           NOT (publisher_advanced OR argo_advanced) THEN
            RAISE EXCEPTION 'cascade observer activation authority did not advance monotonically'
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
