import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { api } from "../api/client";
import { Icon } from "../components/Icon";
import {
  Card,
  ErrorPanel,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
import { formatDate, titleCase } from "../lib/format";
import type { OperationStep } from "../api/types";

const terminalStates = new Set([
  "succeeded",
  "healthy",
  "failed",
  "cancelled",
  "superseded",
  "degraded",
]);

export function OperationPage() {
  const { operationId } = useParams({ from: "/operations/$operationId" });
  const operation = useQuery({
    queryKey: ["operation", operationId],
    queryFn: () => api.operation(operationId),
    refetchInterval: (query) =>
      terminalStates.has(query.state.data?.state.toLowerCase() ?? "")
        ? false
        : 2_000,
  });

  const deploymentId =
    operation.data?.result?.deploymentId ??
    (operation.data?.targetType === "deployment"
      ? operation.data.targetId
      : undefined) ??
    (operation.data?.target?.type === "deployment"
      ? operation.data.target.id
      : undefined);
  const operationDeployment = useQuery({
    queryKey: ["deployment", deploymentId],
    queryFn: () => api.deployment(deploymentId!),
    enabled: Boolean(deploymentId),
    retry: false,
  });
  const applicationId =
    operation.data?.result?.applicationId ??
    operationDeployment.data?.applicationId;

  return (
    <div className="page page--narrow">
      <PageHeader
        eyebrow="Durable operation"
        title={
          operation.data
            ? titleCase(operation.data.kind)
            : "Deployment operation"
        }
        description="Acceptance, Git mutation, Argo reconciliation, and rollout are separate stages. This page never calls a queued operation complete early."
        actions={
          operation.data ? <StatusPill value={operation.data.state} /> : null
        }
      />
      {operation.error ? (
        <ErrorPanel
          error={operation.error}
          onRetry={() => void operation.refetch()}
        />
      ) : null}
      {operation.isPending ? (
        <Card>
          <Skeleton lines={8} />
        </Card>
      ) : operation.data ? (
        <>
          <Card className="operation-card">
            <div className="operation-card__meta">
              <span>Operation ID</span>
              <code>{operation.data.id}</code>
            </div>
            <div className="operation-card__meta">
              <span>Started</span>
              <strong>
                {formatDate(
                  operation.data.startedAt ?? operation.data.createdAt,
                )}
              </strong>
            </div>
            <div className="operation-card__meta">
              <span>Git revision</span>
              <code>
                {operation.data.gitRevision ??
                  operation.data.candidateRevision ??
                  "Pending"}
              </code>
            </div>
          </Card>

          <Card>
            <div className="card__header card__header--inside">
              <div>
                <span className="eyebrow">Progress</span>
                <h2>Release stages</h2>
              </div>
            </div>
            <ol className="stage-list">
              {(operation.data.steps?.length
                ? operation.data.steps
                : fallbackSteps(operation.data.state)
              ).map((step, index) => (
                <li
                  key={step.id ?? `${step.name}-${index}`}
                  className={`stage-list__item stage-list__item--${step.state.toLowerCase()}`}
                >
                  <span className="stage-list__number">
                    {["succeeded", "healthy"].includes(
                      step.state.toLowerCase(),
                    ) ? (
                      <Icon name="check" />
                    ) : (
                      index + 1
                    )}
                  </span>
                  <div>
                    <strong>{step.name}</strong>
                    <p>{step.message ?? stageDescription(step.name)}</p>
                  </div>
                  <StatusPill value={step.state} />
                </li>
              ))}
            </ol>
          </Card>

          {operation.data.problem ? (
            <div className="notice notice--error">
              <div>
                <strong>
                  {operation.data.problem.title ?? "Operation failed"}
                </strong>
                <p>{operation.data.problem.detail}</p>
              </div>
            </div>
          ) : null}
          <div className="form-actions">
            <Link to="/" className="button button--secondary">
              Back to overview
            </Link>
            {applicationId && deploymentId ? (
              <Link
                to="/applications/$applicationId/deployments/$deploymentId"
                params={{ applicationId, deploymentId }}
                className="button button--primary"
              >
                Open deployment <Icon name="arrow" />
              </Link>
            ) : null}
          </div>
        </>
      ) : null}
    </div>
  );
}

function fallbackSteps(state: string): OperationStep[] {
  const order = ["Requested", "Git committed", "Reconciling", "Healthy"];
  const normalized = state.toLowerCase();
  const current = normalized.includes("git")
    ? 1
    : normalized.includes("reconcil")
      ? 2
      : ["healthy", "succeeded"].includes(normalized)
        ? 3
        : 0;
  return order.map((name, index) => ({
    name,
    state: index < current ? "succeeded" : index === current ? state : "queued",
  }));
}

function stageDescription(name: string): string {
  const normalized = name.toLowerCase();
  if (normalized.includes("git"))
    return "Desired state is written through an optimistic Git revision check.";
  if (normalized.includes("reconcil"))
    return "Argo CD observes the commit and applies the runtime chart.";
  if (normalized.includes("health"))
    return "Kubernetes reports the rollout ready.";
  return "The command is recorded durably and ready for its next worker stage.";
}
