import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { api } from "../api/client";
import type { RegistryTarget } from "../api/types";
import { BuildDefinitionForm } from "../components/BuildDefinitionForm";
import { HelmApplicationsPanel } from "../components/HelmApplicationsPanel";
import { RegistryPullCredentialsPanel } from "../components/RegistryPullCredentialsPanel";
import { Icon } from "../components/Icon";
import {
  hasRegistryApplicationCapability,
  hasRegistryPlatformCapability,
} from "../lib/registryAccess";
import {
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";

type SourceKind = "build" | "image" | "helm";

function uniqueTargets(targets: RegistryTarget[]) {
  return [...new Map(targets.map((target) => [target.id, target])).values()];
}

function compactImageReference(image?: string) {
  if (!image) return "Image pending";
  const [repository, digest] = image.split("@sha256:");
  const name = repository?.split("/").at(-1) || repository || image;
  return digest ? `${name}@sha256:${digest.slice(0, 12)}…` : name;
}

export function ApplicationOverviewPage() {
  const { applicationId } = useParams({
    from: "/applications/$applicationId",
  });
  const [source, setSource] = useState<SourceKind>("build");
  const [environmentId, setEnvironmentId] = useState("");
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => api.application(applicationId),
  });
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
  });
  const deployments = useQuery({
    queryKey: ["deployments"],
    queryFn: api.deployments,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const humanSession = me.data?.authentication?.kind === "session";
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const project = projects.data?.items.find(
    (item) => item.id === application.data?.projectId,
  );
  const canReadApplicationRegistry = Boolean(
    application.data &&
    project &&
    hasRegistryApplicationCapability(
      effectiveCapabilities,
      "registry:read",
      application.data,
      project,
    ),
  );
  const canManageApplicationRegistry = Boolean(
    application.data &&
    project &&
    humanSession &&
    hasRegistryApplicationCapability(
      effectiveCapabilities,
      "registry-policies:write",
      application.data,
      project,
    ),
  );
  const registry = useQuery({
    queryKey: ["application-registry", applicationId],
    queryFn: () => api.applicationRegistry(applicationId, 100),
    enabled:
      capabilities.data?.features?.registry === true &&
      canReadApplicationRegistry,
    retry: false,
  });
  const platformRegistry = useQuery({
    queryKey: ["registry-targets", 100],
    queryFn: () => api.registryTargets(100),
    enabled:
      capabilities.data?.features?.registry === true &&
      hasRegistryPlatformCapability(
        effectiveCapabilities,
        "registry-targets:read",
      ),
    retry: false,
  });

  const applicationEnvironments = useMemo(
    () =>
      environments.data?.items.filter(
        (item) => item.projectId === application.data?.projectId,
      ) ?? [],
    [application.data?.projectId, environments.data?.items],
  );
  const selectedEnvironment = applicationEnvironments.find(
    (item) => item.id === environmentId,
  );
  const applicationDeployments =
    deployments.data?.items.filter(
      (item) => item.applicationId === applicationId,
    ) ?? [];
  const features = capabilities.data?.features;
  const loadError = application.error ?? projects.error ?? environments.error;

  if (loadError) {
    return <ErrorPanel error={loadError} onRetry={() => location.reload()} />;
  }
  if (application.isPending || projects.isPending || environments.isPending) {
    return <Skeleton lines={7} />;
  }
  if (!application.data || !project) {
    return (
      <EmptyState
        title="Application scope is unavailable"
        description="The application or its project is no longer readable."
      />
    );
  }

  return (
    <div className="page-stack">
      <PageHeader
        eyebrow={project.name}
        title={application.data.name}
        description="Configure how this service is built and deployed. Start with one source type; runtime settings stay separate."
        actions={
          <Link to="/projects" className="button button--ghost">
            Back to projects
          </Link>
        }
      />

      <Card className="application-source-card">
        <div className="application-source-card__header">
          <div>
            <span className="eyebrow">Service setup</span>
            <h2>Deployment source</h2>
            <p>Choose how Kuberploy should deliver this service.</p>
          </div>
        </div>
        <div
          className="application-source-tabs"
          role="tablist"
          aria-label="Application source"
        >
          <button
            type="button"
            role="tab"
            aria-selected={source === "build"}
            onClick={() => setSource("build")}
          >
            <Icon name="git" />
            GitHub / Dockerfile
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={source === "image"}
            onClick={() => setSource("image")}
          >
            <Icon name="deploy" />
            Existing image
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={source === "helm"}
            onClick={() => setSource("helm")}
          >
            <Icon name="layers" />
            Helm / OCI
          </button>
        </div>
      </Card>

      {source === "build" ? (
        <div className="page-stack">
          <Card className="service-settings-card">
            {features?.builds !== true || features?.builder !== true ? (
              <EmptyState
                icon="git"
                title="Source builds are not ready"
                description="Both the Source Builds API and the cluster builder must report ready before a build definition can be created."
              />
            ) : (
              <BuildDefinitionForm
                application={application.data}
                project={project}
                capabilities={effectiveCapabilities}
                humanSession={humanSession}
                registryTargets={uniqueTargets([
                  ...(registry.data?.items.map((item) => item.target) ?? []),
                  ...(platformRegistry.data?.items ?? []),
                ])}
              />
            )}
          </Card>
          <details className="service-settings-disclosure">
            <summary>
              <span>
                <strong>Runtime image pull</strong>
                <small>
                  Public image or a project credential used only by Kubernetes
                </small>
              </span>
              <Icon name="chevron" />
            </summary>
            <RegistryPullCredentialsPanel
              application={application.data}
              project={project}
              enabled={features?.registry === true}
              canManage={canManageApplicationRegistry}
            />
          </details>
        </div>
      ) : null}

      {source === "image" ? (
        <div className="page-stack">
          <Card>
            <EmptyState
              icon="deploy"
              title="Deploy an existing image"
              description="Use a public image or select a project pull credential, then configure its deployment. Image tags are resolved to an immutable digest before saving."
              action={
                <Link to="/deploy" className="button button--primary">
                  Configure image deployment <Icon name="arrow" />
                </Link>
              }
            />
          </Card>
          <RegistryPullCredentialsPanel
            application={application.data}
            project={project}
            enabled={features?.registry === true}
            canManage={canManageApplicationRegistry}
          />
        </div>
      ) : null}

      {source === "helm" ? (
        <div className="page-stack">
          <Card>
            <Field
              label="Environment"
              required
              hint="Helm desired state is scoped to this application and one environment."
            >
              <select
                value={environmentId}
                onChange={(event) => setEnvironmentId(event.target.value)}
              >
                <option value="">Select environment</option>
                {applicationEnvironments.map((environment) => (
                  <option key={environment.id} value={environment.id}>
                    {environment.name}
                  </option>
                ))}
              </select>
            </Field>
          </Card>
          {features?.helmDeployments !== true ? (
            <EmptyState
              title="Helm applications are not ready"
              description="The Helm desired-state feature must report ready before a release can be configured."
            />
          ) : selectedEnvironment ? (
            <HelmApplicationsPanel
              application={application.data}
              environment={selectedEnvironment}
              project={project}
              capabilities={effectiveCapabilities}
              featureEnabled
              rollbackFeatureEnabled={features?.helmRollbacks === true}
              humanSession={humanSession}
            />
          ) : (
            <EmptyState
              title="Select an environment"
              description="No dummy deployment is required; selecting the target environment is enough to configure Helm desired state."
            />
          )}
        </div>
      ) : null}

      <Card>
        <div className="section-heading">
          <div>
            <span className="eyebrow">Deployment-bound detail</span>
            <h2>Existing deployments</h2>
          </div>
        </div>
        {applicationDeployments.length ? (
          <div className="stack">
            {applicationDeployments.map((deployment) => (
              <Link
                key={deployment.id}
                to="/applications/$applicationId/deployments/$deploymentId"
                params={{ applicationId, deploymentId: deployment.id }}
                className="scope-row scope-row--link deployment-summary-row"
              >
                <div>
                  <strong>
                    {applicationEnvironments.find(
                      (environment) =>
                        environment.id === deployment.environmentId,
                    )?.name ?? "Environment"}
                  </strong>
                  <small title={deployment.image}>
                    {compactImageReference(deployment.image)}
                  </small>
                </div>
                <StatusPill value={deployment.status ?? "pending"} />
                <Icon name="chevron" />
              </Link>
            ))}
          </div>
        ) : (
          <p className="muted-copy">No deployment exists yet.</p>
        )}
      </Card>
    </div>
  );
}
