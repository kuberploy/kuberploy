import { useQueries, useQuery } from "@tanstack/react-query";
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { api } from "../api/client";
import { BuildDefinitionForm } from "../components/BuildDefinitionForm";
import { HelmApplicationsPanel } from "../components/HelmApplicationsPanel";
import { RegistryPullCredentialsPanel } from "../components/RegistryPullCredentialsPanel";
import { GitSSHSourcePanel } from "../components/GitSSHSourcePanel";
import { Icon } from "../components/Icon";
import {
  compatibleBuildRegistryTargets,
  hasBuildApplicationCapability,
} from "../lib/buildAccess";
import { gitRefLabel, shortId } from "../lib/format";
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

type SourceKind = "build" | "image" | "ssh" | "helm";
type WorkspaceTab = "overview" | "source" | "runtime";

function compactImageReference(image?: string) {
  if (!image) return "Image pending";
  const [repository, digest] = image.split("@sha256:");
  const name = repository?.split("/").at(-1) || repository || image;
  return digest ? `${name}@sha256:${digest.slice(0, 12)}…` : name;
}

function applicationSourceTab(kind: string): SourceKind {
  return kind === "oci"
    ? "image"
    : kind === "git-ssh"
      ? "ssh"
      : kind === "helm"
        ? "helm"
        : "build";
}

function applicationSourceLabel(
  kind: string,
  definition?: { triggerRef: string },
) {
  if (kind === "oci") return "OCI image";
  if (kind === "git-ssh") {
    return definition
      ? `Git SSH / ${gitRefLabel(definition.triggerRef)}`
      : "Git SSH";
  }
  if (kind === "helm") return "Helm chart";
  return definition
    ? `GitHub / ${gitRefLabel(definition.triggerRef)}`
    : "GitHub App";
}

export function ApplicationOverviewPage() {
  const {
    applicationId = "",
    projectId: routeProjectId,
    environmentId: routeEnvironmentId,
  } = useParams({ strict: false });
  const search = useSearch({ strict: false }) as {
    tab?: string;
    source?: string;
    environmentId?: string;
  };
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
  const features = capabilities.data?.features;
  const featureStates = capabilities.data?.featureStates;
  const buildsConfigured = featureStates?.builds
    ? featureStates.builds !== "disabled"
    : features?.builds === true;
  const buildsReady = features?.builds === true && features?.builder === true;
  const gitSSHBuildsConfigured = featureStates?.gitSSHBuilds
    ? featureStates.gitSSHBuilds !== "disabled"
    : features?.gitSSHBuilds === true;
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
    enabled: buildsConfigured && canReadBuildDefinitions,
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

  const projectEnvironments = useMemo(
    () =>
      environments.data?.items.filter(
        (item) => item.projectId === application.data?.projectId,
      ) ?? [],
    [application.data?.projectId, environments.data?.items],
  );
  const placementQueries = useQueries({
    queries: projectEnvironments.map((environment) => ({
      queryKey: ["environment-apps", environment.id],
      queryFn: () => api.environmentApps(environment.id),
      retry: false,
    })),
  });
  const applicationEnvironments = useMemo(
    () =>
      projectEnvironments.filter((_, index) =>
        placementQueries[index]?.data?.items.some(
          (placement) => placement.applicationId === applicationId,
        ),
      ),
    [applicationId, placementQueries, projectEnvironments],
  );
  const placementsPending = placementQueries.some((query) => query.isPending);
  useEffect(() => {
    setTab(search.tab === "source" ? "source" : "overview");
    setEnvironmentId(search.environmentId ?? routeEnvironmentId ?? "");
  }, [applicationId, routeEnvironmentId, search.environmentId, search.tab]);
  useEffect(() => {
    if (!application.data || !environments.data) return;
    if (placementsPending) return;
    if (
      environmentId &&
      !applicationEnvironments.some(
        (environment) => environment.id === environmentId,
      )
    ) {
      setEnvironmentId("");
    }
  }, [
    application.data,
    applicationEnvironments,
    environmentId,
    environments.data,
    placementsPending,
  ]);
  const selectedEnvironment = applicationEnvironments.find(
    (item) => item.id === environmentId,
  );
  const applicationDeployments =
    deployments.data?.items.filter(
      (item) => item.applicationId === applicationId,
    ) ?? [];
  const source = applicationSourceTab(application.data?.sourceKind ?? "oci");
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
  const loadError =
    application.error ??
    projects.error ??
    environments.error ??
    placementQueries.find((query) => query.error)?.error;

  if (loadError) {
    return <ErrorPanel error={loadError} onRetry={() => location.reload()} />;
  }
  if (
    application.isPending ||
    projects.isPending ||
    environments.isPending ||
    placementsPending
  ) {
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
  if (
    routeEnvironmentId &&
    (routeProjectId !== application.data.projectId ||
      !applicationEnvironments.some(
        (environment) => environment.id === routeEnvironmentId,
      ))
  ) {
    return (
      <EmptyState
        title="Environment App unavailable"
        description="This App is not placed in the requested Project and Environment, or your access changed."
        action={
          <Link
            to="/projects/$projectId/environments/$environmentId"
            params={{
              projectId: routeProjectId ?? "",
              environmentId: routeEnvironmentId,
            }}
            className="button button--secondary"
          >
            Back to Environment
          </Link>
        }
      />
    );
  }

  return (
    <div className="page">
      <PageHeader
        eyebrow={project.name}
        title={application.data.name}
        description="Manage this App's source, runtime image access, and Environment instances."
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
        aria-label="App sections"
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
                  {applicationSourceLabel(
                    application.data.sourceKind ?? "oci",
                    activeBuildDefinition,
                  )}
                </strong>
                <button className="text-link" onClick={() => setTab("source")}>
                  Manage source <Icon name="arrow" />
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
                <small>App instances</small>
                <strong>{applicationDeployments.length}</strong>
                <span>Created from an Environment</span>
              </div>
            </Card>
          </section>

          <Card className="service-deployments-card">
            <div className="section-heading">
              <div>
                <span className="eyebrow">Environments</span>
                <h2>Environment instances</h2>
                <p>
                  Open an App instance to manage configuration, releases, logs,
                  and metrics.
                </p>
              </div>
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
                        Open App <Icon name="arrow" />
                      </span>
                    </Link>
                  );
                })}
              </div>
            ) : (
              <EmptyState
                compact
                icon="deploy"
                title="No App instance yet"
                description="Open an Environment and use Add App to create its stopped draft."
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
              <span className="eyebrow">App setup</span>
              <h2>App source</h2>
              <p>Choose how Kuberploy should deliver this App.</p>
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
              disabled={source !== "build"}
            >
              <Icon name="git" />
              GitHub / Dockerfile
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={source === "image"}
              disabled={source !== "image"}
            >
              <Icon name="deploy" />
              Existing image
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={source === "ssh"}
              disabled={source !== "ssh"}
            >
              <Icon name="terminal" />
              Git SSH
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={source === "helm"}
              disabled={source !== "helm"}
            >
              <Icon name="layers" />
              Helm chart
            </button>
          </div>
        </Card>
      ) : null}

      {tab === "source" && source === "build" ? (
        <div className="page-stack">
          <Card className="service-settings-card">
            {!buildsConfigured ? (
              <EmptyState
                icon="git"
                title="Source builds are disabled"
                description="The installation must configure the Source Builds API before a build definition can be created."
              />
            ) : (
              <>
                {!buildsReady ? (
                  <div className="notice notice--warning">
                    <div>
                      <strong>Builder runtime unavailable</strong>
                      <p>
                        Source configuration remains editable. Build execution
                        resumes after a matching worker and an eligible builder
                        node report Ready. Dedicated scheduling is used only
                        when node isolation is enabled.
                      </p>
                    </div>
                  </div>
                ) : null}
                {activeBuildDefinition ? (
                  <div className="notice notice--info">
                    <div>
                      <strong>Active immutable definition</strong>
                      <p>
                        GitHub / {gitRefLabel(activeBuildDefinition.triggerRef)}
                        {" · "}
                        {activeBuildDefinition.dockerfilePath}
                      </p>
                      <small>
                        {activeBuildDefinition.platforms.join(", ")} ·{" "}
                        {activeBuildDefinition.registry.server} ·{" "}
                        {shortId(activeBuildDefinition.definitionDigest, 12)}
                      </small>
                      <p>
                        Form below starts a new immutable definition. Existing
                        definition stays in history until its replacement is
                        verified.
                      </p>
                    </div>
                  </div>
                ) : null}
                <BuildDefinitionForm
                  key={application.data.id}
                  application={application.data}
                  project={project}
                  capabilities={effectiveCapabilities}
                  humanSession={humanSession}
                  registryTargets={compatibleBuildRegistryTargets(
                    registry.data?.items ?? [],
                    project.id,
                    application.data.id,
                  )}
                />
              </>
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
              description="Use a public image or select a Project pull credential, then configure the App runtime. Image tags are resolved to an immutable digest before saving."
              action={
                <Link
                  to="/deploy"
                  search={{
                    projectId: project.id,
                    environmentId:
                      environmentId || applicationEnvironments[0]?.id,
                    applicationId: application.data.id,
                  }}
                  className="button button--primary"
                >
                  Configure OCI App <Icon name="arrow" />
                </Link>
              }
            />
          </Card>
        </div>
      ) : null}

      {tab === "source" && source === "ssh" ? (
        <Card className="service-settings-card">
          <GitSSHSourcePanel
            application={application.data}
            project={project}
            enabled={features?.gitSSH === true}
            buildConfigured={gitSSHBuildsConfigured}
            buildReady={features?.gitSSHBuilds === true}
            canManageBuilds={Boolean(
              humanSession &&
              hasBuildApplicationCapability(
                effectiveCapabilities,
                "build-definitions:write",
                application.data,
                project,
              ),
            )}
            registryTargets={compatibleBuildRegistryTargets(
              registry.data?.items ?? [],
              project.id,
              application.data.id,
            )}
          />
        </Card>
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
              description="No placeholder workload is required; selecting the target Environment is enough to configure Helm desired state."
            />
          )}
        </div>
      ) : null}

      {tab === "runtime" ? (
        <RegistryPullCredentialsPanel
          key={`${project.id}:${application.data.id}`}
          application={application.data}
          project={project}
          enabled={features?.registry === true}
          canManage={canManageApplicationRegistry}
        />
      ) : null}
    </div>
  );
}
