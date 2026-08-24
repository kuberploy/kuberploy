import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { api } from "../api/client";
import type { BuildAttempt } from "../api/types";
import { BuildAttemptActions } from "../components/BuildAttemptActions";
import { BuildDefinitionForm } from "../components/BuildDefinitionForm";
import { AutoDeployPoliciesPanel } from "../components/AutoDeployPoliciesPanel";
import { GitHubInstallationsPanel } from "../components/GitHubInstallationsPanel";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
import {
  buildReadableApplications,
  compatibleBuildRegistryTargets,
  hasBuildApplicationCapability,
} from "../lib/buildAccess";
import { formatDate, gitRefLabel, shortId } from "../lib/format";
import { hasRegistryApplicationCapability } from "../lib/registryAccess";

const activeBuildStates = new Set([
  "queued",
  "preparing",
  "running",
  "cancelling",
]);

function BuildAttemptRow({
  attempt,
  application,
  project,
  capabilities,
  humanSession,
}: {
  attempt: BuildAttempt;
  application: Parameters<typeof BuildAttemptActions>[0]["application"];
  project: Parameters<typeof BuildAttemptActions>[0]["project"];
  capabilities: Parameters<typeof BuildAttemptActions>[0]["capabilities"];
  humanSession: boolean;
}) {
  return (
    <article className="build-attempt-row">
      <div className="build-attempt-row__state">
        <StatusPill value={attempt.state} />
        <small>Generation {attempt.generation}</small>
      </div>
      <div className="build-attempt-row__source">
        <strong>{gitRefLabel(attempt.gitRef)}</strong>
        <code>{shortId(attempt.commitSha, 12)}</code>
      </div>
      <div className="build-attempt-row__result">
        <strong>
          {attempt.image?.reference ?? attempt.failureCode ?? "Pending"}
        </strong>
        <small>
          {attempt.executionAttempts}/{attempt.maxAttempts} execution attempts ·{" "}
          {formatDate(attempt.updatedAt)}
        </small>
        {attempt.cacheReuse ? (
          <small>Registry cache: {attempt.cacheReuse.replace("-", " ")}</small>
        ) : null}
      </div>
      <Link
        className="button button--secondary"
        to="/builds/$buildId"
        params={{ buildId: attempt.id }}
      >
        Details <Icon name="arrow" />
      </Link>
      <BuildAttemptActions
        attempt={attempt}
        application={application}
        project={project}
        capabilities={capabilities}
        humanSession={humanSession}
      />
    </article>
  );
}

export function SourceBuildsPage() {
  const [selectedApplicationId, setSelectedApplicationId] = useState("");
  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const features = capabilities.data?.features;
  const featureStates = capabilities.data?.featureStates;
  const githubSetupEnabled = features?.githubAppSetup === true;
  const buildsEnabled = features?.builds === true;
  const buildsConfigured = featureStates?.builds
    ? featureStates.builds !== "disabled"
    : buildsEnabled;
  const builderEnabled = features?.builder === true;
  const autoDeployEnabled = features?.autoDeploy === true;
  const registryEnabled = features?.registry === true;
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const humanSession = me.data?.authentication?.kind === "session";
  const canSetupGitHub = effectiveCapabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes("github-installations:setup"),
  );
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: api.projects,
    enabled: buildsConfigured,
  });
  const applications = useQuery({
    queryKey: ["applications"],
    queryFn: api.applications,
    enabled: buildsConfigured,
  });
  const readableApplications = useMemo(
    () =>
      buildReadableApplications(
        effectiveCapabilities,
        applications.data?.items ?? [],
        projects.data?.items ?? [],
      ),
    [applications.data, effectiveCapabilities, projects.data],
  );
  useEffect(() => {
    if (
      !readableApplications.some(
        (application) => application.id === selectedApplicationId,
      )
    ) {
      setSelectedApplicationId(readableApplications[0]?.id ?? "");
    }
  }, [readableApplications, selectedApplicationId]);
  const selectedApplication = readableApplications.find(
    (application) => application.id === selectedApplicationId,
  );
  const selectedProject = projects.data?.items.find(
    (project) => project.id === selectedApplication?.projectId,
  );
  const canReadApplicationRegistry = Boolean(
    selectedApplication &&
    selectedProject &&
    hasRegistryApplicationCapability(
      effectiveCapabilities,
      "registry:read",
      selectedApplication,
      selectedProject,
    ),
  );
  const applicationRegistry = useQuery({
    queryKey: ["application-registry", selectedApplicationId],
    queryFn: () => api.applicationRegistry(selectedApplicationId, 100),
    enabled:
      buildsConfigured &&
      registryEnabled &&
      Boolean(selectedApplication) &&
      canReadApplicationRegistry,
    retry: false,
  });
  const registryTargets =
    selectedApplication && selectedProject
      ? compatibleBuildRegistryTargets(
          applicationRegistry.data?.items ?? [],
          selectedProject.id,
          selectedApplication.id,
        )
      : [];
  const definitions = useQuery({
    queryKey: ["build-definitions", selectedApplicationId],
    queryFn: () => api.buildDefinitions(selectedApplicationId),
    enabled: buildsConfigured && Boolean(selectedApplication),
    retry: false,
  });
  const attempts = useQuery({
    queryKey: ["build-attempts", selectedApplicationId],
    queryFn: () => api.buildAttempts(selectedApplicationId, 50),
    enabled: buildsConfigured && Boolean(selectedApplication),
    retry: false,
    refetchInterval: (query) =>
      query.state.data?.items.some((attempt) =>
        activeBuildStates.has(attempt.state),
      )
        ? 5_000
        : false,
  });

  const loadError =
    me.error ?? capabilities.error ?? projects.error ?? applications.error;
  const buildError =
    definitions.error ?? attempts.error ?? applicationRegistry.error;
  const loadingBuildCatalog =
    buildsConfigured &&
    (projects.isPending || applications.isPending || capabilities.isPending);
  const canCreateDefinition = Boolean(
    selectedApplication &&
    selectedProject &&
    humanSession &&
    hasBuildApplicationCapability(
      effectiveCapabilities,
      "build-definitions:write",
      selectedApplication,
      selectedProject,
    ),
  );

  return (
    <div className="page">
      <PageHeader
        eyebrow="Delivery"
        title="GitHub source builds"
        description="Create immutable, repository-scoped definitions and follow isolated multi-platform builds from verified webhooks to registry digests."
        actions={
          buildsConfigured && selectedApplication ? (
            <Button
              variant="secondary"
              onClick={() =>
                void Promise.all([
                  definitions.refetch(),
                  attempts.refetch(),
                  applicationRegistry.refetch(),
                ])
              }
            >
              <Icon name="refresh" /> Refresh
            </Button>
          ) : undefined
        }
      />

      {loadError ? (
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([
              me.refetch(),
              capabilities.refetch(),
              projects.refetch(),
              applications.refetch(),
            ])
          }
        />
      ) : null}

      <GitHubInstallationsPanel
        featureEnabled={githubSetupEnabled}
        humanSession={humanSession}
        canSetup={canSetupGitHub}
      />

      {!buildsConfigured ? (
        <Card>
          <EmptyState
            icon="terminal"
            title="Source builds are not ready"
            description="The API has not observed a matching healthy build worker, so build catalogs and commands remain disabled. GitHub installation setup can stay available independently."
          />
        </Card>
      ) : loadingBuildCatalog ? (
        <Card>
          <Skeleton lines={8} />
        </Card>
      ) : readableApplications.length === 0 ? (
        <Card>
          <EmptyState
            icon="apps"
            title="No build-readable application"
            description="Feature flags and top-level action unions do not grant access. You need effective build-definition and build-history read permissions on an application ancestor."
          />
        </Card>
      ) : (
        <>
          {!builderEnabled ? (
            <div className="notice notice--warning">
              <div>
                <strong>Builder runtime unavailable</strong>
                <p>
                  Source configuration remains editable. Build execution resumes
                  after a matching worker and a Ready dedicated node labeled and
                  tainted for DinD are available.
                </p>
              </div>
            </div>
          ) : null}
          <Card className="build-application-picker">
            <div>
              <span className="eyebrow">Application scope</span>
              <h2>Build workspace</h2>
              <p>
                The selector contains only applications covered by both build
                read permissions.
              </p>
            </div>
            <label className="field">
              <span className="field__label">Application</span>
              <select
                aria-label="Build application"
                value={selectedApplicationId}
                onChange={(event) =>
                  setSelectedApplicationId(event.target.value)
                }
              >
                {readableApplications.map((application) => {
                  const project = projects.data?.items.find(
                    (item) => item.id === application.projectId,
                  );
                  return (
                    <option key={application.id} value={application.id}>
                      {project?.name ?? "Project"} / {application.name}
                    </option>
                  );
                })}
              </select>
            </label>
          </Card>

          {buildError ? (
            <ErrorPanel
              error={buildError}
              onRetry={() =>
                void Promise.all([
                  definitions.refetch(),
                  attempts.refetch(),
                  applicationRegistry.refetch(),
                ])
              }
            />
          ) : null}

          {selectedApplication && selectedProject ? (
            <AutoDeployPoliciesPanel
              key={`${selectedProject.id}:${selectedApplication.id}`}
              application={selectedApplication}
              project={selectedProject}
              definitions={definitions.data?.items ?? []}
              enabled={autoDeployEnabled}
              humanSession={Boolean(humanSession)}
              capabilities={effectiveCapabilities}
            />
          ) : null}

          {selectedApplication && selectedProject ? (
            <div className="source-build-layout">
              <Card className="source-build-card">
                <div className="card__header card__header--inside">
                  <div>
                    <span className="eyebrow">Definition</span>
                    <h2>Create from verified source</h2>
                    <p>
                      Definitions are immutable. To change source, platforms,
                      registry, or cache behavior, create a new definition.
                    </p>
                  </div>
                  <StatusPill
                    value={canCreateDefinition ? "ready" : "read-only"}
                    label={canCreateDefinition ? "Writable" : "Read only"}
                  />
                </div>
                <BuildDefinitionForm
                  key={selectedApplication.id}
                  application={selectedApplication}
                  project={selectedProject}
                  capabilities={effectiveCapabilities}
                  humanSession={humanSession}
                  registryTargets={registryTargets}
                />
              </Card>

              <Card className="source-build-card">
                <div className="card__header card__header--inside">
                  <div>
                    <span className="eyebrow">Definitions</span>
                    <h2>Immutable history</h2>
                  </div>
                  <span className="placeholder-badge">
                    {definitions.data?.items.length ?? 0} definitions
                  </span>
                </div>
                {definitions.isPending ? (
                  <Skeleton lines={5} />
                ) : definitions.data?.items.length ? (
                  <div className="build-definition-list">
                    {definitions.data.items.map((definition) => (
                      <article
                        className="build-definition-row"
                        key={definition.id}
                      >
                        <div>
                          <strong>{gitRefLabel(definition.triggerRef)}</strong>
                          <code>
                            {shortId(definition.definitionDigest, 12)}
                          </code>
                        </div>
                        <dl className="build-definition-summary">
                          <div>
                            <dt>Platforms</dt>
                            <dd>{definition.platforms.join(", ")}</dd>
                          </div>
                          <div>
                            <dt>Registry</dt>
                            <dd>{definition.registry.server}</dd>
                          </div>
                          <div>
                            <dt>Cache</dt>
                            <dd>
                              {definition.cacheTrustLane} ·{" "}
                              {definition.cacheImports} imports
                            </dd>
                          </div>
                        </dl>
                        <StatusPill
                          value={definition.enabled ? "active" : "disabled"}
                        />
                        <small>
                          Created {formatDate(definition.createdAt)}
                        </small>
                      </article>
                    ))}
                  </div>
                ) : (
                  <EmptyState
                    icon="git"
                    title="No build definition"
                    description="Create the first immutable definition. A verified matching push will create an attempt."
                    compact
                  />
                )}
              </Card>
            </div>
          ) : null}

          {selectedApplication && selectedProject ? (
            <Card className="source-build-card">
              <div className="card__header card__header--inside">
                <div>
                  <span className="eyebrow">Builds</span>
                  <h2>Attempt history</h2>
                  <p>
                    Results expose bounded status and image metadata—not raw
                    logs, checkout URLs, credentials, provider payloads, or
                    Jobs.
                  </p>
                </div>
                <span className="placeholder-badge">
                  {attempts.data?.items.length ?? 0} recent
                </span>
              </div>
              {attempts.isPending ? (
                <Skeleton lines={6} />
              ) : attempts.data?.items.length ? (
                <div className="build-attempt-list">
                  {attempts.data.items.map((attempt) => (
                    <BuildAttemptRow
                      key={attempt.id}
                      attempt={attempt}
                      application={selectedApplication}
                      project={selectedProject}
                      capabilities={effectiveCapabilities}
                      humanSession={humanSession && builderEnabled}
                    />
                  ))}
                </div>
              ) : (
                <EmptyState
                  icon="terminal"
                  title="No build attempt"
                  description="Attempts appear after GitHub delivers a verified push matching an enabled immutable definition."
                  compact
                />
              )}
            </Card>
          ) : null}
        </>
      )}
    </div>
  );
}
