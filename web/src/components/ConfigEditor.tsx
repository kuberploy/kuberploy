import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import type {
  Application,
  ConfigBundle,
  ConfigChange,
  Deployment,
  Operation,
} from "../api/types";
import { ApiError, api, errorMessage } from "../api/client";
import {
  applyGuidedConfig,
  defaultConfigYaml,
  guidedConfigFromYaml,
  validateYaml,
} from "../lib/configDraft";
import {
  Button,
  buttonVariants,
  CopyButton,
  EmptyState,
  Eyebrow,
  Notice,
  PlaceholderBadge,
  useRovingFocus,
} from "./ui";
import { Icon } from "./Icon";
import { GuidedConfigForm } from "./GuidedConfigForm";
import { MonacoYamlEditor } from "./MonacoYamlEditor";
import { hasDeploymentConfigCapability } from "../lib/configAccess";

type ConfigEditorProps = {
  deployment: Deployment;
  application: Application;
};

const successfulSaveStates = new Set(["succeeded", "healthy"]);
const terminalSaveStates = new Set([
  ...successfulSaveStates,
  "failed",
  "degraded",
  "cancelled",
  "superseded",
]);
const savePollWindow = 15 * 60_000;

function isInvalidPreviewError(error: unknown) {
  return (
    error instanceof ApiError &&
    error.status === 409 &&
    error.problem?.code === "PreviewInvalid"
  );
}

export function ConfigEditor(props: ConfigEditorProps) {
  const { deployment, application } = props;
  if (
    deployment.state === "stopped" &&
    !deployment.image &&
    !deployment.source?.reference &&
    (application.sourceKind === "github" ||
      application.sourceKind === "git-ssh")
  ) {
    const source = application.sourceKind === "git-ssh" ? "ssh" : "build";
    const query = new URLSearchParams({
      tab: "source",
      source,
      environmentId: deployment.environmentId,
    });
    const href = `/projects/${encodeURIComponent(application.projectId)}/environments/${encodeURIComponent(deployment.environmentId)}/apps/${encodeURIComponent(application.id)}?${query}`;
    return (
      <EmptyState
        icon="code"
        title="Build this App first"
        description="Connect your repository in Source & build, then choose Deploy. After the first image is built, you can configure its ports, health checks, resources, and public access here."
        action={
          <a href={href} className={buttonVariants()}>
            Open Source & build <Icon name="arrow" />
          </a>
        }
      />
    );
  }
  return <SavedConfigEditor key={deployment.id} {...props} />;
}

function SavedConfigEditor({ deployment, application }: ConfigEditorProps) {
  const queryClient = useQueryClient();
  const bundle = useQuery({
    queryKey: ["deployment-config", deployment.id],
    queryFn: () => api.deploymentConfig(deployment.id),
    retry: false,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
    staleTime: 60_000,
  });
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const needsProjectContext = effectiveCapabilities.some(
    (capability) =>
      capability.scopeType === "team" &&
      capability.actions?.includes("deployment-config:write") === true,
  );
  const needsEnvironmentContext = effectiveCapabilities.some(
    (capability) =>
      capability.scopeType === "namespace" &&
      capability.actions?.includes("deployment-config:write") === true,
  );
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: api.projects,
    enabled: needsProjectContext,
    retry: false,
  });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    enabled: needsEnvironmentContext,
    retry: false,
  });
  const applicationProject = projects.data?.items.find(
    (project) => project.id === application.projectId,
  );
  const deploymentEnvironment = environments.data?.items.find(
    (environment) => environment.id === deployment.environmentId,
  );
  const canWriteConfig = hasDeploymentConfigCapability(
    effectiveCapabilities,
    "deployment-config:write",
    application,
    deployment,
    applicationProject,
    deploymentEnvironment,
  );
  const middlewareEditingUnavailableReason = !canWriteConfig
    ? "You need effective App configuration write access covering this App and Environment. The current definitions remain inspectable."
    : capabilities.data?.features?.traefikMiddlewares !== true
      ? "The platform has not reported the Traefik middleware runtime capability ready. Existing YAML remains visible and Advanced YAML continues to use the AppConfig contract."
      : undefined;
  const externalDNSConfigured =
    capabilities.data?.features?.externalDNSConfiguration === true;
  const externalDNSCatalog = useQuery({
    queryKey: [
      "application-external-dns-integrations",
      application.id,
      deployment.environmentId,
    ],
    queryFn: () =>
      api.applicationExternalDNSIntegrations(
        application.id,
        deployment.environmentId,
        100,
      ),
    enabled: externalDNSConfigured,
    retry: false,
  });
  const sslipEnabled = capabilities.data?.features?.sslip === true;
  const sslipHostname = useQuery({
    queryKey: [
      "application-sslip-hostname",
      application.id,
      deployment.environmentId,
    ],
    queryFn: () =>
      api.applicationSSLIPHostname(application.id, deployment.environmentId),
    enabled: sslipEnabled,
    retry: false,
  });
  const [tab, setTab] = useState<"form" | "yaml" | "rendered">("form");
  const [rawYaml, setRawYaml] = useState("");
  // Keep the draft's original CAS base through unrelated background refreshes.
  // Only this editor's completed publication advances it automatically.
  const [baseConfig, setBaseConfig] = useState<ConfigBundle | null>(null);
  const [saved, setSaved] = useState<{
    operation: Operation;
    rawYaml: string;
    startedAt: number;
  } | null>(null);
  const [appliedOperationId, setAppliedOperationId] = useState<string | null>(
    null,
  );
  const [publicationConflict, setPublicationConflict] = useState(false);
  const [preview, setPreview] = useState<{
    value: Awaited<ReturnType<typeof api.previewDeploymentConfig>>;
    etag: string;
    rawYaml: string;
    idempotencyKey: string;
  } | null>(null);
  const [draftError, setDraftError] = useState<string | null>(null);
  const rawYamlRef = useRef(rawYaml);
  rawYamlRef.current = rawYaml;

  const serverDocument = baseConfig?.documents[0];
  const fallback = useMemo(
    () =>
      defaultConfigYaml({
        name: application.name,
        image: deployment.image ?? deployment.source?.reference,
        replicas: deployment.replicas,
        port: deployment.port,
      }),
    [
      application.name,
      deployment.image,
      deployment.port,
      deployment.replicas,
      deployment.source?.reference,
    ],
  );

  useEffect(() => {
    if (bundle.isPending || baseConfig) return;
    if (bundle.data) setBaseConfig(bundle.data);
    if (!rawYaml) {
      const document = bundle.data?.documents[0];
      setRawYaml(document?.rawYaml ?? document?.rawYAML ?? fallback);
    }
  }, [baseConfig, bundle.data, bundle.isPending, fallback, rawYaml]);

  const yamlError = rawYaml ? validateYaml(rawYaml) : null;
  const guidedDraft = useMemo(() => {
    if (!rawYaml) return { value: null, error: null };
    try {
      return { value: guidedConfigFromYaml(rawYaml), error: null };
    } catch (error) {
      return { value: null, error: errorMessage(error) };
    }
  }, [rawYaml]);
  const guided = guidedDraft.value;
  const documentId =
    serverDocument?.documentId ?? serverDocument?.id ?? "app.yaml";
  const change: ConfigChange = {
    mode: "yaml",
    documents: [{ documentId, rawYaml }],
  };

  const previewMutation = useMutation({
    mutationFn: (input: {
      deploymentId: string;
      change: ConfigChange;
      etag: string;
      rawYaml: string;
    }) =>
      api.previewDeploymentConfig(input.deploymentId, input.change, input.etag),
    onSuccess: (value, input) => {
      if (
        input.deploymentId !== deployment.id ||
        rawYamlRef.current !== input.rawYaml
      )
        return;
      setPreview({
        value,
        etag: input.etag,
        rawYaml: input.rawYaml,
        idempotencyKey: crypto.randomUUID(),
      });
      if (isInvalidPreviewError(saveMutation.error)) saveMutation.reset();
    },
  });
  const matchingPreview =
    preview && preview.etag === baseConfig?.etag && preview.rawYaml === rawYaml
      ? preview.value
      : null;
  const saveMutation = useMutation({
    mutationFn: (input: {
      deploymentId: string;
      change: ConfigChange;
      etag: string;
      previewToken: string;
      idempotencyKey: string;
      rawYaml: string;
    }) =>
      api.saveDeploymentConfig(
        input.deploymentId,
        input.change,
        input.etag,
        input.previewToken,
        input.idempotencyKey,
      ),
    onSuccess: (value, input) => {
      if (input.deploymentId !== deployment.id) return;
      setSaved({
        operation: value,
        rawYaml: input.rawYaml,
        startedAt: Date.now(),
      });
      setPreview(null);
      setPublicationConflict(false);
      void queryClient.invalidateQueries({ queryKey: ["operations"] });
    },
    onError: (error, input) => {
      if (input.deploymentId !== deployment.id || !isInvalidPreviewError(error))
        return;
      setPreview((current) =>
        current?.value.previewToken === input.previewToken &&
        current.idempotencyKey === input.idempotencyKey
          ? null
          : current,
      );
    },
  });

  const operation = useQuery({
    queryKey: ["operation", saved?.operation.id],
    queryFn: () => api.operation(saved!.operation.id),
    initialData: saved?.operation,
    enabled: Boolean(saved?.operation.id),
    retry: false,
    refetchOnWindowFocus: false,
    refetchInterval: (query) =>
      query.state.error ||
      (terminalSaveStates.has(query.state.data?.state.toLowerCase() ?? "") &&
        (!successfulSaveStates.has(
          query.state.data?.state.toLowerCase() ?? "",
        ) ||
          query.state.data?.pullRequest?.state !== "open")) ||
      !saved ||
      Date.now() - saved.startedAt >= savePollWindow
        ? false
        : 2_000,
  });
  const saveState = operation.data?.state.toLowerCase() ?? "";
  const saveTerminal = terminalSaveStates.has(saveState);
  const saveSuccessful = successfulSaveStates.has(saveState);
  const pullRequest = saveSuccessful ? operation.data?.pullRequest : undefined;
  // A successful review operation means the PR was created. Its public state
  // does not prove merge publication, so only an explicit reload replaces it.
  const needsPublishedBase =
    !pullRequest &&
    saveTerminal &&
    (saveSuccessful || Boolean(operation.data?.gitRevision));
  const publishedConfig = useQuery({
    queryKey: [
      "deployment-config-publication",
      deployment.id,
      saved?.operation.id,
      operation.data?.gitRevision,
    ],
    queryFn: () =>
      api.deploymentConfig(
        deployment.id,
        operation.data?.gitRevision
          ? { atLeastRevision: operation.data.gitRevision, waitSeconds: 5 }
          : undefined,
      ),
    enabled: Boolean(
      saved && needsPublishedBase && appliedOperationId !== saved.operation.id,
    ),
    retry: false,
    refetchOnWindowFocus: false,
    refetchInterval: (query) =>
      query.state.error instanceof ApiError &&
      query.state.error.problem?.code === "GitProjectionNotReady" &&
      saved &&
      Date.now() - saved.startedAt < savePollWindow
        ? 2_000
        : false,
  });
  useEffect(() => {
    if (
      !saved ||
      !publishedConfig.data ||
      appliedOperationId === saved.operation.id
    )
      return;
    const refreshed = publishedConfig.data;
    const newerDraft = rawYamlRef.current !== saved.rawYaml;
    // A revision fence allows descendants. A later change to this same App
    // must not silently authorize overwriting another actor's configuration.
    const conflict =
      newerDraft &&
      Boolean(operation.data?.gitRevision) &&
      refreshed.configRevision !== operation.data?.gitRevision;
    setPublicationConflict(conflict);
    if (!conflict) {
      setBaseConfig(refreshed);
      queryClient.setQueryData(["deployment-config", deployment.id], refreshed);
    }
    if (!newerDraft) {
      const document = refreshed.documents[0];
      setRawYaml(document?.rawYaml ?? document?.rawYAML ?? saved.rawYaml);
    }
    setPreview(null);
    setAppliedOperationId(saved.operation.id);
    void queryClient.invalidateQueries({
      queryKey: ["deployment-status", deployment.id],
    });
    void queryClient.invalidateQueries({ queryKey: ["operations"] });
  }, [
    appliedOperationId,
    deployment.id,
    operation.data?.gitRevision,
    publishedConfig.data,
    queryClient,
    saved,
  ]);
  const savePending = Boolean(
    saved &&
    (!saveTerminal ||
      pullRequest?.state === "open" ||
      (needsPublishedBase && appliedOperationId !== saved.operation.id)),
  );
  const saveCheckError = operation.error ?? publishedConfig.error;
  const [saveCheckPaused, setSaveCheckPaused] = useState(false);
  useEffect(() => {
    setSaveCheckPaused(false);
    if (!savePending || !saved) return;
    const timer = setTimeout(
      () => setSaveCheckPaused(true),
      Math.max(0, saved.startedAt + savePollWindow - Date.now()),
    );
    return () => clearTimeout(timer);
  }, [savePending, saved]);

  const checkSaveStatus = () => {
    setSaved((current) =>
      current ? { ...current, startedAt: Date.now() } : current,
    );
    void operation.refetch();
    if (needsPublishedBase && appliedOperationId !== saved?.operation.id)
      void publishedConfig.refetch();
  };

  const [reloadDraftChanged, setReloadDraftChanged] = useState(false);
  const reloadSaved = useMutation({
    mutationFn: (_draft: string) => api.deploymentConfig(deployment.id),
    onMutate: () => setReloadDraftChanged(false),
    onSuccess: (refreshed, draft) => {
      if (rawYamlRef.current !== draft) {
        setReloadDraftChanged(true);
        return;
      }
      const document = refreshed.documents[0];
      setBaseConfig(refreshed);
      setRawYaml(document?.rawYaml ?? document?.rawYAML ?? "");
      queryClient.setQueryData(["deployment-config", deployment.id], refreshed);
      setSaved(null);
      setAppliedOperationId(null);
      setPublicationConflict(false);
      setPreview(null);
      setDraftError(null);
      previewMutation.reset();
      saveMutation.reset();
    },
  });

  const updateYaml = (value: string) => {
    if (!canWriteConfig) return;
    setRawYaml(value);
    setPreview(null);
    setDraftError(null);
  };

  const switchTab = (next: typeof tab) => {
    if (next === "form" && yamlError) {
      setDraftError(
        "Fix the YAML error before returning to the guided form. The invalid draft has not been discarded.",
      );
      return;
    }
    setDraftError(null);
    setTab(next);
  };

  // A tablist owns one tab stop and answers the arrow keys. Without this the
  // role is announced to a screen reader but the keyboard behaviour it implies
  // is missing.
  const editorTabProps = useRovingFocus(
    3,
    tab === "form" ? 0 : tab === "yaml" ? 1 : 2,
  );

  return (
    <div className="[&>[data-slot='notice']]:mt-4 [&>[data-slot='notice']]:mx-5 [&>[data-slot='notice']]:mb-0">
      <div className="flex items-center justify-between gap-4 py-3 px-5 border-b border-b-line bg-surface-soft to-580:items-start to-580:flex-col">
        <div
          className="[&_button:focus-visible]:outline-[3px] [&_button:focus-visible]:outline-focus [&_button:focus-visible]:outline-offset-[2px] flex items-center gap-1 [&_button]:inline-flex [&_button]:min-h-[31px] [&_button]:items-center [&_button]:gap-1.5 [&_button]:py-0 [&_button]:px-3 [&_button]:border [&_button]:border-transparent [&_button]:rounded-[7px] [&_button]:text-ink-faint [&_button]:bg-transparent [&_button]:cursor-pointer [&_button]:text-meta [&_button]:font-semibold [&_button]:transition-[color] [&_button]:duration-(--motion-fast) [&_button]:ease-(--ease-standard) [&_button_svg]:w-[13px] [&_button.active]:text-ink [&_button.active]:border-line [&_button.active]:bg-surface [&_button.active]:shadow-[0_1px_3px_rgba(15_34_26_0.06)] to-580:max-w-full to-580:overflow-x-auto pointer-coarse:[&_button]:min-h-10 [&_button:hover:not(:disabled)]:text-ink"
          role="tablist"
          aria-label="Configuration editor mode"
        >
          <button
            role="tab"
            type="button"
            aria-selected={tab === "form"}
            className={tab === "form" ? "active" : ""}
            onClick={() => switchTab("form")}
            {...editorTabProps(0)}
          >
            <Icon name="settings" /> Guided
          </button>
          <button
            role="tab"
            type="button"
            aria-selected={tab === "yaml"}
            className={tab === "yaml" ? "active" : ""}
            onClick={() => switchTab("yaml")}
            {...editorTabProps(1)}
          >
            <Icon name="code" /> Advanced YAML
          </button>
          <button
            role="tab"
            type="button"
            aria-selected={tab === "rendered"}
            className={tab === "rendered" ? "active" : ""}
            onClick={() => switchTab("rendered")}
            {...editorTabProps(2)}
          >
            <Icon name="layers" /> Rendered manifests
          </button>
        </div>
        <div className="flex items-center gap-2 [&_code]:text-ink-faint [&_code]:text-xs to-580:hidden">
          {serverDocument?.lockedPointers?.length ? (
            <PlaceholderBadge>
              {serverDocument.lockedPointers.length} locked fields
            </PlaceholderBadge>
          ) : null}
          {/* An unlabelled short hash in the corner told the operator nothing;
              name it, and make it copyable for support threads. */}
          <span className="inline-flex items-center gap-1.5 text-ink-faint text-xs [&_code]:text-ink [&_code]:text-xs">
            <span>Config revision</span>
            <code>
              {baseConfig?.configRevision?.slice(0, 9) ?? "local draft"}
            </code>
            {baseConfig?.configRevision ? (
              <CopyButton
                value={baseConfig.configRevision}
                label="Copy config revision"
              />
            ) : null}
          </span>
        </div>
      </div>

      {bundle.error ? (
        <Notice tone="warning">
          <div>
            <strong>Configuration could not be loaded</strong>
            <p>
              {errorMessage(bundle.error)} The editor is showing a local draft
              and save actions are disabled.
            </p>
          </div>
          <PlaceholderBadge>Local preview</PlaceholderBadge>
        </Notice>
      ) : null}
      {draftError ? (
        <Notice tone="error" role="alert">
          {draftError}
        </Notice>
      ) : null}
      {!capabilities.isPending && !canWriteConfig ? (
        <Notice tone="warning">
          <div>
            <strong>Configuration is read-only</strong>
            <p>
              You can inspect Guided and Advanced YAML safely, but preview and
              commit require effective App configuration write capability at a
              covering scope.
            </p>
          </div>
          <PlaceholderBadge>Read-only</PlaceholderBadge>
        </Notice>
      ) : null}

      {tab === "form" && guided ? (
        <GuidedConfigForm
          key={`${documentId}-${baseConfig?.etag ?? "fallback"}`}
          initial={guided}
          externalDNSCatalog={externalDNSCatalog.data}
          externalDNSRuntimeEnabled={
            capabilities.data?.features?.externalDNS === true
          }
          externalDNSCatalogPending={
            externalDNSConfigured && externalDNSCatalog.isPending
          }
          externalDNSCatalogError={
            externalDNSCatalog.error
              ? errorMessage(externalDNSCatalog.error)
              : externalDNSConfigured
                ? undefined
                : "External DNS integration configuration is not enabled."
          }
          runtimeSecretApplicationId={application.id}
          runtimeSecretEnvironmentId={deployment.environmentId}
          runtimeSecretReferencesEnabled={
            capabilities.data?.features?.secretBindings === true
          }
          reusableMiddlewareProfilesEnabled={
            capabilities.data?.features?.middlewareProfiles === true
          }
          runtimeSecretReferencesUnavailableReason="Runtime-secret references remain read-only until the strict Sealed Secrets runtime and exact Git reference transaction are ready."
          certificateReferencesEnabled={
            capabilities.data?.features?.customCertificates === true
          }
          certificateReferencesUnavailableReason="Custom-certificate selection remains closed until the exact certificate lifecycle, runtime observation, and protected desired-state readiness boundary is healthy. Existing Advanced YAML is preserved."
          certificateIssuersEnabled={
            capabilities.data?.features?.certManager === true
          }
          certificateIssuersUnavailableReason="Let's Encrypt selection remains closed until the exact cert-manager profile and approved ClusterIssuers are freshly observed."
          sslipHostnameEnabled={sslipEnabled}
          sslipHostnamePreview={sslipHostname.data}
          sslipHostnamePending={sslipEnabled && sslipHostname.isPending}
          sslipHostnameError={
            sslipHostname.error
              ? "A fresh exact public ingress IP observation is unavailable for this environment."
              : undefined
          }
          readOnly={!canWriteConfig}
          middlewareEditingUnavailableReason={
            middlewareEditingUnavailableReason
          }
          onChange={(values) => {
            if (!canWriteConfig) return;
            try {
              updateYaml(applyGuidedConfig(rawYaml, values));
            } catch (error) {
              setDraftError(errorMessage(error));
            }
          }}
        />
      ) : null}
      {tab === "form" && !guided && rawYaml ? (
        <Notice tone="warning" role="alert">
          <div>
            <strong>This configuration needs Advanced YAML</strong>
            <p>
              Guided mode cannot safely represent this draft:{" "}
              {guidedDraft.error} The original YAML is preserved without
              modification.
            </p>
          </div>
          <Button
            type="button"
            variant="secondary"
            onClick={() => setTab("yaml")}
          >
            Inspect Advanced YAML
          </Button>
        </Notice>
      ) : null}
      {tab === "yaml" ? (
        <div className="bg-surface">
          <div className="flex items-center gap-3 py-3 px-5 text-ink-soft border-b border-b-line bg-surface-soft [&_svg]:w-4 [&_svg]:h-4 [&_svg]:flex-none [&_svg]:text-mint-dark [&_span]:flex [&_span]:min-w-0 [&_span]:flex-col [&_strong]:text-ink [&_strong]:text-meta [&_strong]:font-semibold [&_small]:mt-0.5 [&_small]:text-xs [&_small]:leading-[1.45]">
            <Icon name="code" />
            <span>
              <strong>One canonical AppConfig draft</strong>
              <small>
                Edits made here appear in Guided mode. Unknown and YAML-only
                fields are retained.
              </small>
            </span>
          </div>
          {yamlError ? (
            <div
              className="py-2 px-5 text-[#8b2f2f] border-b border-b-[#efc8c8] bg-[#fff4f4] font-mono text-xs"
              role="alert"
            >
              {yamlError}
            </div>
          ) : null}
          <MonacoYamlEditor
            value={rawYaml}
            onChange={updateYaml}
            readOnly={!canWriteConfig}
          />
        </div>
      ) : null}
      {tab === "rendered" ? (
        <div className="p-5">
          {matchingPreview ? (
            <section
              className="mt-0 mx-5 mb-5 overflow-hidden border border-[#283b33] rounded-[9px] bg-[#0c1511] [&_pre]:max-h-[400px] [&_pre]:m-0 [&_pre]:overflow-auto [&_pre]:p-4 [&_pre]:text-[#c9ded4] [&_pre]:text-meta [&_pre]:leading-[1.7]"
              aria-label="Rendered manifest diff"
            >
              <div className="flex items-center justify-between py-3 px-4 border-b border-b-[#26362f] [&_h3]:mt-1 [&_h3]:mx-0 [&_h3]:mb-0 [&_h3]:text-white [&_h3]:text-[11px]">
                <div>
                  <Eyebrow>Pinned runtime output</Eyebrow>
                  <h3>Rendered Kubernetes manifest diff</h3>
                  <small>
                    {matchingPreview.renderIdentity.chartName}@
                    {matchingPreview.renderIdentity.chartVersion} · Helm{" "}
                    {matchingPreview.renderIdentity.rendererVersion}
                  </small>
                </div>
                <PlaceholderBadge>
                  {matchingPreview.renderIdentityDigest.slice(0, 18)}
                </PlaceholderBadge>
              </div>
              <pre>
                {matchingPreview.renderedDiff ||
                  "No rendered Kubernetes manifest changes were produced."}
              </pre>
              <small>
                ConfigMap and literal environment values are redacted before
                this bounded diff leaves the server.
              </small>
            </section>
          ) : (
            <EmptyState
              icon="layers"
              title="Preview the exact draft to render manifests"
              description="Rendering uses the platform-pinned kuberploy-runtime chart and Helm identity. Any draft edit or renderer rollout invalidates the preview authority."
              action={<PlaceholderBadge>Preview required</PlaceholderBadge>}
              compact
            />
          )}
        </div>
      ) : null}

      <div className="flex items-center justify-end gap-2 py-4 px-5 border-t border-t-line bg-surface-soft [&>div:first-child]:flex [&>div:first-child]:flex-1 [&>div:first-child]:flex-col [&_strong]:text-meta [&_small]:mt-0.5 [&_small]:text-ink-faint [&_small]:text-xs to-580:items-stretch to-580:flex-col to-580:[&>div:first-child]:mb-1.5">
        <div>
          <strong>
            {matchingPreview
              ? "Preview is bound to this exact draft"
              : "Preview before committing"}
          </strong>
          <small>
            {matchingPreview
              ? `Expires ${matchingPreview.expiresAt} · any edit invalidates it`
              : "No workload is mutated by validation or preview."}
          </small>
        </div>
        <Button
          variant="secondary"
          onClick={() =>
            previewMutation.mutate({
              change,
              deploymentId: deployment.id,
              etag: baseConfig?.etag ?? "",
              rawYaml,
            })
          }
          busy={previewMutation.isPending}
          disabled={
            !canWriteConfig ||
            !baseConfig ||
            Boolean(bundle.error) ||
            savePending ||
            Boolean(pullRequest) ||
            saveMutation.isPending ||
            Boolean(yamlError || draftError)
          }
        >
          <Icon name="git" /> Preview configuration
        </Button>
        <Button
          onClick={() =>
            saveMutation.mutate({
              change,
              deploymentId: deployment.id,
              etag: baseConfig?.etag ?? "",
              previewToken: matchingPreview?.previewToken ?? "",
              idempotencyKey: preview?.idempotencyKey ?? "",
              rawYaml,
            })
          }
          busy={saveMutation.isPending}
          disabled={
            !canWriteConfig ||
            savePending ||
            Boolean(pullRequest) ||
            Boolean(bundle.error) ||
            !matchingPreview ||
            previewMutation.isPending ||
            Boolean(yamlError || draftError)
          }
        >
          Commit configuration <Icon name="arrow" />
        </Button>
      </div>
      {previewMutation.error ? (
        <Notice tone="error">
          <p>{errorMessage(previewMutation.error)}</p>
        </Notice>
      ) : null}
      {saveMutation.error ? (
        <Notice tone="error">
          <p>
            {isInvalidPreviewError(saveMutation.error)
              ? "This preview expired or the repository or configuration changed. Review your draft, then choose Preview configuration again. Your edits are preserved."
              : errorMessage(saveMutation.error)}
          </p>
        </Notice>
      ) : null}
      {saved ? (
        <Notice
          tone={
            saveCheckError ||
            saveCheckPaused ||
            publicationConflict ||
            pullRequest?.state === "closed" ||
            (saveTerminal && !saveSuccessful)
              ? "warning"
              : savePending || pullRequest
                ? "info"
                : "success"
          }
          role="status"
        >
          <div>
            <strong>
              {saveCheckError || saveCheckPaused
                ? "Save status needs attention"
                : publicationConflict
                  ? "Configuration changed while you were editing"
                  : pullRequest
                    ? pullRequest.state === "open"
                      ? "Awaiting pull request merge"
                      : "Pull request closed"
                    : saveTerminal && !saveSuccessful
                      ? "Configuration update failed"
                      : savePending
                        ? "Saving configuration"
                        : "Configuration saved"}
            </strong>
            <p>
              {saveCheckError
                ? errorMessage(saveCheckError)
                : saveCheckPaused
                  ? "Automatic status checks paused. Your draft is preserved; check again to continue."
                  : publicationConflict
                    ? "Your save finished, then this App changed again. Your newer draft and its original revision are preserved. Copy your draft before reloading the saved configuration, then review and preview your edits again."
                    : pullRequest
                      ? "Your change was submitted for review. Creating or closing a pull request does not confirm that it was applied. Your draft is preserved. Review the pull request, then explicitly load the currently saved configuration when you are ready to replace this draft."
                      : saveTerminal && !saveSuccessful
                        ? (operation.data?.problem?.detail ??
                          "The change did not finish. Your draft is preserved; review the operation and preview again after resolving the problem.")
                        : savePending
                          ? "Your change is being published and applied. You can keep editing; preview becomes available when this save finishes."
                          : "The saved configuration is loaded. Any newer edits remain in your draft."}
            </p>
            <a
              className={buttonVariants({ variant: "secondary" })}
              href={`/operations/${encodeURIComponent(saved.operation.id)}`}
            >
              View save operation
            </a>
            {pullRequest ? (
              <>
                <a
                  className={buttonVariants({ variant: "secondary" })}
                  href={pullRequest.url}
                  target="_blank"
                  rel="noreferrer"
                >
                  Review pull request
                </a>
                <p>
                  Replacing the draft discards its unsaved edits and loads the
                  App's current saved configuration.
                </p>
                <Button
                  variant="secondary"
                  onClick={() => reloadSaved.mutate(rawYaml)}
                  busy={reloadSaved.isPending}
                >
                  Replace draft with saved configuration
                </Button>
              </>
            ) : null}
            {saveCheckError ||
            saveCheckPaused ||
            pullRequest ||
            (saveTerminal && !saveSuccessful) ? (
              <Button
                variant="secondary"
                onClick={checkSaveStatus}
                busy={operation.isFetching || publishedConfig.isFetching}
              >
                Check save status
              </Button>
            ) : null}
          </div>
        </Notice>
      ) : null}
      {reloadSaved.error ? (
        <Notice tone="warning" role="alert">
          <p>
            {errorMessage(reloadSaved.error)} Your draft has been preserved.
          </p>
        </Notice>
      ) : null}
      {reloadDraftChanged ? (
        <Notice tone="warning" role="status">
          <strong>Draft changed during reload</strong>
          <p>
            Your newer edits and original revision are preserved. Replace the
            draft again only when you are ready to discard those edits.
          </p>
        </Notice>
      ) : null}
      {reloadSaved.data && !saved && !reloadDraftChanged ? (
        <Notice tone="info" role="status">
          <strong>Saved configuration loaded</strong>
          <p>
            The editor now shows the App's current saved configuration. This
            does not confirm whether the pull request was merged.
          </p>
        </Notice>
      ) : null}

      {matchingPreview ? (
        <section className="mt-0 mx-5 mb-5 overflow-hidden border border-[#283b33] rounded-[9px] bg-[#0c1511] [&_pre]:max-h-[400px] [&_pre]:m-0 [&_pre]:overflow-auto [&_pre]:p-4 [&_pre]:text-[#c9ded4] [&_pre]:text-meta [&_pre]:leading-[1.7]">
          <div className="flex items-center justify-between py-3 px-4 border-b border-b-[#26362f] [&_h3]:mt-1 [&_h3]:mx-0 [&_h3]:mb-0 [&_h3]:text-white [&_h3]:text-[11px]">
            <div>
              <Eyebrow>Exact candidate</Eyebrow>
              <h3>Git diff</h3>
            </div>
            <PlaceholderBadge>
              {matchingPreview.warnings.length} warnings
            </PlaceholderBadge>
          </div>
          {matchingPreview.warnings.length ? (
            <ul className="m-0 py-3 px-8 text-[#ffd694] border-b border-b-[#3b3424] bg-[#241f14] text-meta">
              {matchingPreview.warnings.map((warning, index) => (
                <li key={index}>{warning}</li>
              ))}
            </ul>
          ) : null}
          <pre>
            {matchingPreview.gitDiff || "No textual Git diff was produced."}
          </pre>
        </section>
      ) : null}
    </div>
  );
}
