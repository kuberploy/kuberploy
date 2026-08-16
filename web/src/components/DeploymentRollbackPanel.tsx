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
import { Button, ErrorPanel, Skeleton, StatusPill } from "./ui";

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
    }) =>
      api.rollbackDeployment(
        input.deploymentId,
        input.candidate.sourceOperationId,
        input.idempotencyKey,
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
  useEffect(() => {
    setSelected(undefined);
    setConfirmed(false);
    setIdempotencyKey("");
    rollback.reset();
  }, [deployment.id]);

  if (!allowed) return null;
  return (
    <div className="deployment-rollback-panel">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">Rollback as new intent</span>
          <h3>Prior successful versions</h3>
          <p>
            Select one exact prior operation. Kuberploy creates a new Git intent
            and follows this environment&apos;s direct or pull-request policy;
            it never imperatively rolls back Kubernetes or Argo.
          </p>
        </div>
      </div>
      {sources.isPending ? <Skeleton lines={3} /> : null}
      {sources.error ? (
        <ErrorPanel
          error={sources.error}
          onRetry={() => void sources.refetch()}
        />
      ) : null}
      {!sources.isPending && !sources.error && !sources.data?.items.length ? (
        <p className="muted">No prior version is currently eligible.</p>
      ) : null}
      <div className="helm-history-list">
        {sources.data?.items.map((candidate) => (
          <div className="helm-history-item" key={candidate.sourceOperationId}>
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
        <div className="notice notice--warning">
          <div>
            <strong>Confirm generation {selected.generation}</strong>
            <p>
              A new deployment operation will reconstruct only server-owned
              history from <code>{selected.sourceOperationId}</code>.
            </p>
            <label className="checkbox-row">
              <input
                type="checkbox"
                checked={confirmed}
                onChange={(event) => setConfirmed(event.target.checked)}
              />
              I understand this creates a new Git intent governed by this
              environment's publication policy.
            </label>
          </div>
          <div className="button-row">
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
              disabled={!confirmed || idempotencyKey === ""}
              onClick={() =>
                rollback.mutate({
                  deploymentId: deployment.id,
                  candidate: selected,
                  idempotencyKey,
                })
              }
            >
              Confirm rollback
            </Button>
          </div>
        </div>
      ) : null}
      {rollback.error ? <ErrorPanel error={rollback.error} /> : null}
      {rollback.data ? (
        <div className="notice notice--success" role="status">
          <strong>Rollback Git intent accepted</strong>
          <p>
            Operation <code>{rollback.data.id}</code> is now following the
            environment publication policy.
          </p>
        </div>
      ) : null}
    </div>
  );
}
