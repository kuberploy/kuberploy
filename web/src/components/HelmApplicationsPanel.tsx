import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api/client";
import type {
  Application,
  Capability,
  Environment,
  HelmApproval,
  HelmMutationResult,
  HelmReleaseStatus,
  HelmValuesInput,
  Project,
} from "../api/types";
import { formatDate, shortId, titleCase } from "../lib/format";
import { hasHelmCapability } from "../lib/helmAccess";
import { Icon } from "./Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  Skeleton,
  StatusPill,
} from "./ui";

const maximumValuesBytes = 262_144;

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

function approvalKey(approval: HelmApproval) {
  return `${approval.id}:${approval.revision}`;
}

function helmInput(
  approval: HelmApproval,
  valuesYaml: string,
): HelmValuesInput {
  return {
    approvalId: approval.id,
    approvalRevision: approval.revision,
    valuesYaml,
  };
}

function renderPhase(status: HelmReleaseStatus) {
  if (status.revision.action === "disable") return "not required";
  return status.renderState ?? "pending";
}

function publicationPhase(status: HelmReleaseStatus) {
  if (status.phase === "published") return "desired state published";
  if (status.phase === "failed" || status.phase === "render-failed")
    return "failed";
  return status.phase;
}

function ReleaseTruth({ status }: { status: HelmReleaseStatus }) {
  const pathAbsentRecovery =
    status.failureCode === "cascade-path-absent-recovery-required";
  return (
    <div className="helm-release-truth">
      <Card className="helm-phase-card">
        <span className="eyebrow">Phase 1 · offline render</span>
        <div className="helm-phase-card__status">
          <strong>{titleCase(renderPhase(status))}</strong>
          <StatusPill value={renderPhase(status)} />
        </div>
        <small>
          Approval revision {status.revision.approval.revision} · values{" "}
          <code>{shortId(status.revision.valuesDigest)}</code>
        </small>
      </Card>
      <Card className="helm-phase-card">
        <span className="eyebrow">Phase 2 · protected Git publication</span>
        <div className="helm-phase-card__status">
          <strong>{titleCase(publicationPhase(status))}</strong>
          <StatusPill value={status.phase} />
        </div>
        <small>
          Payload: {status.payloadState ?? "pending"} · Application:{" "}
          {status.applicationState ?? "pending"}
        </small>
      </Card>
      {pathAbsentRecovery ? (
        <div className="notice notice--error" role="alert">
          <div>
            <strong>Disable recovery requires an explicit rollback</strong>
            <p>
              The protected Argo Application path was already absent, so
              Kuberploy did not recreate historical desired state. Roll back or
              re-enable the previous release, wait until it is published, then
              disable it again.
            </p>
          </div>
        </div>
      ) : null}
      <div className="notice">
        <div>
          <strong>
            This is desired-state publication, not rollout health.
          </strong>
          <p>
            Even “published” means the protected Git intents completed. It does
            not claim that Argo synced the release or that workloads are ready.
          </p>
        </div>
      </div>
    </div>
  );
}

function mutationNotice(result?: HelmMutationResult) {
  if (!result) return null;
  return (
    <div className="notice notice--success" role="status">
      <div>
        <strong>
          {result.replayed
            ? "Existing request replayed safely"
            : "Desired intent accepted"}
        </strong>
        <p>
          Revision {result.revision.generation} is durable and pending protected
          processing; it is not an observed runtime result.
        </p>
      </div>
    </div>
  );
}

export function HelmApplicationsPanel({
  application,
  environment,
  project,
  capabilities,
  featureEnabled,
  rollbackFeatureEnabled,
  humanSession,
}: {
  application: Application;
  environment: Environment;
  project?: Project;
  capabilities: Capability[];
  featureEnabled: boolean;
  rollbackFeatureEnabled: boolean;
  humanSession: boolean;
}) {
  const queryClient = useQueryClient();
  const canRead =
    featureEnabled &&
    hasHelmCapability(
      capabilities,
      "helm.read",
      application,
      environment,
      project,
    );
  const canDeploy =
    humanSession &&
    hasHelmCapability(
      capabilities,
      "helm.deploy",
      application,
      environment,
      project,
    );
  const canRetry =
    humanSession &&
    hasHelmCapability(
      capabilities,
      "helm.retry",
      application,
      environment,
      project,
    );
  const canRollback =
    humanSession &&
    rollbackFeatureEnabled &&
    hasHelmCapability(
      capabilities,
      "helm.rollback",
      application,
      environment,
      project,
    );
  const target = [application.id, environment.id] as const;
  const scopeKey = target.join("\u0000");
  const activeScopeRef = useRef(scopeKey);
  activeScopeRef.current = scopeKey;
  const queryClientKeys = {
    head: ["helm-release", ...target] as const,
    history: ["helm-release-history", ...target] as const,
  };
  const approvals = useQuery({
    queryKey: ["helm-approvals", ...target],
    queryFn: () => api.helmApprovals(...target, 50),
    enabled: canRead,
    retry: false,
  });
  const head = useQuery({
    queryKey: queryClientKeys.head,
    queryFn: () => api.helmRelease(...target),
    enabled: canRead,
    retry: false,
    refetchInterval: (query) => {
      const phase = query.state.data?.phase;
      return phase && !["published", "failed", "render-failed"].includes(phase)
        ? 5_000
        : false;
    },
  });
  const history = useQuery({
    queryKey: queryClientKeys.history,
    queryFn: () => api.helmReleaseHistory(...target, 25),
    enabled: canRead,
    retry: false,
  });
  const renderedPreview = useQuery({
    queryKey: ["helm-rendered-preview", ...target],
    queryFn: () => api.helmRenderedPreview(...target),
    enabled: canRead && head.data?.revision.desiredEnabled === true,
    retry: false,
  });
  const [selectedApprovalKey, setSelectedApprovalKey] = useState("");
  const selectedApproval = useMemo(
    () =>
      approvals.data?.items.find(
        (approval) => approvalKey(approval) === selectedApprovalKey,
      ),
    [approvals.data, selectedApprovalKey],
  );
  const [valuesYaml, setValuesYaml] = useState("");
  const [previewedDraft, setPreviewedDraft] = useState("");
  const [retryConfirmation, setRetryConfirmation] = useState(false);
  const [disableConfirmation, setDisableConfirmation] = useState("");
  const [rollbackSource, setRollbackSource] = useState("");
  const [rollbackConfirmation, setRollbackConfirmation] = useState(false);
  const [previewError, setPreviewError] = useState<unknown>(null);
  const [mutationFeedback, setMutationFeedback] = useState<{
    scopeKey: string;
    result?: HelmMutationResult;
    error?: unknown;
  } | null>(null);
  const upsertAttempt = useRef<{ signature: string; key: string } | null>(null);
  const retryAttempt = useRef<{ signature: string; key: string } | null>(null);
  const disableAttempt = useRef<{ signature: string; key: string } | null>(
    null,
  );
  const rollbackAttempt = useRef<{ signature: string; key: string } | null>(
    null,
  );
  const retryScopeRef = useRef<{
    releaseId: string;
    key: string;
    open: boolean;
  } | null>(null);
  const disableScopeRef = useRef<{
    releaseId: string;
    key: string;
    confirmation: string;
  } | null>(null);
  const rollbackScopeRef = useRef<{
    source: string;
    key: string;
    confirmed: boolean;
  } | null>(null);
  retryScopeRef.current = head.data
    ? {
        releaseId: head.data.revision.id,
        key: retryAttempt.current?.key ?? "",
        open: retryConfirmation,
      }
    : null;
  disableScopeRef.current = head.data
    ? {
        releaseId: head.data.revision.id,
        key: disableAttempt.current?.key ?? "",
        confirmation: disableConfirmation,
      }
    : null;
  rollbackScopeRef.current = {
    source: rollbackSource,
    key: rollbackAttempt.current?.key ?? "",
    confirmed: rollbackConfirmation,
  };

  useEffect(() => {
    setSelectedApprovalKey("");
    setValuesYaml("");
    setPreviewedDraft("");
    setRetryConfirmation(false);
    setDisableConfirmation("");
    setRollbackSource("");
    setRollbackConfirmation(false);
    upsertAttempt.current = null;
    retryAttempt.current = null;
    disableAttempt.current = null;
    rollbackAttempt.current = null;
  }, [application.id, environment.id]);

  useEffect(() => {
    if (approvals.isFetching) return;
    const items = approvals.data?.items ?? [];
    const first = items[0];
    if (!first) {
      if (selectedApprovalKey) setSelectedApprovalKey("");
      return;
    }
    if (
      selectedApprovalKey &&
      items.some((approval) => approvalKey(approval) === selectedApprovalKey)
    ) {
      return;
    }
    setSelectedApprovalKey(approvalKey(first));
    setValuesYaml(first.defaultValuesYaml || "{}\n");
    setPreviewedDraft("");
  }, [approvals.data, approvals.isFetching, selectedApprovalKey]);

  const draftIdentity = selectedApproval
    ? `${application.id}\u0000${environment.id}\u0000${approvalKey(selectedApproval)}\u0000${valuesYaml}`
    : "";
  const valuesBytes = new TextEncoder().encode(valuesYaml).byteLength;
  const valuesValid = valuesBytes > 0 && valuesBytes <= maximumValuesBytes;
  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: queryClientKeys.head }),
      queryClient.invalidateQueries({ queryKey: queryClientKeys.history }),
      queryClient.invalidateQueries({
        queryKey: ["helm-rendered-preview", ...target],
      }),
    ]);
  };
  const preview = useMutation({
    mutationFn: ({
      input,
      applicationId,
      environmentId,
    }: {
      input: HelmValuesInput;
      applicationId: string;
      environmentId: string;
      draftIdentity: string;
      scopeKey: string;
    }) => api.previewHelmValues(applicationId, environmentId, input),
    retry: retryNetworkOnce,
    onMutate: () => setPreviewError(null),
    onSuccess: (_value, input) => {
      if (input.scopeKey !== activeScopeRef.current) return;
      setPreviewedDraft(input.draftIdentity);
    },
    onError: (error, input) => {
      if (input.scopeKey !== activeScopeRef.current) return;
      setPreviewError(error);
    },
  });
  const upsert = useMutation({
    mutationFn: ({
      input,
      key,
      applicationId,
      environmentId,
    }: {
      input: HelmValuesInput;
      key: string;
      applicationId: string;
      environmentId: string;
      scopeKey: string;
      draftIdentity: string;
    }) => api.upsertHelmRelease(applicationId, environmentId, input, key),
    retry: retryNetworkOnce,
    onSuccess: async (_value, input) => {
      if (input.scopeKey !== activeScopeRef.current) return;
      if (input.draftIdentity !== draftIdentity) {
        await refresh();
        return;
      }
      if (upsertAttempt.current?.key === input.key) {
        upsertAttempt.current = null;
      }
      setMutationFeedback({ scopeKey, result: _value });
      await refresh();
    },
    onError: (error, input) => {
      if (
        input.scopeKey === activeScopeRef.current &&
        input.draftIdentity === draftIdentity
      ) {
        setMutationFeedback({ scopeKey, error });
      }
    },
  });
  const retry = useMutation({
    mutationFn: ({
      key,
      applicationId,
      environmentId,
    }: {
      key: string;
      applicationId: string;
      environmentId: string;
      scopeKey: string;
      releaseId: string;
    }) => api.retryHelmRelease(applicationId, environmentId, key),
    retry: retryNetworkOnce,
    onSuccess: async (_value, input) => {
      if (input.scopeKey !== activeScopeRef.current) return;
      const current = retryScopeRef.current;
      if (
        !current?.open ||
        current.releaseId !== input.releaseId ||
        retryAttempt.current?.key !== input.key
      )
        return;
      if (retryAttempt.current?.key === input.key) {
        retryAttempt.current = null;
      }
      setRetryConfirmation(false);
      setMutationFeedback({ scopeKey, result: _value });
      await refresh();
    },
    onError: (error, input) => {
      const current = retryScopeRef.current;
      if (
        input.scopeKey === activeScopeRef.current &&
        current?.open &&
        current.releaseId === input.releaseId &&
        retryAttempt.current?.key === input.key
      ) {
        setMutationFeedback({ scopeKey, error });
      }
    },
  });
  const disable = useMutation({
    mutationFn: ({
      key,
      applicationId,
      environmentId,
    }: {
      key: string;
      applicationId: string;
      environmentId: string;
      scopeKey: string;
      releaseId: string;
      confirmation: string;
    }) => api.disableHelmRelease(applicationId, environmentId, key),
    retry: retryNetworkOnce,
    onSuccess: async (_value, input) => {
      if (input.scopeKey !== activeScopeRef.current) return;
      const current = disableScopeRef.current;
      if (
        !current ||
        current.releaseId !== input.releaseId ||
        disableAttempt.current?.key !== input.key ||
        current.confirmation !== input.confirmation
      )
        return;
      if (disableAttempt.current?.key === input.key) {
        disableAttempt.current = null;
      }
      setDisableConfirmation("");
      setMutationFeedback({ scopeKey, result: _value });
      await refresh();
    },
    onError: (error, input) => {
      const current = disableScopeRef.current;
      if (
        input.scopeKey === activeScopeRef.current &&
        current?.releaseId === input.releaseId &&
        disableAttempt.current?.key === input.key &&
        current.confirmation === input.confirmation
      ) {
        setMutationFeedback({ scopeKey, error });
      }
    },
  });
  const rollback = useMutation({
    mutationFn: ({
      source,
      key,
      applicationId,
      environmentId,
    }: {
      source: string;
      key: string;
      applicationId: string;
      environmentId: string;
      scopeKey: string;
      confirmation: boolean;
    }) => api.rollbackHelmRelease(applicationId, environmentId, source, key),
    retry: retryNetworkOnce,
    onSuccess: async (_value, input) => {
      if (input.scopeKey !== activeScopeRef.current) return;
      const current = rollbackScopeRef.current;
      if (
        !current ||
        current.source !== input.source ||
        rollbackAttempt.current?.key !== input.key ||
        !current.confirmed ||
        !input.confirmation
      )
        return;
      if (rollbackAttempt.current?.key === input.key) {
        rollbackAttempt.current = null;
      }
      setRollbackSource("");
      setRollbackConfirmation(false);
      setMutationFeedback({ scopeKey, result: _value });
      await refresh();
    },
    onError: (error, input) => {
      const current = rollbackScopeRef.current;
      if (
        input.scopeKey === activeScopeRef.current &&
        current?.source === input.source &&
        rollbackAttempt.current?.key === input.key &&
        current.confirmed === input.confirmation
      ) {
        setMutationFeedback({ scopeKey, error });
      }
    },
  });

  useEffect(() => {
    setPreviewError(null);
    setMutationFeedback(null);
    preview.reset();
    upsert.reset();
    retry.reset();
    disable.reset();
    rollback.reset();
  }, [application.id, environment.id]);

  const upsertRelease = () => {
    if (!selectedApproval) return;
    const input = helmInput(selectedApproval, valuesYaml);
    const signature = JSON.stringify(input);
    const key =
      upsertAttempt.current?.signature === signature
        ? upsertAttempt.current.key
        : crypto.randomUUID();
    upsertAttempt.current = { signature, key };
    upsert.mutate({
      input,
      key,
      applicationId: application.id,
      environmentId: environment.id,
      scopeKey,
      draftIdentity,
    });
  };

  const retryRelease = () => {
    if (!head.data) return;
    const releaseId = head.data.revision.id;
    const signature = JSON.stringify({
      releaseId,
      action: "retry",
    });
    const key =
      retryAttempt.current?.signature === signature
        ? retryAttempt.current.key
        : crypto.randomUUID();
    retryAttempt.current = { signature, key };
    retry.mutate({
      key,
      applicationId: application.id,
      environmentId: environment.id,
      scopeKey,
      releaseId,
    });
  };

  const disableRelease = () => {
    if (!head.data) return;
    const signature = JSON.stringify({
      releaseId: head.data.revision.id,
      confirmation: disableConfirmation,
      action: "disable",
    });
    const key =
      disableAttempt.current?.signature === signature
        ? disableAttempt.current.key
        : crypto.randomUUID();
    disableAttempt.current = { signature, key };
    disable.mutate({
      key,
      applicationId: application.id,
      environmentId: environment.id,
      scopeKey,
      releaseId: head.data.revision.id,
      confirmation: disableConfirmation,
    });
  };

  const rollbackRelease = () => {
    if (!rollbackSource) return;
    const signature = JSON.stringify({
      source: rollbackSource,
      action: "rollback",
    });
    const key =
      rollbackAttempt.current?.signature === signature
        ? rollbackAttempt.current.key
        : crypto.randomUUID();
    rollbackAttempt.current = { signature, key };
    rollback.mutate({
      source: rollbackSource,
      key,
      applicationId: application.id,
      environmentId: environment.id,
      scopeKey,
      confirmation: rollbackConfirmation,
    });
  };

  if (!canRead) return null;
  const noHead = head.error instanceof ApiError && head.error.status === 404;
  const desiredReleaseDisabled = head.data?.revision.desiredEnabled === false;
  const mutationError =
    mutationFeedback?.scopeKey === scopeKey ? mutationFeedback.error : null;
  const latestMutation =
    mutationFeedback?.scopeKey === scopeKey
      ? mutationFeedback.result
      : undefined;

  return (
    <div className="helm-applications-panel">
      <Card>
        <div className="card__header card__header--inside">
          <div>
            <span className="eyebrow">Approved external Helm</span>
            <h2>{environment.name}</h2>
            <p>
              Select one immutable platform approval and edit only its bounded
              values.yaml. Release identity and delivery controls are
              server-owned.
            </p>
          </div>
        </div>
        {!humanSession ? (
          <div className="notice">
            <div>
              <strong>Read-only automation identity</strong>
              <p>Helm mutations in this browser require a human session.</p>
            </div>
          </div>
        ) : null}
        {approvals.isPending ? <Skeleton lines={4} /> : null}
        {approvals.error ? <ErrorPanel error={approvals.error} /> : null}
        {approvals.data?.items.length === 0 ? (
          <EmptyState
            title="No approved Helm charts"
            description="A platform administrator must publish an immutable approval before this environment can create a Helm release."
          />
        ) : null}
        {selectedApproval ? (
          <div className="helm-editor-grid">
            <Field
              label="Approved chart revision"
              hint="Only immutable entries from the platform approval catalog are selectable."
              required
            >
              <select
                value={selectedApprovalKey}
                onChange={(event) => {
                  const key = event.target.value;
                  const next = approvals.data?.items.find(
                    (approval) => approvalKey(approval) === key,
                  );
                  setSelectedApprovalKey(key);
                  setValuesYaml(next?.defaultValuesYaml || "{}\n");
                  setPreviewedDraft("");
                  setPreviewError(null);
                  preview.reset();
                }}
              >
                {approvals.data?.items.map((approval) => (
                  <option
                    key={approvalKey(approval)}
                    value={approvalKey(approval)}
                  >
                    {approval.repository} · {approval.version} · approval{" "}
                    {approval.revision}
                  </option>
                ))}
              </select>
            </Field>
            <Field
              label="values.yaml"
              hint={`${valuesBytes.toLocaleString()} / ${maximumValuesBytes.toLocaleString()} UTF-8 bytes`}
              error={
                !valuesValid
                  ? "Enter no more than 262144 UTF-8 bytes."
                  : undefined
              }
              required
            >
              <textarea
                className="helm-values-editor"
                aria-label="Helm values YAML"
                maxLength={maximumValuesBytes}
                spellCheck={false}
                value={valuesYaml}
                onChange={(event) => {
                  setValuesYaml(event.target.value);
                  setPreviewedDraft("");
                  setPreviewError(null);
                  preview.reset();
                }}
              />
            </Field>
            {previewError ? <ErrorPanel error={previewError} /> : null}
            {preview.data && previewedDraft === draftIdentity ? (
              <div className="helm-values-preview" role="status">
                <strong>Values validated</strong>
                <p>
                  Digest <code>{preview.data.valuesDigest}</code>
                </p>
                <p>
                  {preview.data.changedPaths.length === 0
                    ? "No effective values differ from the current release."
                    : `${preview.data.changedPaths.length} effective path(s) changed.`}
                </p>
                {preview.data.changedPaths.length > 0 ? (
                  <ul>
                    {preview.data.changedPaths.slice(0, 50).map((path) => (
                      <li key={path}>
                        <code>{path}</code>
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>
            ) : null}
            <div className="helm-editor-actions">
              <Button
                variant="secondary"
                busy={preview.isPending}
                disabled={!valuesValid || upsert.isPending}
                onClick={() =>
                  preview.mutate({
                    input: helmInput(selectedApproval, valuesYaml),
                    applicationId: application.id,
                    environmentId: environment.id,
                    draftIdentity,
                    scopeKey,
                  })
                }
              >
                <Icon name="check" /> Validate values
              </Button>
              {canDeploy ? (
                <Button
                  busy={upsert.isPending}
                  disabled={
                    previewedDraft !== draftIdentity || preview.isPending
                  }
                  onClick={upsertRelease}
                >
                  <Icon name="deploy" />{" "}
                  {noHead ? "Create desired release" : "Create update revision"}
                </Button>
              ) : null}
            </div>
          </div>
        ) : null}
      </Card>

      {mutationNotice(latestMutation)}
      {mutationError ? (
        <ErrorPanel
          error={mutationError}
          title="Helm command was not accepted"
        />
      ) : null}

      <Card>
        <div className="card__header card__header--inside">
          <div>
            <span className="eyebrow">Current desired release</span>
            <h2>Two-phase status</h2>
          </div>
        </div>
        {head.isPending ? <Skeleton lines={5} /> : null}
        {head.error && !noHead ? (
          <ErrorPanel error={head.error} onRetry={() => void head.refetch()} />
        ) : null}
        {noHead ? (
          <EmptyState
            title="No desired Helm release"
            description="Validate approved values and create the first immutable release revision."
          />
        ) : null}
        {head.data ? <ReleaseTruth status={head.data} /> : null}
        {head.data && (canRetry || canDeploy) ? (
          <div className="helm-danger-actions">
            {canRetry ? (
              retryConfirmation ? (
                <div className="notice">
                  <div>
                    <strong>Retry {head.data.revision.releaseName}?</strong>
                    <p>
                      This creates a new immutable revision from the current
                      desired release.
                    </p>
                  </div>
                  <Button busy={retry.isPending} onClick={retryRelease}>
                    Confirm retry
                  </Button>
                  <Button
                    variant="ghost"
                    onClick={() => {
                      retryAttempt.current = null;
                      setRetryConfirmation(false);
                    }}
                  >
                    Cancel
                  </Button>
                </div>
              ) : (
                <Button
                  variant="secondary"
                  onClick={() => {
                    retryAttempt.current = null;
                    setRetryConfirmation(true);
                  }}
                >
                  <Icon name="refresh" /> Retry as new revision
                </Button>
              )
            ) : null}
            {canDeploy ? (
              <div className="helm-disable-confirmation">
                <Field
                  label={`Type ${head.data.revision.releaseName} to disable`}
                  hint="Disable publishes a protected delete intent; it does not imperatively uninstall anything."
                >
                  <input
                    value={disableConfirmation}
                    onChange={(event) =>
                      setDisableConfirmation(event.target.value)
                    }
                  />
                </Field>
                <Button
                  variant="danger"
                  busy={disable.isPending}
                  disabled={
                    disableConfirmation !== head.data.revision.releaseName
                  }
                  onClick={disableRelease}
                >
                  Disable desired release
                </Button>
              </div>
            ) : null}
          </div>
        ) : null}
      </Card>

      <Card>
        <div className="card__header card__header--inside">
          <div>
            <span className="eyebrow">Rendered manifests</span>
            <h2>Read-only preview</h2>
          </div>
        </div>
        {desiredReleaseDisabled ? (
          <EmptyState
            title="Desired release disabled"
            description="The latest immutable revision removed desired Helm state, so there is no current rendered inventory to preview. Roll back a published revision or create an update to enable it again."
          />
        ) : renderedPreview.isPending ? (
          <Skeleton lines={4} />
        ) : renderedPreview.error instanceof ApiError &&
          renderedPreview.error.status === 404 ? (
          <EmptyState
            title="No rendered inventory"
            description="The current desired revision has not produced a verified redacted inventory yet."
          />
        ) : renderedPreview.error ? (
          <ErrorPanel
            error={renderedPreview.error}
            onRetry={() => void renderedPreview.refetch()}
          />
        ) : null}
        {!desiredReleaseDisabled && renderedPreview.data ? (
          <div className="helm-values-preview">
            <p>
              Release revision{" "}
              <code>{shortId(renderedPreview.data.releaseRevisionId)}</code> ·
              generation {renderedPreview.data.generation}
            </p>
            <p>
              Manifest <code>{renderedPreview.data.manifestDigest}</code>
            </p>
            <p>
              Inventory <code>{renderedPreview.data.inventoryDigest}</code> ·{" "}
              {renderedPreview.data.resourceCount} resource(s) ·{" "}
              {renderedPreview.data.previewBytes} sanitized preview bytes
            </p>
            <div className="helm-history-list">
              {renderedPreview.data.resources.map((resource, index) => (
                <article
                  className="helm-history-item"
                  key={`${resource.apiVersion}:${resource.kind}:${resource.namespace}:${resource.name}:${index}`}
                >
                  <div>
                    <strong>
                      {resource.kind} · {resource.name}
                    </strong>
                    <small>
                      {resource.apiVersion} · namespace{" "}
                      {resource.namespace || "cluster-scoped"}
                    </small>
                    {resource.previewOmitted ? (
                      <small>
                        Sanitized YAML omitted because this resource exceeded
                        the bounded preview.
                      </small>
                    ) : resource.sanitizedYaml ? (
                      <pre className="helm-rendered-yaml">
                        <code>{resource.sanitizedYaml}</code>
                      </pre>
                    ) : null}
                  </div>
                </article>
              ))}
            </div>
          </div>
        ) : null}
      </Card>

      <Card>
        <div className="card__header card__header--inside">
          <div>
            <span className="eyebrow">Immutable history</span>
            <h2>Desired release revisions</h2>
          </div>
        </div>
        {history.isPending ? <Skeleton lines={5} /> : null}
        {history.error ? (
          <ErrorPanel
            error={history.error}
            onRetry={() => void history.refetch()}
          />
        ) : null}
        {history.data?.items.length === 0 ? (
          <EmptyState
            title="No release history"
            description="Accepted desired revisions appear here newest first."
          />
        ) : null}
        <div className="helm-history-list">
          {history.data?.items.map((status) => (
            <article className="helm-history-item" key={status.revision.id}>
              <div>
                <strong>
                  Revision {status.revision.generation} ·{" "}
                  {titleCase(status.revision.action)}
                </strong>
                <small>
                  {formatDate(status.revision.createdAt)} ·{" "}
                  {shortId(status.revision.id)}
                </small>
              </div>
              <StatusPill value={status.phase} />
              {canRollback && status.revision.id !== head.data?.revision.id ? (
                <Button
                  variant="secondary"
                  onClick={() => {
                    rollbackAttempt.current = null;
                    setRollbackSource(status.revision.id);
                    setRollbackConfirmation(false);
                  }}
                >
                  Roll back to this revision
                </Button>
              ) : null}
            </article>
          ))}
        </div>
        {rollbackSource ? (
          <div className="notice">
            <div>
              <strong>Rollback creates a new desired revision</strong>
              <p>
                Source revision <code>{rollbackSource}</code> remains immutable;
                history is never rewritten and Kubernetes is not called
                directly.
              </p>
              <label className="checkbox-line">
                <input
                  type="checkbox"
                  checked={rollbackConfirmation}
                  onChange={(event) =>
                    setRollbackConfirmation(event.target.checked)
                  }
                />
                I understand this publishes a new protected Git intent.
              </label>
            </div>
            <Button
              variant="danger"
              busy={rollback.isPending}
              disabled={!rollbackConfirmation}
              onClick={rollbackRelease}
            >
              Confirm rollback
            </Button>
            <Button
              variant="ghost"
              onClick={() => {
                rollbackAttempt.current = null;
                setRollbackSource("");
                setRollbackConfirmation(false);
              }}
            >
              Cancel
            </Button>
          </div>
        ) : null}
      </Card>
    </div>
  );
}
