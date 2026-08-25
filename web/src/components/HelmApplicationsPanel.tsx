import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api/client";
import type {
  Application,
  Capability,
  Environment,
  HelmReleaseRevision,
  HelmSource,
  HelmValuesInput,
  Project,
} from "../api/types";
import { formatDate, shortId } from "../lib/format";
import { hasHelmCapability } from "../lib/helmAccess";
import { Icon } from "./Icon";
import {
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Field,
  Skeleton,
  StatusPill,
} from "./ui";

const maximumValuesBytes = 262_144;
const emptySource: HelmSource = {
  kind: "helm-repository",
  repositoryUrl: "",
  chart: "",
  targetRevision: "",
};

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

function sourceLabel(source: HelmSource) {
  if (source.kind === "git") return "Git repository";
  if (source.kind === "oci") return "OCI registry";
  return "Helm repository";
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
    hasHelmCapability(capabilities, "helm.read", application, environment, project);
  const canDeploy =
    humanSession &&
    hasHelmCapability(capabilities, "helm.deploy", application, environment, project);
  const canRetry =
    humanSession &&
    hasHelmCapability(capabilities, "helm.retry", application, environment, project);
  const canRollback =
    humanSession &&
    rollbackFeatureEnabled &&
    hasHelmCapability(capabilities, "helm.rollback", application, environment, project);
  const target = [application.id, environment.id] as const;
  const headKey = ["helm-release", ...target] as const;
  const historyKey = ["helm-release-history", ...target] as const;
  const head = useQuery({
    queryKey: headKey,
    queryFn: () => api.helmRelease(...target),
    enabled: canRead,
    retry: false,
    refetchInterval: (query) =>
      query.state.data?.state === "pending" ? 3_000 : false,
  });
  const history = useQuery({
    queryKey: historyKey,
    queryFn: () => api.helmReleaseHistory(...target, 25),
    enabled: canRead,
    retry: false,
  });
  const [source, setSource] = useState<HelmSource>(emptySource);
  const [valuesYaml, setValuesYaml] = useState("{}\n");
  const [loadedRevision, setLoadedRevision] = useState("");
  const [confirmAction, setConfirmAction] = useState<
    | { kind: "retry" }
    | { kind: "disable" }
    | { kind: "rollback"; revision: HelmReleaseRevision }
    | null
  >(null);
  const stableAttempt = useRef<{ signature: string; key: string } | null>(null);

  useEffect(() => {
    const revision = head.data;
    if (!revision || revision.id === loadedRevision) return;
    setSource(revision.source);
    setValuesYaml(revision.valuesYaml || "{}\n");
    setLoadedRevision(revision.id);
  }, [head.data, loadedRevision]);

  useEffect(() => {
    setSource(emptySource);
    setValuesYaml("{}\n");
    setLoadedRevision("");
    setConfirmAction(null);
    stableAttempt.current = null;
  }, [application.id, environment.id]);

  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: headKey }),
      queryClient.invalidateQueries({ queryKey: historyKey }),
    ]);
  };
  const deploy = useMutation({
    mutationFn: ({ input, key }: { input: HelmValuesInput; key: string }) =>
      api.upsertHelmRelease(application.id, environment.id, input, key),
    retry: retryNetworkOnce,
    onSuccess: refresh,
  });
  const action = useMutation({
    mutationFn: async (request: NonNullable<typeof confirmAction>) => {
      const key = crypto.randomUUID();
      if (request.kind === "retry")
        return api.retryHelmRelease(application.id, environment.id, key);
      if (request.kind === "disable")
        return api.disableHelmRelease(application.id, environment.id, key);
      return api.rollbackHelmRelease(
        application.id,
        environment.id,
        request.revision.id,
        key,
      );
    },
    retry: retryNetworkOnce,
    onSuccess: async () => {
      setConfirmAction(null);
      await refresh();
    },
  });

  if (!featureEnabled) {
    return (
      <EmptyState
        title="Helm Apps unavailable"
        description="Enable direct Argo CD Helm App reconciliation for this installation."
      />
    );
  }
  if (!canRead) {
    return (
      <EmptyState
        title="Helm App hidden"
        description="Your current access does not include this App and Environment."
      />
    );
  }
  if (head.isPending || history.isPending) return <Skeleton lines={8} />;
  const noRelease = head.error instanceof ApiError && head.error.status === 404;
  const loadError = noRelease ? history.error : head.error ?? history.error;
  if (loadError) return <ErrorPanel error={loadError} onRetry={refresh} />;

  const valuesBytes = new TextEncoder().encode(valuesYaml).byteLength;
  const sourceValid =
    source.repositoryUrl.trim() !== "" &&
    source.targetRevision.trim() !== "" &&
    (source.kind === "git"
      ? Boolean(source.path?.trim())
      : Boolean(source.chart?.trim()));
  const canSave =
    canDeploy && sourceValid && valuesBytes > 0 && valuesBytes <= maximumValuesBytes;
  const save = () => {
    const input: HelmValuesInput = { source, valuesYaml };
    const signature = JSON.stringify(input);
    const key =
      stableAttempt.current?.signature === signature
        ? stableAttempt.current.key
        : crypto.randomUUID();
    stableAttempt.current = { signature, key };
    deploy.mutate({ input, key });
  };

  return (
    <div className="stack stack--lg">
      <Card className="form-card">
        <div className="form-card__heading">
          <span>01</span>
          <div>
            <h2>Helm source</h2>
            <p>
              Kuberploy creates one Argo CD Application. Argo fetches, renders,
              and synchronizes the selected chart.
            </p>
          </div>
        </div>
        <div className="form-grid form-grid--two">
          <Field label="Source type" required>
            <select
              aria-label="Helm source type"
              value={source.kind}
              onChange={(event) => {
                const kind = event.target.value as HelmSource["kind"];
                setSource({
                  kind,
                  repositoryUrl: "",
                  targetRevision: kind === "git" ? "main" : "",
                  ...(kind === "git" ? { path: "" } : { chart: "" }),
                });
              }}
            >
              <option value="helm-repository">Helm repository</option>
              <option value="oci">OCI registry</option>
              <option value="git">Git repository</option>
            </select>
          </Field>
          <Field label={`${sourceLabel(source)} URL`} required>
            <input
              aria-label="Helm repository URL"
              value={source.repositoryUrl}
              placeholder={
                source.kind === "oci"
                  ? "ghcr.io/organization/charts"
                  : source.kind === "git"
                    ? "https://github.com/org/repository.git"
                    : "https://charts.example.com"
              }
              onChange={(event) =>
                setSource({ ...source, repositoryUrl: event.target.value })
              }
            />
          </Field>
          {source.kind === "git" ? (
            <Field label="Chart path" required>
              <input
                value={source.path ?? ""}
                placeholder="charts/app"
                onChange={(event) =>
                  setSource({ ...source, path: event.target.value })
                }
              />
            </Field>
          ) : (
            <Field label="Chart name" required>
              <input
                value={source.chart ?? ""}
                placeholder="valkey"
                onChange={(event) =>
                  setSource({ ...source, chart: event.target.value })
                }
              />
            </Field>
          )}
          <Field label={source.kind === "git" ? "Git revision" : "Chart version"} required>
            <input
              value={source.targetRevision}
              placeholder={source.kind === "git" ? "main" : "1.2.3"}
              onChange={(event) =>
                setSource({ ...source, targetRevision: event.target.value })
              }
            />
          </Field>
        </div>
      </Card>

      <Card className="form-card">
        <div className="form-card__heading">
          <span>02</span>
          <div>
            <h2>Values YAML</h2>
            <p>These values are forwarded to Argo CD as the chart override.</p>
          </div>
        </div>
        <Field
          label="values.yaml"
          hint={`${valuesBytes.toLocaleString()} / ${maximumValuesBytes.toLocaleString()} bytes`}
          error={valuesBytes > maximumValuesBytes ? "Values exceed 262144 bytes." : undefined}
        >
          <textarea
            aria-label="Helm values YAML"
            className="code-editor"
            rows={16}
            spellCheck={false}
            value={valuesYaml}
            onChange={(event) => setValuesYaml(event.target.value)}
          />
        </Field>
        <div className="button-row">
          <Button onClick={save} busy={deploy.isPending} disabled={!canSave}>
            <Icon name="deploy" /> {head.data?.desiredEnabled ? "Update App" : "Deploy App"}
          </Button>
          {head.data?.desiredEnabled && canDeploy ? (
            <Button variant="danger" onClick={() => setConfirmAction({ kind: "disable" })}>
              Disable App
            </Button>
          ) : null}
          {head.data?.state === "failed" && canRetry ? (
            <Button variant="secondary" onClick={() => setConfirmAction({ kind: "retry" })}>
              Retry apply
            </Button>
          ) : null}
        </div>
        {deploy.error || action.error ? <ErrorPanel error={deploy.error ?? action.error} /> : null}
      </Card>

      {head.data ? (
        <Card>
          <div className="section-heading">
            <div>
              <span className="eyebrow">Argo desired state</span>
              <h2>Current revision</h2>
            </div>
            <StatusPill value={head.data.state} />
          </div>
          <p>
            Generation {head.data.generation} · {sourceLabel(head.data.source)} · values{" "}
            <code>{shortId(head.data.valuesDigest)}</code> · updated {formatDate(head.data.updatedAt)}
          </p>
          {head.data.failureCode ? (
            <div className="notice notice--error" role="alert">
              <div><strong>Argo apply failed</strong><p>{head.data.failureCode}</p></div>
            </div>
          ) : null}
        </Card>
      ) : null}

      {(history.data?.items.length ?? 0) > 0 ? (
        <Card>
          <div className="section-heading"><div><span className="eyebrow">History</span><h2>Helm revisions</h2></div></div>
          <div className="data-list">
            {history.data?.items.map((revision) => (
              <div className="data-list__row" key={revision.id}>
                <div>
                  <strong>Generation {revision.generation}</strong>
                  <small>{sourceLabel(revision.source)} · {formatDate(revision.createdAt)}</small>
                </div>
                <StatusPill value={revision.state} />
                {canRollback && revision.desiredEnabled && revision.id !== head.data?.id ? (
                  <Button variant="secondary" onClick={() => setConfirmAction({ kind: "rollback", revision })}>
                    Roll back
                  </Button>
                ) : null}
              </div>
            ))}
          </div>
        </Card>
      ) : null}

      {confirmAction ? (
        <ConfirmDialog
          title={confirmAction.kind === "disable" ? "Disable this Helm App?" : confirmAction.kind === "retry" ? "Retry this Helm App?" : `Roll back to generation ${confirmAction.revision.generation}?`}
          description="Kuberploy will create a new immutable revision and reconcile the owned Argo CD Application."
          confirmLabel={confirmAction.kind === "disable" ? "Disable App" : confirmAction.kind === "retry" ? "Retry" : "Roll back"}
          busy={action.isPending}
          onCancel={() => setConfirmAction(null)}
          onConfirm={() => action.mutate(confirmAction)}
        />
      ) : null}
    </div>
  );
}
