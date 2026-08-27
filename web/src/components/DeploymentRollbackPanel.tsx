import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import type {
  Application,
  Capability,
  Deployment,
  DeploymentRollbackCandidate,
  Environment,
  Project,
} from "../api/types";
import { hasDeploymentRollbackCapability } from "../lib/deploymentAccess";
import { formatDate, shortId } from "../lib/format";
import {
  Button,
  ButtonRow,
  CardHeader,
  ErrorPanel,
  Eyebrow,
  Notice,
  Skeleton,
  StatusPill,
} from "./ui";

export function DeploymentRollbackPanel({
  deployment,
  application,
  environment,
  project,
  capabilities,
  featureEnabled,
  humanSession,
}: {
  deployment: Deployment;
  application: Application;
  environment: Environment;
  project?: Project;
  capabilities: Capability[];
  featureEnabled: boolean;
  humanSession: boolean;
}) {
  const queryClient = useQueryClient();
  const allowed =
    featureEnabled &&
    humanSession &&
    hasDeploymentRollbackCapability(
      capabilities,
      application,
      environment,
      project,
    );
  const sources = useQuery({
    queryKey: ["deployment-rollback-sources", deployment.id],
    queryFn: () => api.deploymentRollbackSources(deployment.id, 25),
    enabled: allowed,
    retry: false,
  });
  const gitBundle = useQuery({
    queryKey: ["deployment-config", deployment.id],
    queryFn: () => api.deploymentConfig(deployment.id),
    enabled: allowed,
    retry: false,
  });
  const [selected, setSelected] = useState<DeploymentRollbackCandidate>();
  const [confirmed, setConfirmed] = useState(false);
  const [idempotencyKey, setIdempotencyKey] = useState("");
  const rollbackSelectionRef = useRef<{
    deploymentId: string;
    sourceOperationId: string;
    idempotencyKey: string;
  } | null>(null);
  rollbackSelectionRef.current = selected
    ? {
        deploymentId: deployment.id,
        sourceOperationId: selected.sourceOperationId,
        idempotencyKey,
      }
    : null;
  const rollback = useMutation({
    mutationFn: (input: {
      deploymentId: string;
      candidate: DeploymentRollbackCandidate;
      idempotencyKey: string;
      gitETag: string;
    }) =>
      api.rollbackDeployment(
        input.deploymentId,
        input.candidate.sourceOperationId,
        input.idempotencyKey,
        input.gitETag,
      ),
    onSuccess: async (_value, input) => {
      if (input.deploymentId !== deployment.id) return;
      const current = rollbackSelectionRef.current;
      if (
        current?.deploymentId === input.deploymentId &&
        current.sourceOperationId === input.candidate.sourceOperationId &&
        current.idempotencyKey === input.idempotencyKey
      ) {
        setSelected(undefined);
        setConfirmed(false);
        setIdempotencyKey("");
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["operations"] }),
        queryClient.invalidateQueries({
          queryKey: ["deployment", deployment.id],
        }),
        queryClient.invalidateQueries({
          queryKey: ["deployment-status", deployment.id],
        }),
        queryClient.invalidateQueries({
          queryKey: ["deployment-rollback-sources", deployment.id],
        }),
      ]);
    },
  });

  if (!allowed) return null;
  return (
    <div className="grid gap-5">
      <CardHeader>
        <div>
          <Eyebrow>Rollback as new intent</Eyebrow>
          <h3>Prior successful versions</h3>
          <p>
            Select one exact prior operation. Kuberploy creates a new Git intent
            and follows this environment&apos;s direct or pull-request policy;
            it never imperatively rolls back Kubernetes or Argo.
          </p>
        </div>
      </CardHeader>
      {sources.isPending ? <Skeleton lines={3} /> : null}
      {gitBundle.isPending ? (
        <p className="">Loading the current protected Git bundle…</p>
      ) : null}
      {gitBundle.error ? (
        <ErrorPanel
          error={gitBundle.error}
          onRetry={() => void gitBundle.refetch()}
        />
      ) : null}
      {sources.error ? (
        <ErrorPanel
          error={sources.error}
          onRetry={() => void sources.refetch()}
        />
      ) : null}
      {!sources.isPending && !sources.error && !sources.data?.items.length ? (
        <p className="">No prior version is currently eligible.</p>
      ) : null}
      <div className="grid gap-4">
        {sources.data?.items.map((candidate) => (
          <div
            className="flex items-center justify-between gap-3 py-4 px-0 border-b border-b-line [&_small]:block [&_small]:mt-1.5 last:border-b-0"
            key={candidate.sourceOperationId}
          >
            <div>
              <strong>Generation {candidate.generation}</strong>
              <small>
                Operation {shortId(candidate.sourceOperationId)} ·{" "}
                {formatDate(candidate.createdAt)}
              </small>
              <code>{candidate.image}</code>
              {candidate.managedReleaseVerified ? (
                <StatusPill value="verified" label="Managed release verified" />
              ) : (
                <StatusPill
                  value="unknown"
                  label="External digest · availability unverified"
                />
              )}
            </div>
            <Button
              type="button"
              variant="secondary"
              onClick={() => {
                setSelected(candidate);
                setConfirmed(false);
                setIdempotencyKey(crypto.randomUUID());
                rollback.reset();
              }}
            >
              Select rollback
            </Button>
          </div>
        ))}
      </div>
      {selected ? (
        <Notice tone="warning">
          <div>
            <strong>Confirm generation {selected.generation}</strong>
            <p>
              A new App rollout operation will reconstruct only server-owned
              history from <code>{selected.sourceOperationId}</code>.
            </p>
            <label className="grid grid-cols-[16px_minmax(0,_1fr)] items-start gap-3 text-ink-soft cursor-pointer text-meta leading-[1.5] [&_input]:w-4 [&_input]:min-h-4 [&_input]:mt-0.5 [&_input]:mx-0 [&_input]:mb-0 [&_input]:accent-mint">
              <input
                type="checkbox"
                checked={confirmed}
                onChange={(event) => setConfirmed(event.target.checked)}
              />
              I understand this creates a new Git intent governed by this
              environment's publication policy.
            </label>
          </div>
          <ButtonRow>
            <Button
              type="button"
              variant="ghost"
              onClick={() => {
                setSelected(undefined);
                setConfirmed(false);
                setIdempotencyKey("");
              }}
            >
              Cancel
            </Button>
            <Button
              type="button"
              variant="danger"
              busy={rollback.isPending}
              disabled={
                !confirmed ||
                idempotencyKey === "" ||
                gitBundle.data?.etag === undefined
              }
              onClick={() =>
                rollback.mutate({
                  deploymentId: deployment.id,
                  candidate: selected,
                  idempotencyKey,
                  gitETag: gitBundle.data?.etag ?? "",
                })
              }
            >
              Confirm rollback
            </Button>
          </ButtonRow>
        </Notice>
      ) : null}
      {rollback.error ? <ErrorPanel error={rollback.error} /> : null}
      {rollback.data ? (
        <Notice tone="success" role="status">
          <strong>Rollback Git intent accepted</strong>
          <p>
            Operation <code>{rollback.data.id}</code> is now following the
            environment publication policy.
          </p>
        </Notice>
      ) : null}
    </div>
  );
}
