import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { api } from "../api/client";
import {
  Card,
  ErrorPanel,
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
  const [tab, setTab] = useState<Tab>("overview");
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
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    enabled:
      secretFeatureEnabled ||
      certificateFeatureEnabled ||
      helmFeatureEnabled ||
      deploymentRollbackFeatureEnabled,
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
      deploymentRollbackFeatureEnabled,
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
  useEffect(() => {
    if (!tabs.includes(tab)) setTab("overview");
  }, [tab, tabs]);

  return (
    <div className="page">
      <div className="backline">
        <Link to="/projects">
          <Icon name="arrow" /> Projects
        </Link>
        <span>/</span>
        <span>{application.data?.name ?? "Application"}</span>
      </div>
      <PageHeader
        eyebrow="Application deployment"
        title={application.data?.name ?? "Loading application"}
        description={
          deployment.data
            ? `${deployment.data.image ?? deployment.data.source?.reference ?? "Image resolving"} · deployment ${shortId(deployment.data.id)}`
            : "Loading immutable release identity…"
        }
        actions={
          <>
            <StatusPill value={health} />
            <Link to="/deploy" className="button button--secondary">
              <Icon name="deploy" /> New deployment
            </Link>
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

      <nav className="page-tabs" aria-label="Application sections">
        {tabs.map((item) => (
          <button
            key={item}
            className={tab === item ? "active" : ""}
            aria-current={tab === item ? "page" : undefined}
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
          {tab === "overview" ? (
            <div className="application-overview">
              <section className="health-grid">
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
                  <Card className="health-card" key={label}>
                    <span className="health-card__icon">
                      <Icon name={icon as Parameters<typeof Icon>[0]["name"]} />
                    </span>
                    <div>
                      <small>{label}</small>
                      <strong>{titleCase(value)}</strong>
                    </div>
                    <StatusPill value={value ?? "pending"} />
                  </Card>
                ))}
              </section>
              <div className="overview-grid">
                <Card>
                  <div className="card__header card__header--inside">
                    <div>
                      <span className="eyebrow">Release</span>
                      <h2>Immutable artifact</h2>
                    </div>
                  </div>
                  <dl className="detail-list">
                    <div>
                      <dt>Image</dt>
                      <dd>
                        <code>
                          {deployment.data.image ??
                            deployment.data.source?.reference ??
                            "Not reported"}
                        </code>
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
                  </dl>
                </Card>
                <Card>
                  <div className="card__header card__header--inside">
                    <div>
                      <span className="eyebrow">Recent activity</span>
                      <h2>Delivery timeline</h2>
                    </div>
                    <button
                      className="text-link"
                      onClick={() => setTab("releases")}
                    >
                      All releases <Icon name="arrow" />
                    </button>
                  </div>
                  <OperationTimeline
                    operations={relatedOperations.slice(0, 4)}
                    empty="No operation has been correlated with this deployment yet."
                  />
                </Card>
              </div>
            </div>
          ) : null}
          {tab === "config" ? (
            <Card className="card--flush">
              <ConfigEditor
                key={deployment.data.id}
                deployment={deployment.data}
                application={application.data}
              />
            </Card>
          ) : null}
          {tab === "variables" ? (
            <RuntimeSecretsPanel
              key={application.data.id}
              application={application.data}
              environments={environments.data?.items ?? []}
              project={applicationProject}
              capabilities={effectiveCapabilities}
              featureEnabled={secretFeatureEnabled}
              humanSession={me.data?.authentication.kind === "session"}
            />
          ) : null}
          {tab === "certificates" ? (
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
          {tab === "helm" && helmEnvironment ? (
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
          {tab === "releases" ? (
            <Card>
              <div className="card__header card__header--inside">
                <div>
                  <span className="eyebrow">History</span>
                  <h2>Operations & releases</h2>
                </div>
              </div>
              <OperationTimeline
                operations={relatedOperations}
                empty="No release operations are indexed for this deployment."
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
          {tab === "artifacts" ? (
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
          {tab === "logs" ? (
            <LogsPanel
              applicationId={applicationId}
              deploymentId={deploymentId}
            />
          ) : null}
          {tab === "metrics" ? (
            <MetricsPanel deploymentId={deploymentId} />
          ) : null}
        </>
      ) : null}
    </div>
  );
}
