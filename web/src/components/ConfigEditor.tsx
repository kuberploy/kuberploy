import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import type { Application, ConfigChange, Deployment } from "../api/types";
import { api, errorMessage } from "../api/client";
import {
  applyGuidedConfig,
  defaultConfigYaml,
  guidedConfigFromYaml,
  validateYaml,
} from "../lib/configDraft";
import {
  Button,
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

export function ConfigEditor({
  deployment,
  application,
}: {
  deployment: Deployment;
  application: Application;
}) {
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
  const [preview, setPreview] = useState<{
    value: Awaited<ReturnType<typeof api.previewDeploymentConfig>>;
    etag: string;
    rawYaml: string;
    idempotencyKey: string;
  } | null>(null);
  const [draftError, setDraftError] = useState<string | null>(null);
  const rawYamlRef = useRef(rawYaml);
  rawYamlRef.current = rawYaml;

  const serverDocument = bundle.data?.documents[0];
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
    if (bundle.isPending || rawYaml) return;
    setRawYaml(serverDocument?.rawYaml ?? serverDocument?.rawYAML ?? fallback);
  }, [bundle.isPending, fallback, rawYaml, serverDocument]);

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
    },
  });
  const matchingPreview =
    preview && preview.etag === bundle.data?.etag && preview.rawYaml === rawYaml
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
    onSuccess: async (_value, input) => {
      if (input.deploymentId !== deployment.id) return;
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ["deployment-config", deployment.id],
        }),
        queryClient.invalidateQueries({ queryKey: ["operations"] }),
        queryClient.invalidateQueries({
          queryKey: ["deployment-status", deployment.id],
        }),
      ]);
      if (rawYamlRef.current !== input.rawYaml) return;
      setPreview(null);
      const refreshed = queryClient.getQueryData<
        Awaited<ReturnType<typeof api.deploymentConfig>>
      >(["deployment-config", deployment.id]);
      const refreshedDocument = refreshed?.documents[0];
      setRawYaml(
        refreshedDocument?.rawYaml ?? refreshedDocument?.rawYAML ?? "",
      );
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
              {bundle.data?.configRevision?.slice(0, 9) ?? "local draft"}
            </code>
            {bundle.data?.configRevision ? (
              <CopyButton
                value={bundle.data.configRevision}
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
          key={`${documentId}-${bundle.data?.etag ?? "fallback"}`}
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
              etag: bundle.data?.etag ?? "",
              rawYaml,
            })
          }
          busy={previewMutation.isPending}
          disabled={
            !canWriteConfig ||
            !bundle.data ||
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
              etag: bundle.data?.etag ?? "",
              previewToken: matchingPreview?.previewToken ?? "",
              idempotencyKey: preview?.idempotencyKey ?? "",
              rawYaml,
            })
          }
          busy={saveMutation.isPending}
          disabled={
            !canWriteConfig ||
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
          <p>{errorMessage(saveMutation.error)}</p>
        </Notice>
      ) : null}
      {saveMutation.data ? (
        <Notice tone="success">
          <div>
            <strong>Configuration operation accepted</strong>
            <p>
              Track operation <code>{saveMutation.data.id}</code> while Git,
              Argo, and rollout stages complete.
            </p>
          </div>
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
