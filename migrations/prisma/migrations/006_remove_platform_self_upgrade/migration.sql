-- Platform upgrades are operator-owned Helm actions. Retire any pre-stable
-- in-app work before removing its mutable state table. Terminal operations and
-- audit events remain as immutable historical evidence.
LOCK TABLE public.operations IN ACCESS EXCLUSIVE MODE;
LOCK TABLE public.outbox IN ACCESS EXCLUSIVE MODE;
LOCK TABLE public.platform_upgrades IN ACCESS EXCLUSIVE MODE;

DELETE FROM public.outbox AS event
USING public.operations AS operation
WHERE event.operation_id = operation.id
  AND operation.kind = 'platform.upgrade';

UPDATE public.operations
SET status = 'cancelled',
    progress = jsonb_build_array(jsonb_build_object(
      'name', 'platform-upgrade',
      'status', 'cancelled',
      'detail', 'Feature removed; use the cluster-admin Helm workflow.',
      'finishedAt', now()
    )),
    problem = jsonb_build_object(
      'code', 'FeatureRemoved',
      'detail', 'Kuberploy platform upgrades are performed by a cluster administrator with Helm.'
    ),
    lease_owner = NULL,
    lease_until = NULL,
    updated_at = now(),
    finished_at = COALESCE(finished_at, now())
WHERE kind = 'platform.upgrade'
  AND status IN ('queued', 'running');

DROP TABLE public.platform_upgrades;
