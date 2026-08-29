import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import {
  Card,
  CardHeader,
  Button,
  ConfirmDialog,
  CopyButton,
  DetailList,
  ErrorPanel,
  Eyebrow,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
import { Icon } from "../components/Icon";
import { ConfigEditor } from "../components/ConfigEditor";
import { LogsPanel, MetricsPanel } from "../components/RuntimePanels";
import { OperationTimeline } from "../components/OperationTimeline";
import { RuntimeSecretsPanel } from "../components/RuntimeSecretsPanel";
import { RegistryPanel } from "../components/RegistryPanel";
import { CertificateBindingsPanel } from "../components/CertificateBindingsPanel";
import { HelmApplicationsPanel } from "../components/HelmApplicationsPanel";
import { DeploymentRollbackPanel } from "../components/DeploymentRollbackPanel";
import { certificateEnvironments } from "../lib/certificateAccess";
import { formatDate, shortId, titleCase } from "../lib/format";
import { hasHelmCapability } from "../lib/helmAccess";
import { hasRegistryApplicationCapability } from "../lib/registryAccess";
import { runtimeSecretEnvironments } from "../lib/runtimeSecretAccess";
import { hasDeploymentUpdateCapability } from "../lib/deploymentAccess";

type Tab =
  | "overview"
  | "config"
  | "variables"
  | "certificates"
  | "helm"
  | "releases"
  | "artifacts"
  | "logs"
  | "metrics";

export function ApplicationPage() {
  const { applicationId, deploymentId } = useParams({
    from: "/applications/$applicationId/deployments/$deploymentId",
  });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [stopOpen, setStopOpen] = useState(false);
  const [deployOpen, setDeployOpen] = useState(false);
  const stopAttempt = useRef<string | null>(null);
  const deployAttempt = useRef<string | null>(null);
  const [tabChoice, setTab] = useState<Tab>("overview");
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => api.application(applicationId),
  });
  const deployment = useQuery({
    queryKey: ["deployment", deploymentId],
    queryFn: () => api.deployment(deploymentId),
  });
  const status = useQuery({
    queryKey: ["deployment-status", deploymentId],
    queryFn: () => api.deploymentStatus(deploymentId),
    refetchInterval: 10_000,
    retry: false,
  });
  const operations = useQuery({
    queryKey: ["operations"],
    queryFn: api.operations,
    refetchInterval: 5_000,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const secretFeatureEnabled =
    capabilities.data?.features?.secretBindings === true;
  const registryFeatureEnabled = capabilities.data?.features?.registry === true;
  const certificateFeatureEnabled =
    capabilities.data?.features?.customCertificates === true;
  const managedRegistryFeatureEnabled =
    capabilities.data?.features?.managedRegistry === true;
  const helmFeatureEnabled =
    capabilities.data?.features?.helmDeployments === true;
  const helmRollbackFeatureEnabled =
    capabilities.data?.features?.helmRollbacks === true;
  const deploymentRollbackFeatureEnabled =
    capabilities.data?.features?.deploymentRollbacks === true;
  const deploymentManagementEnabled =
    capabilities.data?.capabilities?.some(
      (capability) =>
        capability.actions?.includes("deployments:update") === true,
    ) === true;
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    enabled:
      secretFeatureEnabled ||
      certificateFeatureEnabled ||
      helmFeatureEnabled ||
      deploymentRollbackFeatureEnabled ||
      deploymentManagementEnabled,
    retry: false,
  });
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: api.projects,
    enabled:
      secretFeatureEnabled ||
      registryFeatureEnabled ||
      certificateFeatureEnabled ||
      helmFeatureEnabled ||
      deploymentRollbackFeatureEnabled ||
      deploymentManagementEnabled,
    retry: false,
  });
  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    staleTime: 60_000,
  });
  const loadError = application.error ?? deployment.error;
  const relatedOperations =
    operations.data?.items.filter(
      (operation) =>
        operation.target?.id === deploymentId ||
        operation.targetRef?.id === deploymentId ||
        operation.result?.deploymentId === deploymentId ||
        operation.targetId === deploymentId,
    ) ?? [];
  const pullRequest = relatedOperations.find(
    (operation) => operation.pullRequest,
  )?.pullRequest;
  const health = status.data?.rolloutHealth ?? "unknown";
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const applicationProject = projects.data?.items.find(
    (project) => project.id === application.data?.projectId,
  );
  const readableSecretEnvironments = application.data
    ? runtimeSecretEnvironments(
        effectiveCapabilities,
        "secret-bindings:read",
        application.data,
        environments.data?.items ?? [],
        applicationProject,
      )
    : [];
  const showVariablesTab =
    secretFeatureEnabled && readableSecretEnvironments.length > 0;
  const readableCertificateEnvironments = application.data
    ? certificateEnvironments(
        effectiveCapabilities,
        "certificate-bindings:read",
        application.data,
        environments.data?.items ?? [],
        applicationProject,
      )
    : [];
  const showCertificatesTab =
    certificateFeatureEnabled && readableCertificateEnvironments.length > 0;
  const showArtifactsTab =
    registryFeatureEnabled &&
    Boolean(
      application.data &&
      hasRegistryApplicationCapability(
        effectiveCapabilities,
        "registry:read",
        application.data,
        applicationProject,
      ),
    );
  const helmEnvironment = environments.data?.items.find(
    (environment) => environment.id === deployment.data?.environmentId,
  );
  const canStop = Boolean(
    application.data &&
    deployment.data &&
    helmEnvironment &&
    deployment.data.state !== "stopped" &&
    deployment.data.state !== "pending-stop" &&
    hasDeploymentUpdateCapability(
      effectiveCapabilities,
      application.data,
      helmEnvironment,
      applicationProject,
    ),
  );
  const canDeploy = Boolean(
    application.data &&
    deployment.data &&
    helmEnvironment &&
    deployment.data.state !== "pending-git" &&
    deployment.data.state !== "pending-stop" &&
    hasDeploymentUpdateCapability(
      effectiveCapabilities,
      application.data,
      helmEnvironment,
      applicationProject,
    ),
  );
  const stopDeployment = useMutation({
    mutationFn: (idempotencyKey: string) =>
      api.stopDeployment(deploymentId, idempotencyKey),
    onSuccess: async (operation) => {
      stopAttempt.current = null;
      setStopOpen(false);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["deployments"] }),
        queryClient.invalidateQueries({
          queryKey: ["deployment", deploymentId],
        }),
        queryClient.invalidateQueries({ queryKey: ["operations"] }),
      ]);
      await navigate({
        to: "/operations/$operationId",
        params: { operationId: operation.id },
      });
    },
  });
  const redeployDeployment = useMutation({
    mutationFn: async (idempotencyKey: string) => {
      const config = await api.deploymentConfig(deploymentId);
      return api.redeployDeployment(deploymentId, idempotencyKey, config.etag);
    },
    onSuccess: async (operation) => {
      deployAttempt.current = null;
      setDeployOpen(false);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["deployments"] }),
        queryClient.invalidateQueries({
          queryKey: ["deployment", deploymentId],
        }),
        queryClient.invalidateQueries({ queryKey: ["operations"] }),
      ]);
      await navigate({
        to: "/operations/$operationId",
        params: { operationId: operation.id },
      });
    },
  });
  const showHelmTab = Boolean(
    helmFeatureEnabled &&
    application.data &&
    helmEnvironment &&
    hasHelmCapability(
      effectiveCapabilities,
      "helm.read",
      application.data,
      helmEnvironment,
      applicationProject,
    ),
  );
  const tabs: Tab[] = [
    "overview",
    "config",
    ...(showVariablesTab ? (["variables"] as const) : []),
    ...(showCertificatesTab ? (["certificates"] as const) : []),
    ...(showHelmTab ? (["helm"] as const) : []),
    "releases",
    ...(showArtifactsTab ? (["artifacts"] as const) : []),
    "logs",
    "metrics",
  ];
  // Which tabs exist depends on capability, so the picked tab is a preference:
  // the tab actually rendered is derived from the list in this render.
  const activeTab = tabs.includes(tabChoice) ? tabChoice : "overview";

  return (
    <Page>
      <div className="flex items-center gap-2 mb-5 text-ink-faint text-meta [&_a]:inline-flex [&_a]:items-center [&_a]:gap-1.5 [&_a]:text-mint-dark [&_a_svg]:w-3 [&_a_svg]:transform-[rotate(180deg)] pointer-coarse:[&_a]:inline-flex pointer-coarse:[&_a]:min-h-8 pointer-coarse:[&_a]:items-center">
        <Link to="/projects">
          <Icon name="arrow" /> Projects
        </Link>
        <span>/</span>
        <span>{application.data?.name ?? "Application"}</span>
      </div>
      <PageHeader
        eyebrow="App runtime"
        title={application.data?.name ?? "Loading application"}
        description={
          deployment.data
            ? `${deployment.data.image ?? deployment.data.source?.reference ?? "Image resolving"} · runtime ${shortId(deployment.data.id)}`
            : "Loading release identity…"
        }
        actions={
          <>
            <StatusPill value={health} />
            {canDeploy ? (
              <Button variant="primary" onClick={() => setDeployOpen(true)}>
                <Icon name="deploy" />
                {deployment.data?.state === "stopped"
                  ? "Start App"
                  : "Reload App"}
              </Button>
            ) : null}
            {canStop ? (
              <Button variant="danger" onClick={() => setStopOpen(true)}>
                <Icon name="close" /> Stop App
              </Button>
            ) : null}
          </>
        }
      />

      {loadError ? (
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([application.refetch(), deployment.refetch()])
          }
        />
      ) : null}
      {stopOpen ? (
        <ConfirmDialog
          title={`Stop ${application.data?.name ?? "App"}?`}
          description="This removes this Environment's desired App manifest. Argo CD prunes its workload. Configuration remains available for a later redeploy."
          confirmLabel="Stop App"
          confirmation="STOP"
          busy={stopDeployment.isPending}
          error={stopDeployment.error}
          icon="close"
          onCancel={() => {
            stopDeployment.reset();
            setStopOpen(false);
          }}
          onConfirm={() => {
            const key = stopAttempt.current ?? crypto.randomUUID();
            stopAttempt.current = key;
            stopDeployment.mutate(key);
          }}
        />
      ) : null}
      {deployOpen ? (
        <ConfirmDialog
          title={`${deployment.data?.state === "stopped" ? "Start" : "Reload"} ${application.data?.name ?? "App"}?`}
          description={
            deployment.data?.state === "stopped"
              ? "Publish this Environment's saved App configuration. Argo CD will create the workload after the Git change is accepted."
              : "Publish the same saved App configuration again and let Argo CD reconcile a fresh rollout."
          }
          confirmLabel={
            deployment.data?.state === "stopped" ? "Start App" : "Reload App"
          }
          confirmation={
            deployment.data?.state === "stopped" ? "START" : "RELOAD"
          }
          confirmationLabel="Confirm App action"
          busy={redeployDeployment.isPending}
          error={redeployDeployment.error}
          icon="deploy"
          onCancel={() => {
            redeployDeployment.reset();
            setDeployOpen(false);
          }}
          onConfirm={() => {
            const key = deployAttempt.current ?? crypto.randomUUID();
            deployAttempt.current = key;
            redeployDeployment.mutate(key);
          }}
        />
      ) : null}

      <nav
        className="[&_button:focus-visible]:outline-[3px] [&_button:focus-visible]:outline-focus [&_button:focus-visible]:outline-offset-[2px] flex gap-6 mt-[-4px] mx-0 mb-5 border-b border-b-line [&_button]:relative [&_button]:pt-0 [&_button]:px-px [&_button]:pb-[11px] [&_button]:border-0 [&_button]:text-ink-faint [&_button]:bg-transparent [&_button]:cursor-pointer [&_button]:text-meta [&_button]:font-semibold [&_button]:pb-3 [&_button]:transition-[color] [&_button]:duration-(--motion-fast) [&_button]:ease-(--ease-standard) [&_button.active]:text-ink [&_button.active::after]:absolute [&_button.active::after]:right-0 [&_button.active::after]:bottom-[-1px] [&_button.active::after]:left-0 [&_button.active::after]:h-0.5 [&_button.active::after]:content-[''] [&_button.active::after]:bg-mint-dark [&_button.active::after]:origin-left [&_button.active::after]:animate-[tab-underline_var(--motion-base)_var(--ease-standard)] to-580:max-w-full to-580:gap-4 to-580:overflow-x-auto pointer-coarse:[&_button]:min-h-10 [&_button:hover:not(:disabled)]:text-ink"
        aria-label="Application sections"
      >
        {tabs.map((item) => (
          <button
            key={item}
            className={activeTab === item ? "active" : ""}
            aria-current={activeTab === item ? "page" : undefined}
            onClick={() => setTab(item)}
          >
            {item === "config"
              ? "Configuration"
              : item === "variables"
                ? "Variables & secrets"
                : item === "certificates"
                  ? "TLS certificates"
                  : titleCase(item)}
          </button>
        ))}
      </nav>

      {application.isPending || deployment.isPending ? (
        <Card>
          <Skeleton lines={10} />
        </Card>
      ) : application.data && deployment.data ? (
        <>
          {activeTab === "overview" ? (
            <div className="grid gap-5">
              <section className="grid grid-cols-[repeat(auto-fill,_minmax(min(100%,_190px),_1fr))] gap-3 mb-4 to-1120:grid-cols-[repeat(3,_1fr)] to-580:grid-cols-[1fr]">
                {[
                  ["Desired state", status.data?.state, "git"],
                  ["Operation", status.data?.operationStatus, "refresh"],
                  [
                    "Argo sync",
                    status.data?.argoSyncStatus ?? "unknown",
                    "refresh",
                  ],
                  [
                    "Rollout health",
                    status.data?.rolloutHealth ?? "unknown",
                    "deploy",
                  ],
                  [
                    "Ready replicas",
                    status.data?.readyReplicas !== undefined &&
                    status.data?.desiredReplicas !== undefined
                      ? `${status.data.readyReplicas}/${status.data.desiredReplicas}`
                      : "unknown",
                    "deploy",
                  ],
                  [
                    "Rollout condition",
                    status.data?.rolloutConditions?.find(
                      (condition) => condition.status === "True",
                    )?.type ?? "unknown",
                    "refresh",
                  ],
                  [
                    "Git revision",
                    pullRequest && !status.data?.desiredRevision
                      ? `review-${pullRequest.state}`
                      : status.data?.desiredRevision
                        ? "committed"
                        : "pending",
                    "git",
                  ],
                  [
                    "Argo revision",
                    status.data?.argoObservedRevision
                      ? status.data.argoObservedRevision ===
                        status.data.desiredRevision
                        ? "current"
                        : "behind"
                      : "unknown",
                    "deploy",
                  ],
                  ["DNS", status.data?.dnsStatus ?? "not reported", "route"],
                  [
                    "Monitoring",
                    status.data?.monitoringStatus ?? "not reported",
                    "metrics",
                  ],
                ].map(([label, value, icon]) => (
                  <Card
                    className="grid min-w-0 grid-cols-[24px_minmax(0,_1fr)] items-center gap-y-2 gap-x-3 p-4 [&_small]:block [&_small]:overflow-hidden [&_small]:text-ink-soft [&_small]:text-xs [&_small]:font-medium [&_small]:text-ellipsis [&_small]:uppercase [&_small]:tracking-[0.06em] [&_small]:whitespace-nowrap"
                    key={label}
                  >
                    <span className="grid w-6 h-6 place-items-center text-ink-faint [&_svg]:w-4 [&_svg]:h-4">
                      <Icon name={icon as Parameters<typeof Icon>[0]["name"]} />
                    </span>
                    <small>{label}</small>
                    {/* The pill carries the value; printing it again above the
                        pill just doubled every tile's text. */}
                    <StatusPill value={value ?? "pending"} />
                  </Card>
                ))}
              </section>
              <div className="grid grid-cols-[minmax(0,_0.8fr)_minmax(0,_1.2fr)] gap-4 to-820:grid-cols-[1fr]">
                <Card>
                  <CardHeader>
                    <div>
                      <Eyebrow>Release</Eyebrow>
                      <h2>Release artifact</h2>
                    </div>
                  </CardHeader>
                  <DetailList>
                    <div>
                      <dt>Image</dt>
                      <dd>
                        {/* A digest is not something an operator can retype;
                            it always ships with a copy affordance. */}
                        <code>
                          {deployment.data.image ??
                            deployment.data.source?.reference ??
                            "Not reported"}
                        </code>
                        {(deployment.data.image ??
                        deployment.data.source?.reference) ? (
                          <CopyButton
                            value={
                              deployment.data.image ??
                              deployment.data.source?.reference ??
                              ""
                            }
                            label="Copy image reference"
                          />
                        ) : null}
                      </dd>
                    </div>
                    <div>
                      <dt>Replicas</dt>
                      <dd>{deployment.data.replicas ?? "Chart default"}</dd>
                    </div>
                    <div>
                      <dt>Container port</dt>
                      <dd>{deployment.data.port ?? "Not reported"}</dd>
                    </div>
                    <div>
                      <dt>Desired revision</dt>
                      <dd>
                        <code>
                          {status.data?.desiredRevision ??
                            deployment.data.desiredRevision ??
                            deployment.data.configRevision ??
                            "Pending first projection"}
                        </code>
                      </dd>
                    </div>
                    {pullRequest ? (
                      <div>
                        <dt>Protected publication</dt>
                        <dd>
                          <a
                            href={pullRequest.url}
                            target="_blank"
                            rel="noreferrer"
                          >
                            Pull request #{pullRequest.number} ·{" "}
                            {pullRequest.state}
                          </a>
                          {!status.data?.desiredRevision ? (
                            <small>
                              Awaiting verified merge and target indexing; the
                              candidate is not desired state.
                            </small>
                          ) : null}
                        </dd>
                      </div>
                    ) : null}
                    <div>
                      <dt>Observed revision</dt>
                      <dd>
                        <code>
                          {status.data?.argoObservedRevision ?? "Not reported"}
                        </code>
                      </dd>
                    </div>
                    <div>
                      <dt>Updated</dt>
                      <dd>{formatDate(deployment.data.updatedAt)}</dd>
                    </div>
                  </DetailList>
                </Card>
                <Card>
                  <CardHeader>
                    <div>
                      <Eyebrow>Recent activity</Eyebrow>
                      <h2>Delivery timeline</h2>
                    </div>
                    <button
                      className="inline-flex items-center gap-1.5 py-0.5 px-0 border-0 rounded-sm text-mint-dark bg-transparent cursor-pointer text-meta font-medium whitespace-nowrap hover:underline hover:underline-offset-[3px] focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[3px] [&_svg]:w-3.5 [&_svg]:h-3.5 pointer-coarse:inline-flex pointer-coarse:min-h-8 pointer-coarse:items-center"
                      onClick={() => setTab("releases")}
                    >
                      All releases <Icon name="arrow" />
                    </button>
                  </CardHeader>
                  <OperationTimeline
                    operations={relatedOperations.slice(0, 4)}
                    empty="No operation has been correlated with this App runtime yet."
                  />
                </Card>
              </div>
            </div>
          ) : null}
          {activeTab === "config" ? (
            <Card flush>
              <ConfigEditor
                key={deployment.data.id}
                deployment={deployment.data}
                application={application.data}
              />
            </Card>
          ) : null}
          {activeTab === "variables" ? (
            <RuntimeSecretsPanel
              key={`${application.data.id}:${deployment.data.environmentId}`}
              application={application.data}
              environments={environments.data?.items ?? []}
              preferredEnvironmentId={deployment.data.environmentId}
              project={applicationProject}
              capabilities={effectiveCapabilities}
              featureEnabled={secretFeatureEnabled}
              humanSession={me.data?.authentication.kind === "session"}
            />
          ) : null}
          {activeTab === "certificates" ? (
            <CertificateBindingsPanel
              key={application.data.id}
              application={application.data}
              environments={environments.data?.items ?? []}
              project={applicationProject}
              capabilities={effectiveCapabilities}
              featureEnabled={certificateFeatureEnabled}
              humanSession={me.data?.authentication.kind === "session"}
            />
          ) : null}
          {activeTab === "helm" && helmEnvironment ? (
            <HelmApplicationsPanel
              key={`${application.data.id}:${helmEnvironment.id}`}
              application={application.data}
              environment={helmEnvironment}
              project={applicationProject}
              capabilities={effectiveCapabilities}
              featureEnabled={helmFeatureEnabled}
              rollbackFeatureEnabled={helmRollbackFeatureEnabled}
              humanSession={me.data?.authentication.kind === "session"}
            />
          ) : null}
          {activeTab === "releases" ? (
            <Card>
              <CardHeader>
                <div>
                  <Eyebrow>History</Eyebrow>
                  <h2>Operations & releases</h2>
                </div>
              </CardHeader>
              <OperationTimeline
                operations={relatedOperations}
                empty="No release operations are indexed for this App runtime."
              />
              {helmEnvironment ? (
                <DeploymentRollbackPanel
                  key={`${deployment.data.id}:${helmEnvironment.id}`}
                  deployment={deployment.data}
                  application={application.data}
                  environment={helmEnvironment}
                  project={applicationProject}
                  capabilities={effectiveCapabilities}
                  featureEnabled={deploymentRollbackFeatureEnabled}
                  humanSession={me.data?.authentication.kind === "session"}
                />
              ) : null}
            </Card>
          ) : null}
          {activeTab === "artifacts" ? (
            <RegistryPanel
              key={application.data.id}
              application={application.data}
              project={applicationProject}
              capabilities={effectiveCapabilities}
              featureEnabled={registryFeatureEnabled}
              managedFeatureEnabled={managedRegistryFeatureEnabled}
              humanSession={me.data?.authentication.kind === "session"}
            />
          ) : null}
          {activeTab === "logs" ? (
            <LogsPanel
              applicationId={applicationId}
              deploymentId={deploymentId}
            />
          ) : null}
          {activeTab === "metrics" ? (
            <MetricsPanel deploymentId={deploymentId} />
          ) : null}
        </>
      ) : null}
    </Page>
  );
}
