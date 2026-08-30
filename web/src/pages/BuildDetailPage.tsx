import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { api } from "../api/client";
import type { BuildAttempt } from "../api/types";
import { BuildAttemptActions } from "../components/BuildAttemptActions";
import { BuildLogsPanel } from "../components/BuildLogsPanel";
import { BuildPromotionPanel } from "../components/BuildPromotionPanel";
import { Icon } from "../components/Icon";
import {
  Card,
  CardHeader,
  DetailList,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Notice,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
  buttonVariants,
} from "../components/ui";
import {
  hasBuildApplicationCapability,
  hasPotentialBuildAccess,
} from "../lib/buildAccess";
import { formatDate, gitRefLabel, shortId } from "../lib/format";

const activeStates = new Set(["queued", "preparing", "running", "cancelling"]);

export function BuildDetailPage() {
  const { buildId } = useParams({ from: "/builds/$buildId" });
  const queryClient = useQueryClient();
  const [retriedAttempt, setRetriedAttempt] = useState<BuildAttempt>();
  useEffect(() => {
    setRetriedAttempt(undefined);
  }, [buildId]);
  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const buildsEnabled = capabilities.data?.features?.builds === true;
  const builderEnabled = capabilities.data?.features?.builder === true;
  const buildLogsEnabled = capabilities.data?.features?.buildLogs === true;
  const potentialAccess = hasPotentialBuildAccess(effectiveCapabilities);
  const attempt = useQuery({
    queryKey: ["build-attempt", buildId],
    queryFn: () => api.buildAttempt(buildId),
    enabled: (buildsEnabled || buildLogsEnabled) && potentialAccess,
    retry: false,
    refetchInterval: (query) =>
      query.state.data && activeStates.has(query.state.data.state)
        ? 5_000
        : false,
  });
  const application = useQuery({
    queryKey: ["application", attempt.data?.applicationId],
    queryFn: () => api.application(attempt.data?.applicationId ?? ""),
    enabled: Boolean(attempt.data?.applicationId),
    retry: false,
  });
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: api.projects,
    enabled: Boolean(attempt.data),
    retry: false,
  });
  const project = projects.data?.items.find(
    (item) => item.id === attempt.data?.projectId,
  );
  const identityMatches = Boolean(
    attempt.data &&
    application.data &&
    project &&
    application.data.id === attempt.data.applicationId &&
    application.data.projectId === attempt.data.projectId,
  );
  const canRead = Boolean(
    identityMatches &&
    application.data &&
    project &&
    hasBuildApplicationCapability(
      effectiveCapabilities,
      "builds:read",
      application.data,
      project,
    ),
  );
  const canReadLogs = Boolean(
    identityMatches &&
    application.data &&
    project &&
    hasBuildApplicationCapability(
      effectiveCapabilities,
      "logs:read",
      application.data,
      project,
    ),
  );
  const loadError =
    me.error ??
    capabilities.error ??
    attempt.error ??
    application.error ??
    projects.error;

  if (capabilities.isPending || me.isPending) {
    return (
      <Page>
        <Card>
          <Skeleton lines={9} />
        </Card>
      </Page>
    );
  }
  if (!buildsEnabled && !buildLogsEnabled) {
    return (
      <Page>
        <PageHeader eyebrow="Delivery" title="Build detail" />
        <Card>
          <EmptyState
            icon="terminal"
            title="Source builds are not ready"
            description="The build runtime is capability-gated and this installation is not advertising it."
          />
        </Card>
      </Page>
    );
  }
  if (!potentialAccess) {
    return (
      <Page>
        <PageHeader eyebrow="Delivery" title="Build detail" />
        <Card>
          <EmptyState
            icon="terminal"
            title="Build access required"
            description="A coarse action union or an Environment-only grant does not authorize App-wide build metadata."
          />
        </Card>
      </Page>
    );
  }
  if (loadError) {
    return (
      <Page>
        <PageHeader eyebrow="Delivery" title="Build detail" />
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([
              attempt.refetch(),
              application.refetch(),
              projects.refetch(),
            ])
          }
        />
      </Page>
    );
  }
  if (attempt.isPending || application.isPending || projects.isPending) {
    return (
      <Page>
        <Card>
          <Skeleton lines={9} />
        </Card>
      </Page>
    );
  }
  if (!attempt.data || !application.data || !project || !canRead) {
    return (
      <Page>
        <PageHeader eyebrow="Delivery" title="Build detail" />
        <Card>
          <EmptyState
            icon="terminal"
            title="Build not available in this scope"
            description="The attempt, App, and Project ancestry must match one effective builds.read grant."
          />
        </Card>
      </Page>
    );
  }

  const currentAttempt = attempt.data;
  return (
    <Page>
      <PageHeader
        eyebrow={`${project.name} / ${application.data.name}`}
        title={`Build ${shortId(currentAttempt.id, 12)}`}
        description="Safe, bounded attempt metadata from the isolated builder boundary."
        actions={
          <Link
            className={buttonVariants({ variant: "secondary" })}
            to="/applications/$applicationId"
            params={{ applicationId: application.data.id }}
            search={{ tab: "source" }}
          >
            <Icon name="chevron" /> Back to App builds
          </Link>
        }
      />

      {retriedAttempt ? (
        <Notice tone="success" role="status">
          <div>
            <strong>Retry queued</strong>
            <p>
              A new attempt was created from the same recorded source, registry,
              and cache settings.
            </p>
          </div>
          <Link
            className={buttonVariants({ variant: "secondary" })}
            to="/builds/$buildId"
            params={{ buildId: retriedAttempt.id }}
          >
            Open retry
          </Link>
        </Notice>
      ) : null}

      <section className="flex items-center justify-between gap-6 mb-5 p-6 rounded-panel text-ink border border-line bg-surface shadow-panel [&_h2]:my-1.5 [&_h2]:mx-0 [&_h2]:text-lg [&_code]:text-ink-soft [&_code]:text-meta [&_code]:break-words to-760:items-start to-760:flex-col">
        <div>
          <Eyebrow className="text-[#6de7b8]">Attempt state</Eyebrow>
          <h2>{gitRefLabel(currentAttempt.gitRef)}</h2>
          <code>{currentAttempt.commitSha}</code>
        </div>
        <StatusPill value={currentAttempt.state} />
      </section>

      <div className="grid grid-cols-[repeat(2,_minmax(0,_1fr))] gap-5 mb-5 to-760:grid-cols-[1fr]">
        <Card>
          <CardHeader>
            <div>
              <Eyebrow>Execution</Eyebrow>
              <h2>Attempt metadata</h2>
            </div>
          </CardHeader>
          <DetailList>
            <div>
              <dt>Attempt ID</dt>
              <dd>
                <code>{currentAttempt.id}</code>
              </dd>
            </div>
            <div>
              <dt>Definition</dt>
              <dd>
                <code>{currentAttempt.sourceId}</code>
              </dd>
            </div>
            <div>
              <dt>Generation</dt>
              <dd>{currentAttempt.generation}</dd>
            </div>
            <div>
              <dt>Executions</dt>
              <dd>
                {currentAttempt.executionAttempts} /{" "}
                {currentAttempt.maxAttempts}
              </dd>
            </div>
            <div>
              <dt>Created</dt>
              <dd>{formatDate(currentAttempt.createdAt)}</dd>
            </div>
            <div>
              <dt>Started</dt>
              <dd>{formatDate(currentAttempt.startedAt)}</dd>
            </div>
            <div>
              <dt>Completed</dt>
              <dd>{formatDate(currentAttempt.completedAt)}</dd>
            </div>
            <div>
              <dt>Failure</dt>
              <dd>{currentAttempt.failureCode ?? "None"}</dd>
            </div>
          </DetailList>
        </Card>

        <Card>
          <CardHeader>
            <div>
              <Eyebrow>Artifact</Eyebrow>
              <h2>Registry result</h2>
            </div>
          </CardHeader>
          {currentAttempt.image ? (
            <DetailList>
              <div>
                <dt>Reference</dt>
                <dd>
                  <code>{currentAttempt.image.reference}</code>
                </dd>
              </div>
              <div>
                <dt>Digest</dt>
                <dd>
                  <code>{currentAttempt.image.digest}</code>
                </dd>
              </div>
              <div>
                <dt>Platforms</dt>
                <dd>{currentAttempt.image.platforms.join(", ")}</dd>
              </div>
              <div>
                <dt>Cache</dt>
                <dd>
                  <code>{currentAttempt.cacheReference ?? "Not promoted"}</code>
                </dd>
              </div>
              <div>
                <dt>Warnings</dt>
                <dd>{currentAttempt.warnings?.join(", ") || "None"}</dd>
              </div>
            </DetailList>
          ) : (
            <EmptyState
              icon="layers"
              title="No published image yet"
              description="An image reference and digest appear only after a successful verified result."
              compact
            />
          )}
        </Card>
      </div>

      <Card className="[&_p]:mt-1.5 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.5] [&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-base flex items-start justify-between gap-6 to-760:items-start to-760:flex-col">
        <div>
          <Eyebrow>Verified release</Eyebrow>
          <h2>Promote to an environment</h2>
          <p>
            Select only the intended App target. App, Project, namespace,
            registry release, and exact image digest are derived by the server.
          </p>
        </div>
        <BuildPromotionPanel
          key={currentAttempt.id}
          attempt={currentAttempt}
          humanSession={me.data?.authentication.kind === "session"}
          gitOpsReady={
            capabilities.data?.features?.git === true &&
            capabilities.data?.features?.argo === true
          }
        />
      </Card>

      {buildLogsEnabled && canReadLogs ? (
        <BuildLogsPanel key={currentAttempt.id} attemptId={currentAttempt.id} />
      ) : (
        <Card>
          <EmptyState
            icon="logs"
            title={
              buildLogsEnabled
                ? "Build log access required"
                : "Live build logs are not ready"
            }
            description={
              buildLogsEnabled
                ? "This build is visible, but logs.read is not granted for its exact App scope."
                : "Attempt metadata remains available. Live logs stay closed until the separate verified Kubernetes log boundary is ready."
            }
            compact
          />
        </Card>
      )}

      {builderEnabled ? (
        <Card className="[&_p]:mt-1.5 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.5] [&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-base flex items-start justify-between gap-6 to-760:items-start to-760:flex-col">
          <div>
            <Eyebrow>High-risk command</Eyebrow>
            <h2>Attempt controls</h2>
            <p>
              Commands require an exact effective App permission and a human
              session in this UI.
            </p>
          </div>
          <BuildAttemptActions
            key={`${application.data.id}:${currentAttempt.id}`}
            attempt={currentAttempt}
            application={application.data}
            project={project}
            capabilities={effectiveCapabilities}
            humanSession={me.data?.authentication.kind === "session"}
            onUpdated={(updated) => {
              if (updated.id === currentAttempt.id) {
                queryClient.setQueryData(["build-attempt", buildId], updated);
              } else {
                setRetriedAttempt(updated);
              }
            }}
          />
        </Card>
      ) : (
        <Notice tone="warning">
          <div>
            <strong>Builder runtime unavailable</strong>
            <p>Attempt metadata is readable; cancel and retry stay disabled.</p>
          </div>
        </Notice>
      )}
    </Page>
  );
}
