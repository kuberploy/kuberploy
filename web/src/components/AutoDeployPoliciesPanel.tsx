import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, errorMessage } from "../api/client";
import type {
  Application,
  AutoDeployPolicy,
  BuildDefinition,
  Capability,
  Project,
} from "../api/types";
import { formatDate, gitRefLabel, shortId } from "../lib/format";
import {
  canMutateAutoDeployPolicy,
  hasPotentialAutoDeployManagement,
} from "../lib/autoDeployAccess";
import {
  Button,
  Card,
  CardHeader,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  FieldLabel,
  FormGrid,
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
    <div className="grid gap-4 gap-2">
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
  const applicationScopeRef = useRef(applicationId);
  applicationScopeRef.current = applicationId;
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
  // A selection is a preference, not a fact: it is only the id the operator
  // picked. What is actually selected is derived here, so a list that loses the
  // picked entry falls back within the same render instead of painting an
  // invalid selection and correcting it in an effect on the next one.
  const [definitionChoice, setDefinitionId] = useState("");
  const [deploymentChoice, setDeploymentId] = useState("");
  const [serviceActorChoice, setServiceActorId] = useState("");
  const definitionId = definitions.some((item) => item.id === definitionChoice)
    ? definitionChoice
    : (definitions[0]?.id ?? "");
  const serviceActorId = serviceAccounts.some(
    (item) => item.id === serviceActorChoice,
  )
    ? serviceActorChoice
    : (serviceAccounts[0]?.id ?? "");
  const [disableConfirmation, setDisableConfirmation] =
    useState<AutoDeployPolicy | null>(null);
  const createAttempt = useRef<{ signature: string; key: string } | null>(null);
  const reviseAttempt = useRef<{ signature: string; key: string } | null>(null);
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
  const deploymentId = authorizedCandidates.some(
    (item) => item.id === deploymentChoice,
  )
    ? deploymentChoice
    : (authorizedCandidates[0]?.id ?? "");
  const create = useMutation({
    mutationFn: (input: {
      applicationId: string;
      buildDefinitionId: string;
      environmentId: string;
      templateDeploymentId: string;
      serviceActorId: string;
      idempotencyKey: string;
    }) => {
      if (
        !input.applicationId ||
        !input.buildDefinitionId ||
        !input.environmentId ||
        !input.templateDeploymentId ||
        !input.serviceActorId
      )
        throw new Error("Select an existing App configuration.");
      return api.createAutoDeployPolicy(
        input.applicationId,
        {
          buildDefinitionId: input.buildDefinitionId,
          environmentId: input.environmentId,
          templateDeploymentId: input.templateDeploymentId,
          serviceActorId: input.serviceActorId,
          enabled: true,
        },
        input.idempotencyKey,
      );
    },
    onSuccess: (_value, input) => {
      if (input.applicationId !== applicationScopeRef.current) return;
      if (createAttempt.current?.key === input.idempotencyKey) {
        createAttempt.current = null;
      }
      return queryClient.invalidateQueries({
        queryKey: ["auto-deploy-policies", input.applicationId],
      });
    },
  });
  const revise = useMutation({
    mutationFn: ({
      policy,
      enabled: nextEnabled,
      idempotencyKey,
    }: {
      policy: AutoDeployPolicy;
      enabled: boolean;
      idempotencyKey: string;
      applicationId: string;
    }) =>
      api.reviseAutoDeployPolicy(
        policy.id,
        {
          templateDeploymentId: policy.current.sourceDeploymentId,
          serviceActorId: policy.current.serviceActorId,
          enabled: nextEnabled,
          expectedRevision: policy.currentRevision,
        },
        idempotencyKey,
      ),
    onSuccess: (_value, input) => {
      if (input.applicationId !== applicationScopeRef.current) return;
      if (reviseAttempt.current?.key === input.idempotencyKey) {
        reviseAttempt.current = null;
      }
      return queryClient.invalidateQueries({
        queryKey: ["auto-deploy-policies", input.applicationId],
      });
    },
  });

  const enablePolicy = () => {
    const deployment = authorizedCandidates.find(
      (item) => item.id === deploymentId,
    );
    const signature = JSON.stringify({
      applicationId,
      definitionId,
      deploymentId,
      serviceActorId,
    });
    const key =
      createAttempt.current?.signature === signature
        ? createAttempt.current.key
        : crypto.randomUUID();
    createAttempt.current = { signature, key };
    create.mutate({
      applicationId,
      buildDefinitionId: definitionId,
      environmentId: deployment?.environmentId ?? "",
      templateDeploymentId: deployment?.id ?? "",
      serviceActorId,
      idempotencyKey: key,
    });
  };

  const revisePolicy = (
    policy: AutoDeployPolicy,
    nextEnabled: boolean,
    confirmed = false,
  ) => {
    if (!nextEnabled && !confirmed) {
      setDisableConfirmation(policy);
      return;
    }
    const signature = JSON.stringify({
      policyId: policy.id,
      revision: policy.currentRevision,
      sourceDeploymentId: policy.current.sourceDeploymentId,
      serviceActorId: policy.current.serviceActorId,
      enabled: nextEnabled,
    });
    const key =
      reviseAttempt.current?.signature === signature
        ? reviseAttempt.current.key
        : crypto.randomUUID();
    reviseAttempt.current = { signature, key };
    revise.mutate({
      policy,
      enabled: nextEnabled,
      applicationId,
      idempotencyKey: key,
    });
  };

  if (!enabled) return null;
  const loadError = policies.error;
  const mutationCatalogError =
    potentialManagement &&
    (deployments.error ?? accounts.error ?? environments.error);
  return (
    <Card className="mb-5">
      <CardHeader>
        <div>
          <Eyebrow>Automatic App delivery</Eyebrow>
          <h2>Verified build → pinned AppConfig</h2>
          <p>
            Choose an exact App configuration snapshot and Project service
            account identity. No token is created or stored. AppConfig or parent
            VariableSet drift pauses automation until you save a new revision.
          </p>
        </div>
        <StatusPill value="ready" label="Controller ready" />
      </CardHeader>
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
        <FormGrid>
          <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
            <FieldLabel>Build definition</FieldLabel>
            <select
              value={definitionId}
              onChange={(event) => setDefinitionId(event.target.value)}
            >
              {definitions.map((item) => (
                <option key={item.id} value={item.id}>
                  {gitRefLabel(item.triggerRef)} ·{" "}
                  {shortId(item.definitionDigest, 12)}
                </option>
              ))}
            </select>
          </label>
          <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
            <FieldLabel>Pinned App configuration</FieldLabel>
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
          <label className="flex min-w-0 flex-col gap-1.5 gap-2 [&_input]:w-full [&_input]:py-0 [&_input]:px-3 [&_input]:border [&_input]:border-line-strong [&_input]:outline-none [&_input]:text-ink [&_input]:bg-surface [&_input]:transition-[border-color,box-shadow] [&_input]:duration-(--motion-fast) [&_input]:ease-(--ease-standard) [&_input]:min-h-11 [&_input]:rounded-[9px] [&_input]:text-sm [&_select]:w-full [&_select]:py-0 [&_select]:px-3 [&_select]:border [&_select]:border-line-strong [&_select]:outline-none [&_select]:text-ink [&_select]:bg-surface [&_select]:transition-[border-color,box-shadow] [&_select]:duration-(--motion-fast) [&_select]:ease-(--ease-standard) [&_select]:min-h-11 [&_select]:rounded-[9px] [&_select]:text-sm [&_textarea]:w-full [&_textarea]:py-0 [&_textarea]:px-3 [&_textarea]:border [&_textarea]:border-line-strong [&_textarea]:outline-none [&_textarea]:text-ink [&_textarea]:bg-surface [&_textarea]:transition-[border-color,box-shadow] [&_textarea]:duration-(--motion-fast) [&_textarea]:ease-(--ease-standard) [&_textarea]:min-h-11 [&_textarea]:rounded-[9px] [&_textarea]:text-sm">
            <FieldLabel>Service account (identity only)</FieldLabel>
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
            onClick={enablePolicy}
            busy={create.isPending}
            disabled={!definitionId || !deploymentId || !serviceActorId}
          >
            Enable pinned policy
          </Button>
          {create.error ? (
            <p className="text-tone-bad text-xs leading-[1.45]">
              {errorMessage(create.error)}
            </p>
          ) : null}
        </FormGrid>
      ) : null}
      {policies.isPending ? (
        <Skeleton lines={4} />
      ) : policies.data?.items.length ? (
        <div className="grid gap-4">
          {policies.data.items.map((policy) => (
            <article
              className="last:border-b-0 [&_small]:text-ink-faint [&_small]:text-xs grid grid-cols-[minmax(0,_1fr)_auto] gap-3 p-4 border-b border-b-line [&>div:first-child]:flex [&>div:first-child]:min-w-0 [&>div:first-child]:items-center [&>div:first-child]:justify-between [&>div:first-child]:gap-3 [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_code]:text-ink-faint [&_code]:text-xs [&>small]:self-center [&>small]:text-right"
              key={policy.id}
            >
              <div>
                <StatusPill
                  value={policy.current.enabled ? "enabled" : "disabled"}
                />
                <strong>{shortId(policy.buildDefinitionId, 12)}</strong>
                <small>
                  Revision {policy.currentRevision} · runtime{" "}
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
                  <div className="flex items-center flex-wrap gap-2">
                    <Button
                      variant="secondary"
                      busy={revise.isPending}
                      onClick={() =>
                        revisePolicy(policy, !policy.current.enabled)
                      }
                    >
                      {policy.current.enabled ? "Disable" : "Enable"}
                    </Button>
                    <Button
                      variant="secondary"
                      busy={revise.isPending}
                      onClick={() =>
                        revisePolicy(policy, policy.current.enabled)
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
          description="Pin a verified build definition to an existing App configuration."
        />
      )}
      {revise.error ? (
        <p className="text-tone-bad text-xs leading-[1.45]">
          {errorMessage(revise.error)}
        </p>
      ) : null}
      {disableConfirmation ? (
        <ConfirmDialog
          title={`Disable auto-deploy policy ${disableConfirmation.id}?`}
          description="Verified builds will stop creating App rollout operations until this policy is enabled again."
          confirmLabel="Disable policy"
          icon="close"
          busy={revise.isPending}
          onCancel={() => setDisableConfirmation(null)}
          onConfirm={() => {
            const policy = disableConfirmation;
            setDisableConfirmation(null);
            revisePolicy(policy, false, true);
          }}
        />
      ) : null}
    </Card>
  );
}
