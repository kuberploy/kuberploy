import {
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  Link,
  useNavigate,
  useParams,
  useSearch,
} from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "../api/client";
import { BuildDefinitionForm } from "../components/BuildDefinitionForm";
import { HelmApplicationsPanel } from "../components/HelmApplicationsPanel";
import { RegistryPullCredentialsPanel } from "../components/RegistryPullCredentialsPanel";
import { GitSSHSourcePanel } from "../components/GitSSHSourcePanel";
import { Icon } from "../components/Icon";
import type { IconName } from "../components/Icon";
import {
  compatibleBuildRegistryTargets,
  hasBuildApplicationCapability,
} from "../lib/buildAccess";
import { gitRefLabel, shortId } from "../lib/format";
import { hasRegistryApplicationCapability } from "../lib/registryAccess";
import {
  Select,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  Notice,
  Page,
  PageHeader,
  PageStack,
  Skeleton,
  StatusPill,
  buttonVariants,
} from "../components/ui";
import { canDeleteApplication } from "../lib/appCreationAccess";

type SourceKind = "build" | "image" | "ssh" | "helm";
type WorkspaceTab = "overview" | "source" | "runtime";

function compactImageReference(image?: string) {
  if (!image) return "Image pending";
  const [repository, digest] = image.split("@sha256:");
  const name = repository?.split("/").at(-1) || repository || image;
  return digest ? `${name}@sha256:${digest.slice(0, 12)}…` : name;
}

const sourceKinds: ReadonlyArray<readonly [SourceKind, IconName, string]> = [
  ["build", "git", "GitHub / Dockerfile"],
  ["image", "deploy", "Existing image"],
  ["ssh", "terminal", "Git SSH"],
  ["helm", "layers", "Helm chart"],
];

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
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const deleteAttempt = useRef<string | null>(null);
  const [disconnectOpen, setDisconnectOpen] = useState(false);
  const disconnectAttempt = useRef<string | null>(null);
  const search = useSearch({ strict: false }) as {
    tab?: string;
    source?: string;
    environmentId?: string;
  };
  const [tab, setTab] = useState<WorkspaceTab>(
    search.tab === "source" ? "source" : "overview",
  );
  const [environmentChoice, setEnvironmentId] = useState(
    search.environmentId ?? routeEnvironmentId ?? "",
  );
  // The URL seeds this workspace; the operator can then move around inside it
  // without touching the URL. Reseeding is an adjustment made during render
  // rather than in an effect, so a new App never paints with the old App's tab.
  const workspaceSeed = `${applicationId}|${search.tab ?? ""}|${search.environmentId ?? routeEnvironmentId ?? ""}`;
  const [seededWorkspace, setSeededWorkspace] = useState(workspaceSeed);
  if (seededWorkspace !== workspaceSeed) {
    setSeededWorkspace(workspaceSeed);
    setTab(search.tab === "source" ? "source" : "overview");
    setEnvironmentId(search.environmentId ?? routeEnvironmentId ?? "");
  }
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
      "app-sources:read",
      application.data,
      project,
    ),
  );
  const canManageBuildDefinitions = Boolean(
    application.data &&
    project &&
    humanSession &&
    hasBuildApplicationCapability(
      effectiveCapabilities,
      "app-sources:write",
      application.data,
      project,
    ),
  );
  const buildDefinitions = useQuery({
    queryKey: ["app-source", applicationId],
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
  // Once placements are known, an environment the App no longer runs in is not
  // a valid selection. Derived here so the invalid id never reaches the render.
  const environmentSettled =
    Boolean(application.data) &&
    Boolean(environments.data) &&
    !placementsPending;
  const environmentId =
    !environmentSettled ||
    !environmentChoice ||
    applicationEnvironments.some(
      (environment) => environment.id === environmentChoice,
    )
      ? environmentChoice
      : "";
  const selectedEnvironment = applicationEnvironments.find(
    (item) => item.id === environmentId,
  );
  const applicationDeployments =
    deployments.data?.items.filter(
      (item) => item.applicationId === applicationId,
    ) ?? [];
  const deleteApplication = useMutation({
    mutationFn: (idempotencyKey: string) =>
      api.deleteApplication(
        applicationId,
        application.data?.name ?? "",
        idempotencyKey,
      ),
    onSuccess: async () => {
      deleteAttempt.current = null;
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["applications"] }),
        queryClient.invalidateQueries({
          queryKey: ["application", applicationId],
        }),
        ...applicationEnvironments.map((environment) =>
          queryClient.invalidateQueries({
            queryKey: ["environment-apps", environment.id],
          }),
        ),
      ]);
      if (routeEnvironmentId) {
        await navigate({
          to: "/projects/$projectId/environments/$environmentId",
          params: {
            projectId: project?.id ?? "",
            environmentId: routeEnvironmentId,
          },
        });
      } else {
        await navigate({
          to: "/projects/$projectId",
          params: { projectId: project?.id ?? "" },
        });
      }
    },
  });
  const source = applicationSourceTab(application.data?.sourceKind ?? "oci");
  const activeBuildDefinition = useMemo(
    () =>
      (buildDefinitions.data?.items ?? [])
        .filter((definition) => definition.enabled)
        .sort(
          (left, right) =>
            right.sourceRevision - left.sourceRevision,
        )[0],
    [buildDefinitions.data?.items],
  );
  const disconnectSource = useMutation({
    mutationFn: ({
      definitionId,
      key,
    }: {
      definitionId: string;
      key: string;
    }) => api.disconnectBuildDefinition(applicationId, definitionId, key),
    onSuccess: async () => {
      disconnectAttempt.current = null;
      setDisconnectOpen(false);
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ["app-source", applicationId],
        }),
        queryClient.invalidateQueries({
          queryKey: ["auto-deploy-policies", applicationId],
        }),
      ]);
    },
  });
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
    return (
      <Page>
        <Skeleton lines={7} />
      </Page>
    );
  }
  if (!application.data || !project) {
    return (
      <Page>
        <EmptyState
          title="Application scope is unavailable"
          description="The application or its project is no longer readable."
        />
      </Page>
    );
  }
  const canDelete = canDeleteApplication(capabilities.data, project);
  if (
    routeEnvironmentId &&
    (routeProjectId !== application.data.projectId ||
      !applicationEnvironments.some(
        (environment) => environment.id === routeEnvironmentId,
      ))
  ) {
    return (
      <Page>
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
              className={buttonVariants({ variant: "secondary" })}
            >
              Back to Environment
            </Link>
          }
        />
      </Page>
    );
  }

  return (
    <Page>
      <PageHeader
        eyebrow={project.name}
        title={application.data.name}
        description="Manage this App's source, runtime image access, and Environment instances."
        actions={
          <>
            <Link
              to="/projects/$projectId"
              params={{ projectId: project.id }}
              className={buttonVariants({ variant: "ghost" })}
            >
              Back to project
            </Link>
            {canDelete ? (
              <Button variant="danger" onClick={() => setDeleteOpen(true)}>
                <Icon name="close" /> Delete App
              </Button>
            ) : null}
          </>
        }
      />

      <nav
        className="[&_button:focus-visible]:outline-[3px] [&_button:focus-visible]:outline-focus [&_button:focus-visible]:outline-offset-[2px] flex gap-6 mt-[-4px] mx-0 mb-5 border-b border-b-line [&_button]:relative [&_button]:pt-0 [&_button]:px-px [&_button]:pb-[11px] [&_button]:border-0 [&_button]:text-ink-faint [&_button]:bg-transparent [&_button]:cursor-pointer [&_button]:text-meta [&_button]:font-semibold [&_button]:pb-3 [&_button]:transition-[color] [&_button]:duration-(--motion-fast) [&_button]:ease-(--ease-standard) [&_button.active]:text-ink [&_button.active::after]:absolute [&_button.active::after]:right-0 [&_button.active::after]:bottom-[-1px] [&_button.active::after]:left-0 [&_button.active::after]:h-0.5 [&_button.active::after]:content-[''] [&_button.active::after]:bg-mint-dark [&_button.active::after]:origin-left [&_button.active::after]:animate-[tab-underline_var(--motion-base)_var(--ease-standard)] to-580:max-w-full to-580:gap-4 to-580:overflow-x-auto pointer-coarse:[&_button]:min-h-10 [&_button:hover:not(:disabled)]:text-ink mb-6"
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
        <PageStack>
          <section className="grid grid-cols-[repeat(auto-fit,_minmax(240px,_1fr))] gap-4">
            <Card className="grid grid-cols-[38px_minmax(0,_1fr)] items-start gap-3 !p-5 [&>div]:grid [&>div]:gap-1 [&_small]:text-ink-soft [&_small]:text-[11px] [&_span]:text-ink-soft [&_span]:text-[11px] [&_strong]:text-sm">
              <span className="grid w-9 h-9 place-items-center border border-line rounded-[9px] bg-surface-soft [&_svg]:w-4">
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
                <button
                  className="inline-flex items-center gap-1.5 py-0.5 px-0 border-0 rounded-sm text-mint-dark bg-transparent cursor-pointer text-meta font-medium whitespace-nowrap hover:underline hover:underline-offset-[3px] focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[3px] [&_svg]:w-3.5 [&_svg]:h-3.5 pointer-coarse:inline-flex pointer-coarse:min-h-8 pointer-coarse:items-center"
                  onClick={() => setTab("source")}
                >
                  Manage source <Icon name="arrow" />
                </button>
              </div>
            </Card>
            <Card className="grid grid-cols-[38px_minmax(0,_1fr)] items-start gap-3 !p-5 [&>div]:grid [&>div]:gap-1 [&_small]:text-ink-soft [&_small]:text-[11px] [&_span]:text-ink-soft [&_span]:text-[11px] [&_strong]:text-sm">
              <span className="grid w-9 h-9 place-items-center border border-line rounded-[9px] bg-surface-soft [&_svg]:w-4">
                <Icon name="layers" />
              </span>
              <div>
                <small>Environments</small>
                <strong>{applicationEnvironments.length}</strong>
                <span>Available in {project.name}</span>
              </div>
            </Card>
            <Card className="grid grid-cols-[38px_minmax(0,_1fr)] items-start gap-3 !p-5 [&>div]:grid [&>div]:gap-1 [&_small]:text-ink-soft [&_small]:text-[11px] [&_span]:text-ink-soft [&_span]:text-[11px] [&_strong]:text-sm">
              <span className="grid w-9 h-9 place-items-center border border-line rounded-[9px] bg-surface-soft [&_svg]:w-4">
                <Icon name="deploy" />
              </span>
              <div>
                <small>App instances</small>
                <strong>{applicationDeployments.length}</strong>
                <span>Created from an Environment</span>
              </div>
            </Card>
          </section>

          <Card className="!p-6">
            <div className="[&_p]:mx-0 [&_p]:mt-[5px] [&_p]:max-w-[680px] [&_p]:text-xs [&_p]:text-ink-soft">
              <div>
                <Eyebrow>Environments</Eyebrow>
                <h2>Environment instances</h2>
                <p>
                  Open an App instance to manage configuration, releases, logs,
                  and metrics.
                </p>
              </div>
            </div>
            {applicationDeployments.length ? (
              <div className="grid grid-cols-[repeat(auto-fill,_minmax(min(100%,_300px),_1fr))] gap-4 mt-5">
                {applicationDeployments.map((deployment) => {
                  const environment = applicationEnvironments.find(
                    (item) => item.id === deployment.environmentId,
                  );
                  return (
                    <Link
                      key={deployment.id}
                      to="/applications/$applicationId/deployments/$deploymentId"
                      params={{ applicationId, deploymentId: deployment.id }}
                      className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[-3px] hover:border-line-strong hover:shadow-[0_3px_12px_rgba(24_24_27_0.07)] [&>div]:flex [&>div]:items-center [&>div]:justify-between [&>div]:gap-3 [&>span:last-child]:inline-flex [&>span:last-child]:items-center [&>span:last-child]:gap-1.5 [&>span:last-child]:text-ink [&>span:last-child]:font-medium [&>span:last-child]:self-end [&>span:last-child]:text-xs [&>span:last-child_svg]:w-[13px] grid min-h-[140px] gap-4 p-4 border border-line rounded-[10px] bg-surface [&>strong]:overflow-hidden [&>strong]:text-meta [&>strong]:text-ellipsis [&>strong]:whitespace-nowrap"
                    >
                      <div>
                        <span className="inline-flex items-center gap-1.5 text-xs font-medium [&_svg]:w-3.5">
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
                    className={buttonVariants({ variant: "secondary" })}
                    onClick={() => setTab("source")}
                  >
                    Configure source
                  </button>
                }
              />
            )}
          </Card>
        </PageStack>
      ) : null}

      {tab === "source" ? (
        <Card className="!p-0 overflow-hidden">
          <div className="pt-6 px-6 pb-5 [&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-lg [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_p]:mt-1.5 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.5]">
            <div>
              <Eyebrow>App setup</Eyebrow>
              <h2>App source</h2>
              <p>Choose how Kuberploy should deliver this App.</p>
            </div>
          </div>
          {/* An App's source kind is fixed at creation. These are not tabs and
              nothing here is selectable, so they carry no tab roles — a list
              with the current item marked is what a screen reader should hear. */}
          <ul
            className="flex gap-1 m-0 py-0 px-6 border-t border-t-line bg-surface-soft list-none [&_li]:inline-flex [&_li]:min-h-[50px] [&_li]:items-center [&_li]:gap-2 [&_li]:py-0 [&_li]:px-4 [&_li]:border-b [&_li]:border-b-transparent [&_li]:text-ink-soft [&_li]:text-meta [&_li]:font-medium [&_li_svg]:w-4 [&_li[aria-current='true']]:text-ink [&_li[aria-current='true']]:border-b-ink [&_li[aria-current='true']]:bg-surface [&_li[data-inactive]]:text-ink-faint [&_li[data-inactive]_svg]:opacity-55 to-760:overflow-x-auto to-760:px-3 to-760:[&_li]:flex-none to-760:[&_li]:px-3"
            aria-label="Application source"
          >
            {sourceKinds.map(([kind, icon, label]) => (
              <li
                key={kind}
                aria-current={source === kind ? "true" : undefined}
                data-inactive={source === kind ? undefined : "true"}
              >
                <Icon name={icon} />
                {label}
              </li>
            ))}
          </ul>
        </Card>
      ) : null}

      {tab === "source" && source === "build" ? (
        <PageStack>
          <Card className="!p-0 overflow-hidden">
            {!buildsConfigured ? (
              <EmptyState
                icon="git"
                title="Source builds are disabled"
                description="The installation must configure the Source Builds API before a GitHub source can be connected."
              />
            ) : (
              <>
                {!buildsReady ? (
                  <Notice tone="warning">
                    <div>
                      <strong>Builder runtime unavailable</strong>
                      <p>
                        Source configuration remains editable. Build execution
                        resumes after a matching worker and an eligible builder
                        node report Ready. Dedicated scheduling is used only
                        when node isolation is enabled.
                      </p>
                    </div>
                  </Notice>
                ) : null}
                {activeBuildDefinition ? (
                  <Notice tone="info">
                    <div>
                      <strong>Connected GitHub source</strong>
                      <p>
                        GitHub / {gitRefLabel(activeBuildDefinition.triggerRef)}
                        {" · "}
                        {activeBuildDefinition.dockerfilePath}
                      </p>
                      <small>
                        {activeBuildDefinition.platforms.join(", ")} ·{" "}
                        {activeBuildDefinition.registry.server} ·{" "}
                        {shortId(activeBuildDefinition.sourceDigest, 12)}
                      </small>
                      <p>
                        Edit and save the App source below. Existing build
                        attempts keep their exact source snapshot.
                      </p>
                      {canManageBuildDefinitions ? (
                        <Button
                          variant="danger"
                          onClick={() => {
                            disconnectSource.reset();
                            disconnectAttempt.current = null;
                            setDisconnectOpen(true);
                          }}
                        >
                          <Icon name="close" /> Disconnect source
                        </Button>
                      ) : null}
                    </div>
                  </Notice>
                ) : null}
                <BuildDefinitionForm
                  key={`${application.data.id}:${capabilities.data?.defaults?.buildPlatform ?? "linux/amd64"}`}
                  application={application.data}
                  project={project}
                  capabilities={effectiveCapabilities}
                  defaultBuildPlatform={
                    capabilities.data?.defaults?.buildPlatform ?? "linux/amd64"
                  }
                  humanSession={humanSession}
                  source={activeBuildDefinition}
                  registryTargets={compatibleBuildRegistryTargets(
                    registry.data?.items ?? [],
                    project.id,
                    application.data.id,
                  )}
                />
              </>
            )}
          </Card>
        </PageStack>
      ) : null}

      {tab === "source" && source === "image" ? (
        <PageStack>
          <Card>
            <EmptyState
              icon="deploy"
              title="Deploy an existing image"
              description="Use a public image or select a Project pull credential, then configure the App runtime. Image tags are resolved to an exact digest before saving."
              action={
                <Link
                  to="/deploy"
                  search={{
                    projectId: project.id,
                    environmentId:
                      environmentId || applicationEnvironments[0]?.id,
                    applicationId: application.data.id,
                  }}
                  className={buttonVariants({ variant: "primary" })}
                >
                  Configure OCI App <Icon name="arrow" />
                </Link>
              }
            />
          </Card>
        </PageStack>
      ) : null}

      {tab === "source" && source === "ssh" ? (
        <Card className="!p-0 overflow-hidden">
          <GitSSHSourcePanel
            key={`${application.data.id}:${capabilities.data?.defaults?.buildPlatform ?? "linux/amd64"}`}
            application={application.data}
            project={project}
            enabled={features?.gitSSH === true}
            buildConfigured={gitSSHBuildsConfigured}
            buildReady={features?.gitSSHBuilds === true}
            canManageBuilds={Boolean(
              humanSession &&
              hasBuildApplicationCapability(
                effectiveCapabilities,
                "app-sources:write",
                application.data,
                project,
              ),
            )}
            defaultBuildPlatform={
              capabilities.data?.defaults?.buildPlatform ?? "linux/amd64"
            }
            registryTargets={compatibleBuildRegistryTargets(
              registry.data?.items ?? [],
              project.id,
              application.data.id,
            )}
          />
        </Card>
      ) : null}

      {tab === "source" && source === "helm" ? (
        <PageStack>
          <Card>
            <Field
              label="Environment"
              required
              hint="Helm desired state is scoped to this application and one environment."
            >
              <Select
                value={environmentId}
                onChange={(event) => setEnvironmentId(event.target.value)}
              >
                <option value="">Select environment</option>
                {applicationEnvironments.map((environment) => (
                  <option key={environment.id} value={environment.id}>
                    {environment.name}
                  </option>
                ))}
              </Select>
            </Field>
          </Card>
          {features?.helmDeployments !== true ? (
            <EmptyState
              title="Helm applications are not ready"
              description="The Helm desired-state feature must report ready before a release can be configured."
            />
          ) : selectedEnvironment ? (
            <HelmApplicationsPanel
              key={`${application.data.id}:${selectedEnvironment.id}`}
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
        </PageStack>
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
      {deleteOpen ? (
        <ConfirmDialog
          title={`Delete ${application.data.name}?`}
          description="Only an App with no deployments, build configuration, releases, bindings, or policies can be deleted. Audit history remains."
          confirmLabel="Delete App"
          confirmation={application.data.name}
          busy={deleteApplication.isPending}
          error={deleteApplication.error}
          icon="close"
          onCancel={() => {
            deleteApplication.reset();
            setDeleteOpen(false);
          }}
          onConfirm={() => {
            const key = deleteAttempt.current ?? crypto.randomUUID();
            deleteAttempt.current = key;
            deleteApplication.mutate(key);
          }}
        />
      ) : null}
      {disconnectOpen && activeBuildDefinition ? (
        <ConfirmDialog
          title={`Disconnect ${gitRefLabel(activeBuildDefinition.triggerRef)}?`}
          description="This removes this source connection, its completed build history, release projection, and auto-deploy policy history. It does not delete the repository, deploy key, registry images, or App. Active work must finish or be cancelled first."
          confirmLabel="Disconnect source"
          confirmation="DISCONNECT"
          busy={disconnectSource.isPending}
          error={disconnectSource.error}
          icon="close"
          onCancel={() => {
            disconnectSource.reset();
            disconnectAttempt.current = null;
            setDisconnectOpen(false);
          }}
          onConfirm={() => {
            const key = disconnectAttempt.current ?? crypto.randomUUID();
            disconnectAttempt.current = key;
            disconnectSource.mutate({
              definitionId: activeBuildDefinition.id,
              key,
            });
          }}
        />
      ) : null}
    </Page>
  );
}
