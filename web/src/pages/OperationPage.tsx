import { cn } from "@/lib/utils";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { api } from "../api/client";
import { Icon } from "../components/Icon";
import {
  Card,
  CardHeader,
  ErrorPanel,
  Eyebrow,
  FormActions,
  Notice,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
  buttonVariants,
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
    <Page narrow>
      <PageHeader
        eyebrow="Durable operation"
        title={
          operation.data ? operationTitle(operation.data.kind) : "App operation"
        }
        description="Kuberploy records the requested change, saves desired state, synchronizes it through Argo CD, and waits for the running result."
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
          <Card className="!p-0 grid grid-cols-[1.5fr_1fr_1fr] gap-px mb-4 overflow-hidden bg-line to-580:grid-cols-[1fr]">
            <div className="flex min-w-0 flex-col gap-2 py-5 px-5 bg-surface [&_span]:text-ink-faint [&_span]:text-xs [&_span]:uppercase [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_code]:overflow-hidden [&_code]:text-meta [&_code]:text-ellipsis [&_code]:whitespace-nowrap">
              <span>Operation ID</span>
              <code>{operation.data.id}</code>
            </div>
            <div className="flex min-w-0 flex-col gap-2 py-5 px-5 bg-surface [&_span]:text-ink-faint [&_span]:text-xs [&_span]:uppercase [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_code]:overflow-hidden [&_code]:text-meta [&_code]:text-ellipsis [&_code]:whitespace-nowrap">
              <span>Started</span>
              <strong>
                {formatDate(
                  operation.data.startedAt ?? operation.data.createdAt,
                )}
              </strong>
            </div>
            <div className="flex min-w-0 flex-col gap-2 py-5 px-5 bg-surface [&_span]:text-ink-faint [&_span]:text-xs [&_span]:uppercase [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_code]:overflow-hidden [&_code]:text-meta [&_code]:text-ellipsis [&_code]:whitespace-nowrap">
              <span>Git revision</span>
              <code>
                {operation.data.gitRevision ??
                  operation.data.candidateRevision ??
                  "Pending"}
              </code>
            </div>
          </Card>

          <Card>
            <CardHeader>
              <div>
                <Eyebrow>Progress</Eyebrow>
                <h2>Change progress</h2>
              </div>
            </CardHeader>
            <ol className="m-0 p-0 list-none">
              {(operation.data.steps?.length
                ? operation.data.steps
                : fallbackSteps(operation.data.state)
              ).map((step, index) => (
                <li
                  key={step.id ?? `${step.name}-${index}`}
                  className="grid min-h-[68px] grid-cols-[31px_1fr_auto] items-center gap-3 border-t border-t-line first:border-t-0 [&_strong]:text-meta [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-faint [&_p]:text-xs"
                >
                  <span
                    className={cn(
                      "grid w-7 h-7 place-items-center border rounded-full text-meta font-bold [&_svg]:w-[13px]",
                      ["succeeded", "healthy"].includes(
                        step.state.toLowerCase(),
                      )
                        ? "border-mint-line bg-mint-soft text-mint-dark"
                        : "border-line bg-surface-soft text-ink-faint",
                    )}
                  >
                    {["succeeded", "healthy"].includes(
                      step.state.toLowerCase(),
                    ) ? (
                      <Icon name="check" />
                    ) : (
                      index + 1
                    )}
                  </span>
                  <div>
                    <strong>{operationStageTitle(step.name)}</strong>
                    <p>{step.message ?? stageDescription(step.name)}</p>
                  </div>
                  <StatusPill value={step.state} />
                </li>
              ))}
            </ol>
          </Card>

          {operation.data.problem ? (
            <Notice tone="error">
              <div>
                <strong>
                  {operation.data.problem.title ?? "Operation failed"}
                </strong>
                <p>{operation.data.problem.detail}</p>
              </div>
            </Notice>
          ) : null}
          <FormActions>
            <Link to="/" className={buttonVariants({ variant: "secondary" })}>
              Back to overview
            </Link>
            {applicationId && deploymentId ? (
              <Link
                to="/applications/$applicationId/deployments/$deploymentId"
                params={{ applicationId, deploymentId }}
                className={buttonVariants({ variant: "primary" })}
              >
                Open App <Icon name="arrow" />
              </Link>
            ) : null}
          </FormActions>
        </>
      ) : null}
    </Page>
  );
}

export function operationTitle(kind: string): string {
  switch (kind) {
    case "deployment.git-write":
      return "Apply App change";
    case "deployment.config-draft-save":
      return "Save App draft";
    case "deployment.clone-draft":
      return "Clone App draft";
    case "variable-set.git-write":
      return "Apply variable changes";
    default:
      return titleCase(kind);
  }
}

export function operationStageTitle(name: string): string {
  const normalized = name.toLowerCase();
  if (normalized.includes("git")) return "Save desired state";
  if (normalized.includes("reconcil")) return "Synchronize App";
  if (normalized.includes("health")) return "Verify App health";
  if (normalized.includes("request")) return "Record request";
  return titleCase(name);
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
