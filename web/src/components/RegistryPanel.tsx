import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type {
  Application,
  ApplicationRegistryTarget,
  Capability,
  Project,
  RegistryCleanupPlan,
  RegistryPolicy,
  RegistryPolicyInput,
  RegistryTarget,
} from "../api/types";
import { canonicalBuildRepository } from "../lib/buildAccess";
import { formatDate, titleCase } from "../lib/format";
import {
  hasRegistryApplicationCapability,
  hasRegistryPlatformCapability,
} from "../lib/registryAccess";
import { Icon } from "./Icon";
import {
  Select,
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  MutedCopy,
  Notice,
  Skeleton,
  StatusPill,
} from "./ui";

type RegistryPanelProps = {
  application: Application;
  project?: Project;
  capabilities: Capability[];
  featureEnabled: boolean;
  managedFeatureEnabled: boolean;
  humanSession: boolean;
};

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

function shortDigest(value: string) {
  if (value.length <= 28) return value;
  return `${value.slice(0, 19)}…${value.slice(-8)}`;
}

function formatBytes(value: number) {
  if (!Number.isFinite(value) || value < 0) return "Not reported";
  if (value < 1024) return `${value} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let result = value;
  let unit = -1;
  do {
    result /= 1024;
    unit++;
  } while (result >= 1024 && unit < units.length - 1);
  return `${result.toFixed(result >= 10 ? 0 : 1)} ${units[unit]}`;
}

type PolicyDraft = Required<RegistryPolicyInput>;

const defaultPolicy: PolicyDraft = {
  repository: "",
  keepLastSuccessful: 10,
  minimumSafetyAgeSeconds: 86_400,
  cacheKeepGenerations: 2,
  cacheUnusedExpirySeconds: 604_800,
  cacheByteQuota: 10_737_418_240,
};

function policyDraft(
  policy?: RegistryPolicy,
  defaultRepository = "",
): PolicyDraft {
  if (!policy) return { ...defaultPolicy, repository: defaultRepository };
  return {
    repository: policy.repository,
    keepLastSuccessful: policy.keepLastSuccessful,
    minimumSafetyAgeSeconds: policy.minimumSafetyAgeSeconds,
    cacheKeepGenerations: policy.cacheKeepGenerations,
    cacheUnusedExpirySeconds: policy.cacheUnusedExpirySeconds,
    cacheByteQuota: policy.cacheByteQuota,
  };
}

function validatePolicy(draft: PolicyDraft) {
  const errors: Partial<Record<keyof PolicyDraft, string>> = {};
  if (!draft.repository.trim()) errors.repository = "Enter an OCI repository.";
  if (
    !Number.isInteger(draft.keepLastSuccessful) ||
    draft.keepLastSuccessful < 1 ||
    draft.keepLastSuccessful > 100
  )
    errors.keepLastSuccessful = "Keep between 1 and 100 successful releases.";
  if (
    !Number.isInteger(draft.minimumSafetyAgeSeconds) ||
    draft.minimumSafetyAgeSeconds < 60
  )
    errors.minimumSafetyAgeSeconds = "Use at least 60 seconds.";
  if (
    !Number.isInteger(draft.cacheKeepGenerations) ||
    draft.cacheKeepGenerations < 1 ||
    draft.cacheKeepGenerations > 20
  )
    errors.cacheKeepGenerations = "Keep between 1 and 20 cache generations.";
  if (
    !Number.isInteger(draft.cacheUnusedExpirySeconds) ||
    draft.cacheUnusedExpirySeconds < 60
  )
    errors.cacheUnusedExpirySeconds = "Use at least 60 seconds.";
  if (!Number.isInteger(draft.cacheByteQuota) || draft.cacheByteQuota < 1)
    errors.cacheByteQuota = "Enter a positive byte quota.";
  return errors;
}

function PolicyEditor({
  applicationId,
  target,
  policy,
  defaultRepository,
  onSaved,
}: {
  applicationId: string;
  target: RegistryTarget;
  policy?: RegistryPolicy;
  defaultRepository?: string;
  onSaved?: () => void;
}) {
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState(() =>
    policyDraft(policy, defaultRepository),
  );
  const [errors, setErrors] = useState<
    Partial<Record<keyof PolicyDraft, string>>
  >({});
  const saveAttempt = useRef<{ signature: string; key: string } | null>(null);
  const policyBaseline = useRef(
    JSON.stringify(policyDraft(policy, defaultRepository)),
  );
  const editorScope = JSON.stringify({
    applicationId,
    targetId: target.id,
    draft,
  });
  const editorScopeRef = useRef(editorScope);
  editorScopeRef.current = editorScope;
  useEffect(() => {
    const nextDraft = policyDraft(policy, defaultRepository);
    const nextBaseline = JSON.stringify(nextDraft);
    setDraft((current) => {
      if (JSON.stringify(current) !== policyBaseline.current) return current;
      policyBaseline.current = nextBaseline;
      return nextDraft;
    });
    setErrors({});
  }, [defaultRepository, policy]);
  const mutation = useMutation({
    mutationFn: ({
      input,
      idempotencyKey,
      applicationId: requestedApplicationId,
      targetId,
      editorScope: _editorScope,
    }: {
      input: RegistryPolicyInput;
      idempotencyKey: string;
      applicationId: string;
      targetId: string;
      editorScope: string;
    }) =>
      api.putRegistryPolicy(
        requestedApplicationId,
        targetId,
        input,
        idempotencyKey,
      ),
    retry: retryNetworkOnce,
    onSuccess: async (_value, input) => {
      const sameDraft = input.editorScope === editorScopeRef.current;
      if (sameDraft && saveAttempt.current?.key === input.idempotencyKey) {
        saveAttempt.current = null;
      }
      await queryClient.invalidateQueries({
        queryKey: ["application-registry", input.applicationId],
      });
      if (sameDraft) onSaved?.();
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    const nextErrors = validatePolicy(draft);
    setErrors(nextErrors);
    if (Object.keys(nextErrors).length > 0) return;
    const input = { ...draft, repository: draft.repository.trim() };
    const signature = JSON.stringify({
      applicationId,
      targetId: target.id,
      input,
    });
    const idempotencyKey =
      saveAttempt.current?.signature === signature
        ? saveAttempt.current.key
        : crypto.randomUUID();
    saveAttempt.current = { signature, key: idempotencyKey };
    mutation.mutate({
      input,
      applicationId,
      targetId: target.id,
      idempotencyKey,
      editorScope,
    });
  };
  const numberField = (
    key: Exclude<keyof PolicyDraft, "repository">,
    label: string,
    hint: string,
  ) => (
    <Field label={label} hint={hint} error={errors[key]} required>
      <input
        type="number"
        min={key === "keepLastSuccessful" ? 1 : 0}
        value={draft[key]}
        onChange={(event) =>
          setDraft((current) => ({
            ...current,
            [key]: Number(event.target.value),
          }))
        }
      />
    </Field>
  );
  return (
    <form
      className="grid gap-4 p-5 border border-line rounded-panel bg-surface-soft"
      onSubmit={submit}
    >
      <Field
        label="Release repository"
        hint="Source builds require the canonical per-project and per-App repository shown by default."
        error={errors.repository}
        required
      >
        <input
          value={draft.repository}
          placeholder={`${target.repositoryPrefix}/service`}
          onChange={(event) =>
            setDraft((current) => ({
              ...current,
              repository: event.target.value,
            }))
          }
        />
      </Field>
      <div className="grid grid-cols-[repeat(2,_minmax(0,_1fr))] gap-4 to-900:grid-cols-[1fr]">
        {numberField(
          "keepLastSuccessful",
          "Keep successful releases",
          "Default: 10; allowed: 1–100.",
        )}
        {numberField(
          "minimumSafetyAgeSeconds",
          "Minimum safety age (seconds)",
          "Artifacts newer than this remain protected.",
        )}
        {numberField(
          "cacheKeepGenerations",
          "Cache generations",
          target.mode === "external"
            ? "Operator-managed metadata on this external target."
            : "Default: 2; allowed: 1–20.",
        )}
        {numberField(
          "cacheUnusedExpirySeconds",
          "Unused cache expiry (seconds)",
          target.mode === "external"
            ? "Operator-managed metadata on this external target."
            : "Default: 604800 (7 days).",
        )}
        {numberField(
          "cacheByteQuota",
          "Cache quota (bytes)",
          target.mode === "external"
            ? "Operator-managed metadata on this external target."
            : "Default: 10737418240 (10 GiB).",
        )}
      </div>
      {mutation.error ? <ErrorPanel error={mutation.error} /> : null}
      <Button type="submit" busy={mutation.isPending}>
        <Icon name="check" /> {policy ? "Save policy" : "Attach target"}
      </Button>
    </form>
  );
}

function CleanupPanel({
  application,
  target,
  capabilities,
  project,
  humanSession,
}: {
  application: Application;
  target: ApplicationRegistryTarget;
  capabilities: Capability[];
  project?: Project;
  humanSession: boolean;
}) {
  const queryClient = useQueryClient();
  const canPreview = hasRegistryApplicationCapability(
    capabilities,
    "registry-cleanup:preview",
    application,
    project,
  );
  const canExecute = hasRegistryApplicationCapability(
    capabilities,
    "registry-cleanup:execute",
    application,
    project,
  );
  const [planId, setPlanId] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const previewAttempt = useRef<{ signature: string; key: string } | null>(
    null,
  );
  const executeAttempt = useRef<{ signature: string; key: string } | null>(
    null,
  );
  const scopeKey = `${application.id}\u0000${target.target.id}`;
  const activeScopeKey = useRef(scopeKey);
  activeScopeKey.current = scopeKey;
  const status = useQuery({
    queryKey: ["registry-cleanup-plan", planId],
    queryFn: () => api.registryCleanupPlan(planId),
    enabled: Boolean(planId),
    retry: false,
    refetchInterval: (query) =>
      query.state.data?.state === "executing" ? 5_000 : false,
  });
  const preview = useMutation({
    mutationFn: ({
      applicationId: requestedApplicationId,
      targetId,
      key,
    }: {
      scope: string;
      applicationId: string;
      targetId: string;
      key: string;
    }) => api.previewRegistryCleanup(requestedApplicationId, targetId, key),
    retry: retryNetworkOnce,
    onSuccess: (plan, input) => {
      if (input.scope !== activeScopeKey.current) return;
      if (previewAttempt.current?.key === input.key) {
        previewAttempt.current = null;
      }
      setPlanId(plan.id);
      setConfirmation("");
      queryClient.setQueryData(["registry-cleanup-plan", plan.id], plan);
    },
  });
  const execute = useMutation({
    mutationFn: ({
      plan,
      confirmation: submittedConfirmation,
      idempotencyKey,
    }: {
      plan: RegistryCleanupPlan;
      scope: string;
      confirmation: string;
      applicationId: string;
      targetId: string;
      idempotencyKey: string;
    }) =>
      api.executeRegistryCleanup(
        plan.id,
        submittedConfirmation,
        idempotencyKey,
      ),
    retry: retryNetworkOnce,
    onSuccess: (plan, input) => {
      if (input.scope !== activeScopeKey.current) return;
      if (executeAttempt.current?.key === input.idempotencyKey) {
        executeAttempt.current = null;
      }
      queryClient.setQueryData(["registry-cleanup-plan", plan.id], plan);
      void queryClient.invalidateQueries({
        queryKey: ["application-registry", input.applicationId],
      });
    },
  });
  const createPreview = () => {
    const signature = JSON.stringify({
      applicationId: application.id,
      targetId: target.target.id,
    });
    const idempotencyKey =
      previewAttempt.current?.signature === signature
        ? previewAttempt.current.key
        : crypto.randomUUID();
    previewAttempt.current = { signature, key: idempotencyKey };
    preview.mutate({
      scope: scopeKey,
      applicationId: application.id,
      targetId: target.target.id,
      key: idempotencyKey,
    });
  };
  const executePlan = (nextPlan: RegistryCleanupPlan) => {
    const signature = JSON.stringify({
      planId: nextPlan.id,
      confirmation,
    });
    const idempotencyKey =
      executeAttempt.current?.signature === signature
        ? executeAttempt.current.key
        : crypto.randomUUID();
    executeAttempt.current = { signature, key: idempotencyKey };
    execute.mutate({
      plan: nextPlan,
      scope: scopeKey,
      confirmation,
      applicationId: application.id,
      targetId: target.target.id,
      idempotencyKey,
    });
  };
  const plan = status.data ?? preview.data;
  if (!canPreview || !humanSession) return null;
  return (
    <div className="grid gap-4 pt-6 border-t border-t-line">
      <div className="flex items-start justify-between gap-5 [&_h3]:mt-1 [&_h3]:mx-0 [&_h3]:mb-0 to-580:items-start to-580:flex-col">
        <div>
          <Eyebrow>Managed lifecycle</Eyebrow>
          <h3>Fail-closed cleanup preview</h3>
        </div>
        <Button
          variant="secondary"
          busy={preview.isPending}
          disabled={execute.isPending}
          onClick={createPreview}
        >
          <Icon name="refresh" /> Create preview
        </Button>
      </div>
      <MutedCopy>
        Preview requires fresh, complete inventory, catalog, Git, runtime, and
        operation observations. Execution revalidates the same authorities.
      </MutedCopy>
      {preview.error ? <ErrorPanel error={preview.error} /> : null}
      {status.error ? (
        <ErrorPanel
          error={status.error}
          onRetry={() => void status.refetch()}
        />
      ) : null}
      {plan ? (
        <div className="grid gap-4 p-5 border border-line-strong rounded-panel bg-surface-soft">
          <div className="flex items-start justify-between gap-5 [&>div]:grid [&>div]:gap-1.5 [&>div]:min-w-0 [&_code]:overflow-hidden [&_code]:text-ellipsis to-580:items-start to-580:flex-col">
            <div>
              <small>Plan ID</small>
              <code>{plan.id}</code>
            </div>
            <StatusPill value={plan.state} />
          </div>
          <dl className="grid grid-cols-[repeat(3,_minmax(0,_1fr))] gap-px m-0 overflow-hidden border border-line rounded-panel bg-line [&>div]:grid [&>div]:gap-1.5 [&>div]:min-w-0 [&>div]:py-4 [&>div]:px-4 [&>div]:bg-surface [&_dt]:text-[0.76rem] [&_dd]:m-0 [&_dd]:font-semibold [&_dd]:break-words to-900:grid-cols-[repeat(2,_minmax(0,_1fr))] to-580:grid-cols-[1fr]">
            <div>
              <dt>Protected manifests</dt>
              <dd>{plan.summary.protectedManifests}</dd>
            </div>
            <div>
              <dt>Eligible manifests</dt>
              <dd>{plan.summary.deletedManifests}</dd>
            </div>
            <div>
              <dt>Unreachable blobs</dt>
              <dd>{plan.summary.garbageCollectBlobs}</dd>
            </div>
            <div>
              <dt>Estimated reclaim</dt>
              <dd>{formatBytes(plan.summary.estimatedBytes)}</dd>
            </div>
          </dl>
          {plan.failure ? (
            <Notice tone="error" role="alert">
              <div>
                <strong>Cleanup failed</strong>
                <p>{plan.failure}</p>
              </div>
            </Notice>
          ) : null}
          {plan.state === "preview" && canExecute ? (
            <form
              className="grid gap-4 pt-4 border-t border-t-line [&_[data-slot='button']]:justify-self-start"
              onSubmit={(event) => {
                event.preventDefault();
                if (confirmation === plan.id) executePlan(plan);
              }}
            >
              <Field
                label="Confirm exact plan ID"
                hint="Execution is accepted only when this value exactly matches the preview plan ID."
                error={
                  confirmation && confirmation !== plan.id
                    ? "The confirmation does not match this plan."
                    : undefined
                }
                required
              >
                <input
                  value={confirmation}
                  autoComplete="off"
                  onChange={(event) => setConfirmation(event.target.value)}
                />
              </Field>
              {execute.error ? <ErrorPanel error={execute.error} /> : null}
              <Button
                type="submit"
                variant="danger"
                busy={execute.isPending}
                disabled={confirmation !== plan.id || preview.isPending}
              >
                Execute managed cleanup
              </Button>
            </form>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function InventoryTable({ target }: { target: ApplicationRegistryTarget }) {
  return (
    <>
      <div className="flex items-start justify-between gap-5 [&_h3]:mt-1 [&_h3]:mx-0 [&_h3]:mb-0 to-580:items-start to-580:flex-col">
        <div>
          <Eyebrow>Release inventory</Eyebrow>
          <h3>Availability</h3>
        </div>
        {target.releasesTruncated ? (
          <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
            Most recent 50
          </span>
        ) : null}
      </div>
      {target.releases.length === 0 ? (
        <EmptyState
          compact
          icon="deploy"
          title="No release inventory"
          description="No release records have been observed for this service and target."
        />
      ) : (
        <div className="overflow-x-auto mt-4 border border-line rounded-lg [&>table]:w-full [&>table]:border-collapse">
          <table className="[&_code]:whitespace-nowrap">
            <thead>
              <tr>
                <th>Digest</th>
                <th>Availability</th>
                <th>Succeeded</th>
              </tr>
            </thead>
            <tbody>
              {target.releases.map((release) => (
                <tr key={release.id}>
                  <td>
                    <code title={release.rootDigest}>
                      {shortDigest(release.rootDigest)}
                    </code>
                  </td>
                  <td>
                    <StatusPill value={release.availability} />
                  </td>
                  <td>
                    {formatDate(release.succeededAt ?? release.createdAt)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="flex items-start justify-between gap-5 [&_h3]:mt-1 [&_h3]:mx-0 [&_h3]:mb-0 to-580:items-start to-580:flex-col mt-1">
        <div>
          <Eyebrow>Build cache</Eyebrow>
          <h3>Generation availability</h3>
        </div>
        {target.cacheGenerationsTruncated ? (
          <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
            Most recent 50
          </span>
        ) : null}
      </div>
      {target.cacheGenerations.length === 0 ? (
        <EmptyState
          compact
          icon="code"
          title="No cache generations"
          description="No build-cache generation metadata has been observed."
        />
      ) : (
        <div className="overflow-x-auto mt-4 border border-line rounded-lg [&>table]:w-full [&>table]:border-collapse">
          <table className="[&_code]:whitespace-nowrap">
            <thead>
              <tr>
                <th>Generation</th>
                <th>Platform / lane</th>
                <th>Availability</th>
                <th>Size</th>
                <th>Last used</th>
              </tr>
            </thead>
            <tbody>
              {target.cacheGenerations.map((cache) => (
                <tr key={cache.id}>
                  <td>{cache.generation}</td>
                  <td>
                    {cache.platformSet} · {cache.trustLane}
                  </td>
                  <td>
                    <StatusPill value={cache.state} />
                  </td>
                  <td>{formatBytes(cache.sizeBytes)}</td>
                  <td>{formatDate(cache.lastUsedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function RegistryTargetCard({
  application,
  item,
  project,
  capabilities,
  managedFeatureEnabled,
  humanSession,
}: {
  application: Application;
  item: ApplicationRegistryTarget;
  project?: Project;
  capabilities: Capability[];
  managedFeatureEnabled: boolean;
  humanSession: boolean;
}) {
  const [editingPolicy, setEditingPolicy] = useState(false);
  const canWritePolicy = hasRegistryApplicationCapability(
    capabilities,
    "registry-policies:write",
    application,
    project,
  );
  return (
    <Card className="grid gap-6">
      <CardHeader>
        <div>
          <Eyebrow>{item.target.mode} target</Eyebrow>
          <h2>{item.target.name}</h2>
          <p>
            <code>{item.target.endpoint}</code> · {item.policy.repository}
          </p>
        </div>
        <StatusPill
          value={item.inventory?.complete ? "observed" : "unavailable"}
          label={
            item.inventory?.complete ? "Inventory complete" : "Unavailable"
          }
        />
      </CardHeader>
      <dl className="grid grid-cols-[repeat(3,_minmax(0,_1fr))] gap-px m-0 overflow-hidden border border-line rounded-panel bg-line [&>div]:grid [&>div]:gap-1.5 [&>div]:min-w-0 [&>div]:py-4 [&>div]:px-4 [&>div]:bg-surface [&_dt]:text-[0.76rem] [&_dd]:m-0 [&_dd]:font-semibold [&_dd]:break-words to-900:grid-cols-[repeat(2,_minmax(0,_1fr))] to-580:grid-cols-[1fr]">
        <div>
          <dt>Successful releases retained</dt>
          <dd>{item.policy.keepLastSuccessful}</dd>
        </div>
        <div>
          <dt>Safety age</dt>
          <dd>{item.policy.minimumSafetyAgeSeconds}s</dd>
        </div>
        <div>
          <dt>Cache generations</dt>
          <dd>{item.policy.cacheKeepGenerations}</dd>
        </div>
        <div>
          <dt>Cache quota</dt>
          <dd>{formatBytes(item.policy.cacheByteQuota)}</dd>
        </div>
        <div>
          <dt>Lifecycle owner</dt>
          <dd>
            {item.target.mode === "managed"
              ? "Kuberploy managed"
              : "Registry operator"}
          </dd>
        </div>
        <div>
          <dt>Observed</dt>
          <dd>{formatDate(item.observedAt)}</dd>
        </div>
      </dl>
      {!item.inventory ? (
        <Notice>
          <div>
            <strong>Registry inventory unavailable</strong>
            <p>
              Artifact counts and availability are not reported as zero while
              the observer has no complete snapshot.
            </p>
          </div>
        </Notice>
      ) : null}
      {canWritePolicy && humanSession ? (
        <div className="grid gap-4">
          <Button
            variant="secondary"
            onClick={() => setEditingPolicy((current) => !current)}
          >
            <Icon name="settings" />
            {editingPolicy ? "Close policy editor" : "Edit retention policy"}
          </Button>
          {editingPolicy ? (
            <PolicyEditor
              applicationId={application.id}
              target={item.target}
              policy={item.policy}
              onSaved={() => setEditingPolicy(false)}
            />
          ) : null}
        </div>
      ) : null}
      <InventoryTable target={item} />
      {item.target.mode === "managed" && managedFeatureEnabled ? (
        <CleanupPanel
          application={application}
          target={item}
          capabilities={capabilities}
          project={project}
          humanSession={humanSession}
        />
      ) : null}
    </Card>
  );
}

export function RegistryPanel({
  application,
  project,
  capabilities,
  featureEnabled,
  managedFeatureEnabled,
  humanSession,
}: RegistryPanelProps) {
  const canRead = hasRegistryApplicationCapability(
    capabilities,
    "registry:read",
    application,
    project,
  );
  const canWritePolicy = hasRegistryApplicationCapability(
    capabilities,
    "registry-policies:write",
    application,
    project,
  );
  const canReadTargets = hasRegistryPlatformCapability(
    capabilities,
    "registry-targets:read",
  );
  const inventory = useQuery({
    queryKey: ["application-registry", application.id, 50],
    queryFn: () => api.applicationRegistry(application.id, 50),
    enabled: featureEnabled && canRead,
    retry: false,
  });
  const targets = useQuery({
    queryKey: ["registry-targets", 100],
    queryFn: () => api.registryTargets(100),
    enabled: featureEnabled && canRead && canReadTargets,
    retry: false,
  });
  const [attachTargetID, setAttachTargetID] = useState("");
  const attachedIDs = useMemo(
    () => new Set(inventory.data?.items.map((item) => item.target.id) ?? []),
    [inventory.data?.items],
  );
  const attachableTargets =
    targets.data?.items.filter((target) => !attachedIDs.has(target.id)) ?? [];
  const attachTarget = attachableTargets.find(
    (target) => target.id === attachTargetID,
  );

  if (!featureEnabled) return null;
  if (!canRead) {
    return (
      <Card>
        <EmptyState
          icon="settings"
          title="Registry access required"
          description="An exact registry:read capability covering this application is required."
        />
      </Card>
    );
  }
  if (inventory.isPending) {
    return (
      <Card>
        <Skeleton lines={10} />
      </Card>
    );
  }
  if (inventory.error) {
    return (
      <ErrorPanel
        error={inventory.error}
        title="Could not load artifact inventory"
        onRetry={() => void inventory.refetch()}
      />
    );
  }
  return (
    <div className="grid gap-4">
      {inventory.data?.items.length === 0 ? (
        <Card>
          <EmptyState
            icon="layers"
            title="No registry policy configured"
            description="A platform administrator can attach an approved target. Scoped administrators can edit an existing application policy."
          />
        </Card>
      ) : (
        inventory.data?.items.map((item) => (
          <RegistryTargetCard
            key={item.target.id}
            application={application}
            item={item}
            project={project}
            capabilities={capabilities}
            managedFeatureEnabled={managedFeatureEnabled}
            humanSession={humanSession}
          />
        ))
      )}

      {canWritePolicy && canReadTargets && humanSession ? (
        <Card>
          <CardHeader>
            <div>
              <Eyebrow>Application policy</Eyebrow>
              <h2>Attach an approved target</h2>
            </div>
          </CardHeader>
          {targets.error ? (
            <ErrorPanel
              error={targets.error}
              onRetry={() => void targets.refetch()}
            />
          ) : null}
          {attachableTargets.length === 0 ? (
            <MutedCopy>
              Every configured target is already attached to this application.
            </MutedCopy>
          ) : (
            <>
              <Field label="Registry target" required>
                <Select
                  value={attachTargetID}
                  onChange={(event) => setAttachTargetID(event.target.value)}
                >
                  <option value="">Select a target</option>
                  {attachableTargets.map((target) => (
                    <option key={target.id} value={target.id}>
                      {target.name} · {titleCase(target.mode)}
                    </option>
                  ))}
                </Select>
              </Field>
              {attachTarget ? (
                <PolicyEditor
                  key={attachTarget.id}
                  applicationId={application.id}
                  target={attachTarget}
                  defaultRepository={canonicalBuildRepository(
                    attachTarget,
                    application.projectId,
                    application.id,
                  )}
                  onSaved={() => setAttachTargetID("")}
                />
              ) : null}
            </>
          )}
        </Card>
      ) : null}
    </div>
  );
}
