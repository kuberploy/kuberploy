import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { api } from "../api/client";
import { BuildDefinitionForm } from "../components/BuildDefinitionForm";
import { HelmApplicationsPanel } from "../components/HelmApplicationsPanel";
import { RegistryPullCredentialsPanel } from "../components/RegistryPullCredentialsPanel";
import { Icon } from "../components/Icon";
import { hasBuildApplicationCapability } from "../lib/buildAccess";
import { gitRefLabel } from "../lib/format";
import { hasRegistryApplicationCapability } from "../lib/registryAccess";
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
type WorkspaceTab = "overview" | "source" | "runtime";

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
  const [tab, setTab] = useState<WorkspaceTab>("overview");
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
  const canReadBuildDefinitions = Boolean(
    application.data &&
    project &&
    hasBuildApplicationCapability(
      effectiveCapabilities,
      "build-definitions:read",
      application.data,
      project,
    ),
  );
  const buildDefinitions = useQuery({
    queryKey: ["build-definitions", applicationId],
    queryFn: () => api.buildDefinitions(applicationId),
    enabled:
      capabilities.data?.features?.builds === true && canReadBuildDefinitions,
    retry: false,
  });
  const registry = useQuery({
    queryKey: ["application-registry", applicationId],
    queryFn: () => api.applicationRegistry(applicationId, 100),
    enabled:
      capabilities.data?.features?.registry === true &&
      canReadApplicationRegistry,
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
  const activeBuildDefinition = useMemo(
    () =>
      (buildDefinitions.data?.items ?? [])
        .filter((definition) => definition.enabled)
        .sort(
          (left, right) =>
            right.definitionGeneration - left.definitionGeneration,
        )[0],
    [buildDefinitions.data?.items],
  );
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
    <div className="page">
      <PageHeader
        eyebrow={project.name}
        title={application.data.name}
        description="Manage this service's source, runtime image access, and environment deployments."
        actions={
          <Link
            to="/projects/$projectId"
            params={{ projectId: project.id }}
            className="button button--ghost"
          >
            Back to project
          </Link>
        }
      />

      <nav
        className="page-tabs service-workspace-tabs"
        aria-label="Service sections"
      >
        {(["overview", "source", "runtime"] as const).map((item) => (
          <button
            key={item}
            className={tab === item ? "active" : ""}
            aria-current={tab === item ? "page" : undefined}
            onClick={() => setTab(item)}
          >
            {item === "overview"
              ? "Overview"
              : item === "source"
                ? "Source & build"
                : "Runtime image access"}
          </button>
        ))}
      </nav>

      {tab === "overview" ? (
        <div className="page-stack">
          <section className="service-summary-grid">
            <Card className="service-summary-card">
              <span className="service-summary-card__icon">
                <Icon name="git" />
              </span>
              <div>
                <small>Source</small>
                <strong>
                  {activeBuildDefinition
                    ? `GitHub / ${gitRefLabel(activeBuildDefinition.triggerRef)}`
                    : "Choose Git, image, or Helm"}
                </strong>
                <button className="text-link" onClick={() => setTab("source")}>
                  {activeBuildDefinition ? "Manage source" : "Configure source"}{" "}
                  <Icon name="arrow" />
                </button>
              </div>
            </Card>
            <Card className="service-summary-card">
              <span className="service-summary-card__icon">
                <Icon name="layers" />
              </span>
              <div>
                <small>Environments</small>
                <strong>{applicationEnvironments.length}</strong>
                <span>Available in {project.name}</span>
              </div>
            </Card>
            <Card className="service-summary-card">
              <span className="service-summary-card__icon">
                <Icon name="deploy" />
              </span>
              <div>
                <small>Deployments</small>
                <strong>{applicationDeployments.length}</strong>
                <Link to="/deploy" className="text-link">
                  New deployment <Icon name="arrow" />
                </Link>
              </div>
            </Card>
          </section>

          <Card className="service-deployments-card">
            <div className="section-heading">
              <div>
                <span className="eyebrow">Environments</span>
                <h2>Deployments</h2>
                <p>
                  Open an environment deployment to manage configuration,
                  releases, logs, and metrics.
                </p>
              </div>
              <Link to="/deploy" className="button button--primary">
                <Icon name="plus" /> Deploy service
              </Link>
            </div>
            {applicationDeployments.length ? (
              <div className="deployment-card-grid">
                {applicationDeployments.map((deployment) => {
                  const environment = applicationEnvironments.find(
                    (item) => item.id === deployment.environmentId,
                  );
                  return (
                    <Link
                      key={deployment.id}
                      to="/applications/$applicationId/deployments/$deploymentId"
                      params={{ applicationId, deploymentId: deployment.id }}
                      className="deployment-card"
                    >
                      <div>
                        <span className="deployment-card__environment">
                          <Icon name="layers" />
                          {environment?.name ?? "Environment"}
                        </span>
                        <StatusPill value={deployment.status ?? "pending"} />
                      </div>
                      <strong title={deployment.image}>
                        {compactImageReference(deployment.image)}
                      </strong>
                      <span>
                        Open deployment <Icon name="arrow" />
                      </span>
                    </Link>
                  );
                })}
              </div>
            ) : (
              <EmptyState
                compact
                icon="deploy"
                title="No deployment yet"
                description="Choose a source, then deploy this service to an environment."
                action={
                  <button
                    className="button button--secondary"
                    onClick={() => setTab("source")}
                  >
                    Configure source
                  </button>
                }
              />
            )}
          </Card>
        </div>
      ) : null}

      {tab === "source" ? (
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
      ) : null}

      {tab === "source" && source === "build" ? (
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
                registryTargets={
                  registry.data?.items.map((item) => item.target) ?? []
                }
              />
            )}
          </Card>
        </div>
      ) : null}

      {tab === "source" && source === "image" ? (
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
        </div>
      ) : null}

      {tab === "source" && source === "helm" ? (
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

      {tab === "runtime" ? (
        <RegistryPullCredentialsPanel
          application={application.data}
          project={project}
          enabled={features?.registry === true}
          canManage={canManageApplicationRegistry}
        />
      ) : null}
    </div>
  );
}
