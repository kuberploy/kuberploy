import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "../api/client";
import type { BuildAttempt, BuildDefinition } from "../api/types";
import { BuildAttemptActions } from "../components/BuildAttemptActions";
import { BuildDefinitionForm } from "../components/BuildDefinitionForm";
import { AutoDeployPoliciesPanel } from "../components/AutoDeployPoliciesPanel";
import { GitHubInstallationsPanel } from "../components/GitHubInstallationsPanel";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  CardHeader,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  FieldLabel,
  Notice,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
  buttonVariants,
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
    <article className="last:border-b-0 [&_small]:text-ink-faint [&_small]:text-xs [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_code]:text-ink-faint [&_code]:text-xs grid grid-cols-[minmax(110px,_0.45fr)_minmax(160px,_0.8fr)_minmax(220px,_1.2fr)_auto] items-center gap-4 p-4 border-b border-b-line [&>[data-slot='button']]:whitespace-nowrap to-760:grid-cols-[1fr_1fr] to-760:[&>.build-attempt-actions]:col-[1_/_-1] to-520:grid-cols-[1fr] to-520:[&>[data-slot='button']]:justify-self-start">
      <div className="grid min-w-0 gap-1.5">
        <StatusPill value={attempt.state} />
        <small>Generation {attempt.generation}</small>
      </div>
      <div className="grid min-w-0 gap-1.5">
        <strong>{gitRefLabel(attempt.gitRef)}</strong>
        <code>{shortId(attempt.commitSha, 12)}</code>
      </div>
      <div className="grid min-w-0 gap-1.5 to-760:col-[1_/_-1]">
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
        className={buttonVariants({ variant: "secondary" })}
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
  // The picked id is a preference; the effective selection is derived from the
  // readable list in the same render, so losing access to an App falls back
  // immediately instead of one render later.
  const [applicationChoice, setSelectedApplicationId] = useState("");
  const [disconnectDefinition, setDisconnectDefinition] =
    useState<BuildDefinition | null>(null);
  const disconnectAttempt = useRef<string | null>(null);
  const queryClient = useQueryClient();
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
  const selectedApplicationId = readableApplications.some(
    (application) => application.id === applicationChoice,
  )
    ? applicationChoice
    : (readableApplications[0]?.id ?? "");
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
  const disconnectSource = useMutation({
    mutationFn: ({
      definition,
      key,
    }: {
      definition: BuildDefinition;
      key: string;
    }) =>
      api.disconnectBuildDefinition(
        definition.applicationId,
        definition.id,
        key,
      ),
    onSuccess: async (_, input) => {
      disconnectAttempt.current = null;
      setDisconnectDefinition(null);
      await Promise.all([
        definitions.refetch(),
        attempts.refetch(),
        queryClient.invalidateQueries({
          queryKey: ["auto-deploy-policies", input.definition.applicationId],
        }),
      ]);
    },
  });

  return (
    <Page>
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
            <Notice tone="warning">
              <div>
                <strong>Builder runtime unavailable</strong>
                <p>
                  Source configuration remains editable. Build execution resumes
                  after a matching worker and an eligible Ready builder node are
                  available. Dedicated labels and taints are required only when
                  node isolation is enabled.
                </p>
              </div>
            </Notice>
          ) : null}
          <Card className="mb-5 grid grid-cols-[minmax(0,_1fr)_minmax(260px,_0.5fr)] items-end gap-6 [&_p]:mt-1.5 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.5] [&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-base to-760:grid-cols-[1fr]">
            <div>
              <Eyebrow>Application scope</Eyebrow>
              <h2>Build workspace</h2>
              <p>
                The selector contains only applications covered by both build
                read permissions.
              </p>
            </div>
            <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
              <FieldLabel>Application</FieldLabel>
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
            <div className="grid grid-cols-[minmax(0,_1.35fr)_minmax(330px,_0.65fr)] gap-5 items-start to-1050:grid-cols-[1fr]">
              <Card className="mb-5">
                <CardHeader>
                  <div>
                    <Eyebrow>Definition</Eyebrow>
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
                </CardHeader>
                <BuildDefinitionForm
                  key={`${selectedApplication.id}:${capabilities.data?.defaults?.buildPlatform ?? "linux/amd64"}`}
                  application={selectedApplication}
                  project={selectedProject}
                  capabilities={effectiveCapabilities}
                  defaultBuildPlatform={
                    capabilities.data?.defaults?.buildPlatform ?? "linux/amd64"
                  }
                  humanSession={humanSession}
                  registryTargets={registryTargets}
                />
              </Card>

              <Card className="mb-5">
                <CardHeader>
                  <div>
                    <Eyebrow>Definitions</Eyebrow>
                    <h2>Source connections</h2>
                  </div>
                  <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
                    {definitions.data?.items.length ?? 0} definitions
                  </span>
                </CardHeader>
                {definitions.isPending ? (
                  <Skeleton lines={5} />
                ) : definitions.data?.items.length ? (
                  <div className="overflow-hidden border border-line rounded-[11px]">
                    {definitions.data.items.map((definition) => (
                      <article
                        className="last:border-b-0 [&_small]:text-ink-faint [&_small]:text-xs grid grid-cols-[minmax(0,_1fr)_auto] gap-3 p-4 border-b border-b-line [&>div:first-child]:flex [&>div:first-child]:min-w-0 [&>div:first-child]:items-center [&>div:first-child]:justify-between [&>div:first-child]:gap-3 [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_code]:text-ink-faint [&_code]:text-xs [&>small]:self-center [&>small]:text-right"
                        key={definition.id}
                      >
                        <div>
                          <strong>{gitRefLabel(definition.triggerRef)}</strong>
                          <code>
                            {shortId(definition.definitionDigest, 12)}
                          </code>
                        </div>
                        <dl className="grid col-[1_/_-1] grid-cols-[repeat(3,_minmax(0,_1fr))] gap-3 m-0 [&>div]:min-w-0 [&>div]:py-2 [&>div]:px-3 [&>div]:rounded-lg [&>div]:bg-surface-soft [&_dt]:text-ink-faint [&_dt]:text-[11px] [&_dd]:mt-1 [&_dd]:mx-0 [&_dd]:mb-0 [&_dd]:overflow-hidden [&_dd]:text-xs [&_dd]:text-ellipsis [&_dd]:whitespace-nowrap to-520:grid-cols-[1fr]">
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
                        {canCreateDefinition ? (
                          <Button
                            className="justify-self-end"
                            variant="danger"
                            onClick={() => {
                              disconnectSource.reset();
                              disconnectAttempt.current = null;
                              setDisconnectDefinition(definition);
                            }}
                          >
                            <Icon name="close" /> Disconnect source
                          </Button>
                        ) : null}
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
            <Card className="mb-5">
              <CardHeader>
                <div>
                  <Eyebrow>Builds</Eyebrow>
                  <h2>Attempt history</h2>
                  <p>
                    Results expose bounded status and image metadata—not raw
                    logs, checkout URLs, credentials, provider payloads, or
                    Jobs.
                  </p>
                </div>
                <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
                  {attempts.data?.items.length ?? 0} recent
                </span>
              </CardHeader>
              {attempts.isPending ? (
                <Skeleton lines={6} />
              ) : attempts.data?.items.length ? (
                <div className="overflow-hidden border border-line rounded-[11px]">
                  {attempts.data.items.map((attempt) => (
                    <BuildAttemptRow
                      key={`${selectedApplication.id}:${attempt.id}`}
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
      {disconnectDefinition ? (
        <ConfirmDialog
          title={`Disconnect ${gitRefLabel(disconnectDefinition.triggerRef)}?`}
          description="This removes this source connection, its completed build history, release projection, and auto-deploy policy history. It does not delete the repository, deploy key, registry images, or App. Active work must finish or be cancelled first."
          confirmLabel="Disconnect source"
          confirmation="DISCONNECT"
          busy={disconnectSource.isPending}
          error={disconnectSource.error}
          icon="close"
          onCancel={() => {
            disconnectSource.reset();
            disconnectAttempt.current = null;
            setDisconnectDefinition(null);
          }}
          onConfirm={() => {
            const key = disconnectAttempt.current ?? crypto.randomUUID();
            disconnectAttempt.current = key;
            disconnectSource.mutate({
              definition: disconnectDefinition,
              key,
            });
          }}
        />
      ) : null}
    </Page>
  );
}
