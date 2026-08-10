import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, errorMessage } from "../api/client";
import type {
  Application,
  AutoDeployPolicy,
  BuildDefinition,
  Capability,
  Project,
} from "../api/types";
import { formatDate, shortId } from "../lib/format";
import {
  canMutateAutoDeployPolicy,
  hasPotentialAutoDeployManagement,
} from "../lib/autoDeployAccess";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Skeleton,
  StatusPill,
} from "./ui";

function PolicyHistory({ policy }: { policy: AutoDeployPolicy }) {
  const revisions = useQuery({
    queryKey: ["auto-deploy-policy-revisions", policy.id],
    queryFn: () => api.autoDeployPolicyRevisions(policy.id),
  });
  const runs = useQuery({
    queryKey: ["auto-deploy-policy-runs", policy.id],
    queryFn: () => api.autoDeployPolicyRuns(policy.id),
    refetchInterval: (query) =>
      query.state.data?.items.some(
        (run) => run.state === "pending" || run.state === "processing",
      )
        ? 5_000
        : false,
  });
  if (revisions.isPending || runs.isPending) return <Skeleton lines={3} />;
  return (
    <div className="stack stack--compact">
      <details>
        <summary>
          {revisions.data?.items.length ?? 0} immutable revisions
        </summary>
        <ul>
          {(revisions.data?.items ?? []).map((revision) => (
            <li key={revision.revision}>
              Revision {revision.revision} ·{" "}
              {revision.enabled ? "enabled" : "disabled"} · config{" "}
              {shortId(revision.sourceConfigETag, 16)} ·{" "}
              {formatDate(revision.createdAt)}
            </li>
          ))}
        </ul>
      </details>
      <details>
        <summary>{runs.data?.items.length ?? 0} durable run receipts</summary>
        {(runs.data?.items ?? []).length ? (
          <ul>
            {(runs.data?.items ?? []).map((run) => (
              <li key={`${run.attemptId}-${run.policyRevision}`}>
                <StatusPill value={run.state} /> attempt{" "}
                {shortId(run.attemptId, 12)} · revision {run.policyRevision}
                {run.operationId
                  ? ` · operation ${shortId(run.operationId, 12)}`
                  : ""}
                {run.failureCode ? ` · ${run.failureCode}` : ""}
              </li>
            ))}
          </ul>
        ) : (
          <p>No successful build release has triggered this policy yet.</p>
        )}
      </details>
    </div>
  );
}

export function AutoDeployPoliciesPanel({
  application,
  project,
  definitions,
  enabled,
  humanSession,
  capabilities,
}: {
  application: Application;
  project: Project;
  definitions: BuildDefinition[];
  enabled: boolean;
  humanSession: boolean;
  capabilities: Capability[];
}) {
  const applicationId = application.id;
  const projectId = project.id;
  const potentialManagement = hasPotentialAutoDeployManagement(
    humanSession,
    capabilities,
    application,
    project,
  );
  const queryClient = useQueryClient();
  const policies = useQuery({
    queryKey: ["auto-deploy-policies", applicationId],
    queryFn: () => api.autoDeployPolicies(applicationId),
    enabled,
    retry: false,
  });
  const deployments = useQuery({
    queryKey: ["deployments"],
    queryFn: api.deployments,
    enabled: enabled && potentialManagement,
    retry: false,
  });
  const accounts = useQuery({
    queryKey: ["service-accounts", projectId],
    queryFn: () => api.serviceAccounts(projectId),
    enabled: enabled && potentialManagement,
    retry: false,
  });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    enabled: enabled && potentialManagement,
    retry: false,
  });
  const candidates = useMemo(
    () =>
      (deployments.data?.items ?? []).filter(
        (item) => item.applicationId === applicationId,
      ),
    [applicationId, deployments.data],
  );
  const serviceAccounts = (accounts.data?.items ?? []).filter((account) =>
    (environments.data?.items ?? []).some(
      (environment) =>
        candidates.some(
          (candidate) => candidate.environmentId === environment.id,
        ) &&
        canMutateAutoDeployPolicy(
          humanSession,
          capabilities,
          application,
          environment,
          project,
          account,
        ),
    ),
  );
  const [definitionId, setDefinitionId] = useState("");
  const [deploymentId, setDeploymentId] = useState("");
  const [serviceActorId, setServiceActorId] = useState("");
  const selectedAccount = serviceAccounts.find(
    (item) => item.id === serviceActorId,
  );
  const authorizedCandidates = candidates.filter((candidate) => {
    const environment = environments.data?.items.find(
      (item) => item.id === candidate.environmentId,
    );
    return Boolean(
      environment &&
      selectedAccount &&
      canMutateAutoDeployPolicy(
        humanSession,
        capabilities,
        application,
        environment,
        project,
        selectedAccount,
      ),
    );
  });
  useEffect(() => {
    if (!definitions.some((item) => item.id === definitionId))
      setDefinitionId(definitions[0]?.id ?? "");
  }, [definitionId, definitions]);
  useEffect(() => {
    if (!authorizedCandidates.some((item) => item.id === deploymentId))
      setDeploymentId(authorizedCandidates[0]?.id ?? "");
  }, [authorizedCandidates, deploymentId]);
  useEffect(() => {
    if (!serviceAccounts.some((item) => item.id === serviceActorId))
      setServiceActorId(serviceAccounts[0]?.id ?? "");
  }, [serviceAccounts, serviceActorId]);
  const create = useMutation({
    mutationFn: () => {
      const deployment = authorizedCandidates.find(
        (item) => item.id === deploymentId,
      );
      if (!deployment)
        throw new Error("Select an existing deployment configuration.");
      return api.createAutoDeployPolicy(applicationId, {
        buildDefinitionId: definitionId,
        environmentId: deployment.environmentId,
        templateDeploymentId: deployment.id,
        serviceActorId,
        enabled: true,
      });
    },
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: ["auto-deploy-policies", applicationId],
      }),
  });
  const revise = useMutation({
    mutationFn: ({
      policy,
      enabled: nextEnabled,
    }: {
      policy: AutoDeployPolicy;
      enabled: boolean;
    }) =>
      api.reviseAutoDeployPolicy(policy.id, {
        templateDeploymentId: policy.current.sourceDeploymentId,
        serviceActorId: policy.current.serviceActorId,
        enabled: nextEnabled,
        expectedRevision: policy.currentRevision,
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: ["auto-deploy-policies", applicationId],
      }),
  });

  if (!enabled) return null;
  const loadError = policies.error;
  const mutationCatalogError =
    potentialManagement &&
    (deployments.error ?? accounts.error ?? environments.error);
  return (
    <Card className="source-build-card">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">Automatic deployment</span>
          <h2>Verified build → pinned AppConfig</h2>
          <p>
            Choose an exact deployment/config snapshot and project service
            account identity. No token is created or stored. AppConfig or parent
            VariableSet drift pauses automation until you save a new revision.
          </p>
        </div>
        <StatusPill value="ready" label="Controller ready" />
      </div>
      {loadError ? (
        <ErrorPanel error={loadError} onRetry={() => void policies.refetch()} />
      ) : null}
      {potentialManagement && mutationCatalogError ? (
        <ErrorPanel
          error={mutationCatalogError}
          onRetry={() =>
            void Promise.all([
              deployments.refetch(),
              accounts.refetch(),
              environments.refetch(),
            ])
          }
        />
      ) : null}
      {potentialManagement &&
      !mutationCatalogError &&
      serviceAccounts.length > 0 &&
      authorizedCandidates.length > 0 &&
      definitions.length > 0 ? (
        <div className="form-grid">
          <label className="field">
            <span className="field__label">Build definition</span>
            <select
              value={definitionId}
              onChange={(event) => setDefinitionId(event.target.value)}
            >
              {definitions.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.triggerRef} · {shortId(item.definitionDigest, 12)}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">Pinned deployment config</span>
            <select
              value={deploymentId}
              onChange={(event) => setDeploymentId(event.target.value)}
            >
              {authorizedCandidates.map((item) => (
                <option key={item.id} value={item.id}>
                  {shortId(item.id, 12)} · {item.environmentId}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">
              Service account (identity only)
            </span>
            <select
              value={serviceActorId}
              onChange={(event) => setServiceActorId(event.target.value)}
            >
              {serviceAccounts.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.name} · {item.role}
                </option>
              ))}
            </select>
          </label>
          <Button
            onClick={() => create.mutate()}
            busy={create.isPending}
            disabled={!definitionId || !deploymentId || !serviceActorId}
          >
            Enable pinned policy
          </Button>
          {create.error ? (
            <p className="field__error">{errorMessage(create.error)}</p>
          ) : null}
        </div>
      ) : null}
      {policies.isPending ? (
        <Skeleton lines={4} />
      ) : policies.data?.items.length ? (
        <div className="stack">
          {policies.data.items.map((policy) => (
            <article className="build-definition-row" key={policy.id}>
              <div>
                <StatusPill
                  value={policy.current.enabled ? "enabled" : "disabled"}
                />
                <strong>{shortId(policy.buildDefinitionId, 12)}</strong>
                <small>
                  Revision {policy.currentRevision} · deployment{" "}
                  {shortId(policy.current.sourceDeploymentId, 12)}
                </small>
              </div>
              {(() => {
                const environment = environments.data?.items.find(
                  (item) => item.id === policy.environmentId,
                );
                const account = accounts.data?.items.find(
                  (item) => item.id === policy.current.serviceActorId,
                );
                const allowed = Boolean(
                  environment &&
                  account &&
                  canMutateAutoDeployPolicy(
                    humanSession,
                    capabilities,
                    application,
                    environment,
                    project,
                    account,
                  ),
                );
                return allowed ? (
                  <div className="button-group">
                    <Button
                      variant="secondary"
                      busy={revise.isPending}
                      onClick={() =>
                        revise.mutate({
                          policy,
                          enabled: !policy.current.enabled,
                        })
                      }
                    >
                      {policy.current.enabled ? "Disable" : "Enable"}
                    </Button>
                    <Button
                      variant="secondary"
                      busy={revise.isPending}
                      onClick={() =>
                        revise.mutate({
                          policy,
                          enabled: policy.current.enabled,
                        })
                      }
                    >
                      Repin current config
                    </Button>
                  </div>
                ) : null;
              })()}
              <PolicyHistory policy={policy} />
            </article>
          ))}
        </div>
      ) : (
        <EmptyState
          title="No auto-deploy policies"
          description="Pin a verified build definition to an existing deployment configuration."
        />
      )}
      {revise.error ? (
        <p className="field__error">{errorMessage(revise.error)}</p>
      ) : null}
    </Card>
  );
}
