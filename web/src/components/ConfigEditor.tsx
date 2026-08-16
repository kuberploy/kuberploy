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
import { Button, EmptyState, PlaceholderBadge } from "./ui";
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
    ? "You need an effective deployment configuration write capability covering this application and environment. The current definitions remain inspectable."
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
    setRawYaml("");
    setPreview(null);
    setDraftError(null);
  }, [deployment.id]);

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

  return (
    <div className="config-editor">
      <div className="editor-toolbar">
        <div
          className="editor-tabs"
          role="tablist"
          aria-label="Configuration editor mode"
        >
          <button
            role="tab"
            aria-selected={tab === "form"}
            className={tab === "form" ? "active" : ""}
            onClick={() => switchTab("form")}
          >
            <Icon name="settings" /> Guided
          </button>
          <button
            role="tab"
            aria-selected={tab === "yaml"}
            className={tab === "yaml" ? "active" : ""}
            onClick={() => switchTab("yaml")}
          >
            <Icon name="code" /> Advanced YAML
          </button>
          <button
            role="tab"
            aria-selected={tab === "rendered"}
            className={tab === "rendered" ? "active" : ""}
            onClick={() => switchTab("rendered")}
          >
            <Icon name="layers" /> Rendered manifests
          </button>
        </div>
        <div className="editor-toolbar__meta">
          {serverDocument?.lockedPointers?.length ? (
            <PlaceholderBadge>
              {serverDocument.lockedPointers.length} locked fields
            </PlaceholderBadge>
          ) : null}
          <code>
            {bundle.data?.configRevision?.slice(0, 9) ?? "local draft"}
          </code>
        </div>
      </div>

      {bundle.error ? (
        <div className="notice notice--warning">
          <div>
            <strong>Configuration could not be loaded</strong>
            <p>
              {errorMessage(bundle.error)} The editor is showing a local draft
              and save actions are disabled.
            </p>
          </div>
          <PlaceholderBadge>Local preview</PlaceholderBadge>
        </div>
      ) : null}
      {draftError ? (
        <div className="notice notice--error" role="alert">
          {draftError}
        </div>
      ) : null}
      {!capabilities.isPending && !canWriteConfig ? (
        <div className="notice notice--warning">
          <div>
            <strong>Configuration is read-only</strong>
            <p>
              You can inspect Guided and Advanced YAML safely, but preview and
              commit require an effective deployment configuration write
              capability at a covering scope.
            </p>
          </div>
          <PlaceholderBadge>Read-only</PlaceholderBadge>
        </div>
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
        <div className="notice notice--warning" role="alert">
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
        </div>
      ) : null}
      {tab === "yaml" ? (
        <div className="advanced-editor">
          <div className="advanced-editor__notice">
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
            <div className="yaml-diagnostic" role="alert">
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
        <div className="rendered-state">
          {matchingPreview ? (
            <section className="diff-panel" aria-label="Rendered manifest diff">
              <div className="diff-panel__header">
                <div>
                  <span className="eyebrow">Pinned runtime output</span>
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

      <div className="editor-actions">
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
        <div className="notice notice--error">
          <p>{errorMessage(previewMutation.error)}</p>
        </div>
      ) : null}
      {saveMutation.error ? (
        <div className="notice notice--error">
          <p>{errorMessage(saveMutation.error)}</p>
        </div>
      ) : null}
      {saveMutation.data ? (
        <div className="notice notice--success">
          <div>
            <strong>Configuration operation accepted</strong>
            <p>
              Track operation <code>{saveMutation.data.id}</code> while Git,
              Argo, and rollout stages complete.
            </p>
          </div>
        </div>
      ) : null}

      {matchingPreview ? (
        <section className="diff-panel">
          <div className="diff-panel__header">
            <div>
              <span className="eyebrow">Exact candidate</span>
              <h3>Git diff</h3>
            </div>
            <PlaceholderBadge>
              {matchingPreview.warnings.length} warnings
            </PlaceholderBadge>
          </div>
          {matchingPreview.warnings.length ? (
            <ul className="warning-list">
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
