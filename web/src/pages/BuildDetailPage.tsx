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
  EmptyState,
  ErrorPanel,
  PageHeader,
  Skeleton,
  StatusPill,
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
      <div className="page">
        <Card>
          <Skeleton lines={9} />
        </Card>
      </div>
    );
  }
  if (!buildsEnabled && !buildLogsEnabled) {
    return (
      <div className="page">
        <PageHeader eyebrow="Delivery" title="Build detail" />
        <Card>
          <EmptyState
            icon="terminal"
            title="Source builds are not ready"
            description="The build runtime is capability-gated and this installation is not advertising it."
          />
        </Card>
      </div>
    );
  }
  if (!potentialAccess) {
    return (
      <div className="page">
        <PageHeader eyebrow="Delivery" title="Build detail" />
        <Card>
          <EmptyState
            icon="terminal"
            title="Build access required"
            description="A coarse action union or an environment-only grant does not authorize application-wide build metadata."
          />
        </Card>
      </div>
    );
  }
  if (loadError) {
    return (
      <div className="page">
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
      </div>
    );
  }
  if (attempt.isPending || application.isPending || projects.isPending) {
    return (
      <div className="page">
        <Card>
          <Skeleton lines={9} />
        </Card>
      </div>
    );
  }
  if (!attempt.data || !application.data || !project || !canRead) {
    return (
      <div className="page">
        <PageHeader eyebrow="Delivery" title="Build detail" />
        <Card>
          <EmptyState
            icon="terminal"
            title="Build not available in this scope"
            description="The attempt, application, and project ancestry must match one effective builds.read grant."
          />
        </Card>
      </div>
    );
  }

  const currentAttempt = attempt.data;
  return (
    <div className="page">
      <PageHeader
        eyebrow={`${project.name} / ${application.data.name}`}
        title={`Build ${shortId(currentAttempt.id, 12)}`}
        description="Safe, bounded attempt metadata from the isolated builder boundary."
        actions={
          <Link className="button button--secondary" to="/builds">
            <Icon name="chevron" /> Back to builds
          </Link>
        }
      />

      {retriedAttempt ? (
        <div className="notice notice--success" role="status">
          <div>
            <strong>Immutable retry queued</strong>
            <p>
              A new attempt was created without changing source, definition,
              registry, or cache authority.
            </p>
          </div>
          <Link
            className="button button--secondary"
            to="/builds/$buildId"
            params={{ buildId: retriedAttempt.id }}
          >
            Open retry
          </Link>
        </div>
      ) : null}

      <section className="build-detail-hero">
        <div>
          <span className="eyebrow eyebrow--light">Attempt state</span>
          <h2>{gitRefLabel(currentAttempt.gitRef)}</h2>
          <code>{currentAttempt.commitSha}</code>
        </div>
        <StatusPill value={currentAttempt.state} />
      </section>

      <div className="build-detail-grid">
        <Card>
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">Execution</span>
              <h2>Attempt metadata</h2>
            </div>
          </div>
          <dl className="detail-list">
            <div>
              <dt>Attempt ID</dt>
              <dd>
                <code>{currentAttempt.id}</code>
              </dd>
            </div>
            <div>
              <dt>Definition</dt>
              <dd>
                <code>{currentAttempt.definitionId}</code>
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
          </dl>
        </Card>

        <Card>
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">Artifact</span>
              <h2>Registry result</h2>
            </div>
          </div>
          {currentAttempt.image ? (
            <dl className="detail-list">
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
            </dl>
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

      <Card className="build-detail-command-card">
        <div>
          <span className="eyebrow">Verified release</span>
          <h2>Promote to an environment</h2>
          <p>
            Select only deployment intent. Application, project, namespace,
            registry release, and immutable image are derived by the server.
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
        <BuildLogsPanel attemptId={currentAttempt.id} />
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
                ? "This build is visible, but logs.read is not granted for its exact application scope."
                : "Attempt metadata remains available. Live logs stay closed until the separate verified Kubernetes log boundary is ready."
            }
            compact
          />
        </Card>
      )}

      {builderEnabled ? (
        <Card className="build-detail-command-card">
          <div>
            <span className="eyebrow">High-risk command</span>
            <h2>Attempt controls</h2>
            <p>
              Commands require an exact effective application permission and a
              human session in this UI.
            </p>
          </div>
          <BuildAttemptActions
            key={currentAttempt.id}
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
        <div className="notice notice--warning">
          <div>
            <strong>Builder runtime unavailable</strong>
            <p>Attempt metadata is readable; cancel and retry stay disabled.</p>
          </div>
        </div>
      )}
    </div>
  );
}
