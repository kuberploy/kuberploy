import { useEffect, useMemo, useState } from "react";
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
          processing; it is not an observed deployment result.
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
    enabled: canRead,
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

  useEffect(() => {
    const first = approvals.data?.items[0];
    if (!first || selectedApprovalKey) return;
    setSelectedApprovalKey(approvalKey(first));
    setValuesYaml(first.defaultValuesYaml || "{}\n");
  }, [approvals.data, selectedApprovalKey]);

  const draftIdentity = selectedApproval
    ? `${approvalKey(selectedApproval)}\u0000${valuesYaml}`
    : "";
  const valuesBytes = new TextEncoder().encode(valuesYaml).byteLength;
  const valuesValid = valuesBytes > 0 && valuesBytes <= maximumValuesBytes;
  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: queryClientKeys.head }),
      queryClient.invalidateQueries({ queryKey: queryClientKeys.history }),
    ]);
  };
  const preview = useMutation({
    mutationFn: (input: HelmValuesInput) =>
      api.previewHelmValues(...target, input),
    retry: retryNetworkOnce,
    onSuccess: () => setPreviewedDraft(draftIdentity),
  });
  const upsert = useMutation({
    mutationFn: ({ input, key }: { input: HelmValuesInput; key: string }) =>
      api.upsertHelmRelease(...target, input, key),
    retry: retryNetworkOnce,
    onSuccess: refresh,
  });
  const retry = useMutation({
    mutationFn: (key: string) => api.retryHelmRelease(...target, key),
    retry: retryNetworkOnce,
    onSuccess: async () => {
      setRetryConfirmation(false);
      await refresh();
    },
  });
  const disable = useMutation({
    mutationFn: (key: string) => api.disableHelmRelease(...target, key),
    retry: retryNetworkOnce,
    onSuccess: async () => {
      setDisableConfirmation("");
      await refresh();
    },
  });
  const rollback = useMutation({
    mutationFn: ({ source, key }: { source: string; key: string }) =>
      api.rollbackHelmRelease(...target, source, key),
    retry: retryNetworkOnce,
    onSuccess: async () => {
      setRollbackSource("");
      setRollbackConfirmation(false);
      await refresh();
    },
  });

  if (!canRead) return null;
  const noHead = head.error instanceof ApiError && head.error.status === 404;
  const mutationError =
    upsert.error ?? retry.error ?? disable.error ?? rollback.error;
  const latestMutation =
    upsert.data ?? retry.data ?? disable.data ?? rollback.data;

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
                  preview.reset();
                }}
              />
            </Field>
            {preview.error ? <ErrorPanel error={preview.error} /> : null}
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
                disabled={!valuesValid}
                onClick={() =>
                  preview.mutate(helmInput(selectedApproval, valuesYaml))
                }
              >
                <Icon name="check" /> Validate values
              </Button>
              {canDeploy ? (
                <Button
                  busy={upsert.isPending}
                  disabled={previewedDraft !== draftIdentity}
                  onClick={() =>
                    upsert.mutate({
                      input: helmInput(selectedApproval, valuesYaml),
                      key: crypto.randomUUID(),
                    })
                  }
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
                  <Button
                    busy={retry.isPending}
                    onClick={() => retry.mutate(crypto.randomUUID())}
                  >
                    Confirm retry
                  </Button>
                  <Button
                    variant="ghost"
                    onClick={() => setRetryConfirmation(false)}
                  >
                    Cancel
                  </Button>
                </div>
              ) : (
                <Button
                  variant="secondary"
                  onClick={() => setRetryConfirmation(true)}
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
                  onClick={() => disable.mutate(crypto.randomUUID())}
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
        {renderedPreview.isPending ? <Skeleton lines={4} /> : null}
        {renderedPreview.error instanceof ApiError &&
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
        {renderedPreview.data ? (
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
              onClick={() =>
                rollback.mutate({
                  source: rollbackSource,
                  key: crypto.randomUUID(),
                })
              }
            >
              Confirm rollback
            </Button>
            <Button variant="ghost" onClick={() => setRollbackSource("")}>
              Cancel
            </Button>
          </div>
        ) : null}
      </Card>
    </div>
  );
}
