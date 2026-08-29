import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type { CreatePlatformArgoGitBinding } from "../api/types";
import { Icon } from "../components/Icon";
import {
  Select,
  Button,
  Card,
  CardHeader,
  DetailList,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  FormCard,
  FormGrid,
  Notice,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
import {
  canonicalBranchRef,
  formatDate,
  gitRefLabel,
  shortId,
} from "../lib/format";

const branchNamePattern =
  /^(?!refs\/)[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9])?$/;

function isStatus(error: unknown, status: number) {
  return error instanceof ApiError && error.status === status;
}

export function PlatformArgoGitBindingPage() {
  const queryClient = useQueryClient();
  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    staleTime: 60_000,
  });
  const humanPlatformAdmin =
    me.data?.role === "platform-admin" &&
    me.data.authentication?.kind === "session";
  const binding = useQuery({
    queryKey: ["platform-argo-git-binding"],
    queryFn: api.platformArgoGitBinding,
    enabled: humanPlatformAdmin,
    retry: false,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    enabled: humanPlatformAdmin && Boolean(binding.data),
    retry: false,
    staleTime: 10_000,
  });
  const bindingMissing = isStatus(binding.error, 404);
  const canSelectAuthority = humanPlatformAdmin && bindingMissing;
  const argoReady =
    capabilities.data?.features?.argoCD === true ||
    capabilities.data?.features?.argo === true;
  const installations = useQuery({
    queryKey: ["github-installations"],
    queryFn: api.githubInstallations,
    enabled: canSelectAuthority,
    retry: false,
  });
  // Picked ids are preferences; the effective selection is derived from the
  // list that is loaded right now, in the same render.
  const [installationChoice, setInstallationId] = useState("");
  const [repositoryChoice, setRepositoryId] = useState("");
  const [targetRef, setTargetRef] = useState("main");
  const [confirmed, setConfirmed] = useState(false);
  const [validationError, setValidationError] = useState("");
  const createAttempt = useRef<{ signature: string; key: string } | null>(null);

  const installationId = installations.data?.items.some(
    (item) => item.id === installationChoice,
  )
    ? installationChoice
    : (installations.data?.items[0]?.id ?? "");

  const repositories = useQuery({
    queryKey: ["github-installation-repositories", installationId],
    queryFn: () => api.githubInstallationRepositories(installationId),
    enabled: canSelectAuthority && installationId !== "",
    retry: false,
  });
  const activeRepositories = useMemo(
    () =>
      (repositories.data?.items ?? []).filter(
        (repository) => repository.lifecycle === "active",
      ),
    [repositories.data],
  );

  const repositoryId = activeRepositories.some(
    (item) => item.id === repositoryChoice,
  )
    ? repositoryChoice
    : (activeRepositories[0]?.id ?? "");

  const create = useMutation({
    mutationFn: ({
      input,
      idempotencyKey,
    }: {
      input: CreatePlatformArgoGitBinding;
      idempotencyKey: string;
    }) => api.createPlatformArgoGitBinding(input, idempotencyKey),
    onSuccess: async (created, input) => {
      if (createAttempt.current?.key === input.idempotencyKey) {
        createAttempt.current = null;
      }
      setConfirmed(false);
      queryClient.setQueryData(["platform-argo-git-binding"], created);
      await queryClient.invalidateQueries({
        queryKey: ["platform-argo-git-binding"],
      });
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const branch = gitRefLabel(targetRef.trim());
    const exactRef = canonicalBranchRef(branch);
    if (!installationId || !repositoryId) {
      setValidationError("Select a verified installation and repository.");
      return;
    }
    if (!branchNamePattern.test(branch)) {
      setValidationError("Use a branch name such as main with no spaces.");
      return;
    }
    if (!confirmed) {
      setValidationError(
        "Confirm the protected Git authority before creating it.",
      );
      return;
    }
    setValidationError("");
    const input = { installationId, repositoryId, targetRef: exactRef };
    const signature = JSON.stringify(input);
    const idempotencyKey =
      createAttempt.current?.signature === signature
        ? createAttempt.current.key
        : crypto.randomUUID();
    createAttempt.current = { signature, key: idempotencyKey };
    create.mutate({ input, idempotencyKey });
  };

  return (
    <Page>
      <PageHeader
        eyebrow="Platform settings"
        title="Argo Git authority"
        description="Bind this installation's protected desired state to one verified GitHub App repository. Kuberploy manages only its current Kubernetes cluster."
        actions={
          humanPlatformAdmin ? (
            <Button
              variant="secondary"
              busy={binding.isFetching}
              onClick={() => void binding.refetch()}
            >
              <Icon name="refresh" /> Refresh
            </Button>
          ) : undefined
        }
      />

      {me.isPending ? (
        <Card>
          <Skeleton lines={6} />
        </Card>
      ) : !humanPlatformAdmin ? (
        <Card>
          <EmptyState
            icon="settings"
            title="Interactive platform administrator required"
            description="This high-risk authority workflow is unavailable to service accounts and non-platform roles."
          />
        </Card>
      ) : binding.isPending ? (
        <Card>
          <Skeleton lines={8} />
        </Card>
      ) : binding.data ? (
        <Card>
          <CardHeader bar>
            <div>
              <Eyebrow>Git authority</Eyebrow>
              <h2>
                {binding.data.repository.owner}/{binding.data.repository.name}
              </h2>
            </div>
            <StatusPill value={binding.data.state} />
          </CardHeader>
          <DetailList>
            <div>
              <dt>Installation-scoped path</dt>
              <dd>
                <code>{binding.data.pathPrefix}</code>
              </dd>
            </div>
            <div>
              <dt>Target branch</dt>
              <dd>
                <code>{gitRefLabel(binding.data.targetRef)}</code>
              </dd>
            </div>
            <div>
              <dt>Provider repository identity</dt>
              <dd>
                GitHub installation {binding.data.repository.installationId} ·
                repository {binding.data.repository.repositoryId}
              </dd>
            </div>
            <div>
              <dt>Observed head</dt>
              <dd>
                {binding.data.targetHeadRevision
                  ? shortId(binding.data.targetHeadRevision, 12)
                  : "Not observed"}
              </dd>
            </div>
            <div>
              <dt>Updated</dt>
              <dd>{formatDate(binding.data.updatedAt)}</dd>
            </div>
          </DetailList>
          <Notice role="status">
            <div>
              <strong>
                {argoReady
                  ? "Authority recorded; Argo is ready"
                  : "Authority recorded; Argo remains fail-closed"}
              </strong>
              <p>
                {argoReady
                  ? "Protected desired state is materialized and the exact Kubernetes observations are healthy."
                  : "Capability stays disabled until repository credentials, the root Application, protected desired-state materialization, and exact Kubernetes observations are ready."}
              </p>
            </div>
          </Notice>
        </Card>
      ) : isStatus(binding.error, 503) ? (
        <Card>
          <EmptyState
            icon="route"
            title="Platform Git binding is not configured"
            description="The operator must configure this installation's GitHub App identity before this workflow is available. Argo remains disabled."
          />
        </Card>
      ) : binding.error && !bindingMissing ? (
        <ErrorPanel
          error={binding.error}
          onRetry={() => void binding.refetch()}
        />
      ) : (
        <FormCard>
          <CardHeader bar>
            <div>
              <Eyebrow>Create once</Eyebrow>
              <h2>Select verified repository authority</h2>
              <p>
                Only opaque catalog IDs and a branch ref leave this form. Clone
                URLs, credential references, Secret names, and custom paths are
                not accepted.
              </p>
            </div>
          </CardHeader>

          {installations.error || repositories.error ? (
            <ErrorPanel
              error={installations.error ?? repositories.error}
              onRetry={() =>
                void Promise.all([
                  installations.refetch(),
                  installationId ? repositories.refetch() : Promise.resolve(),
                ])
              }
              title="Could not load the verified GitHub catalog"
            />
          ) : null}

          <FormGrid as="form" onSubmit={submit}>
            <Field label="Verified GitHub App installation" required>
              <Select
                value={installationId}
                onChange={(event) => {
                  setInstallationId(event.target.value);
                  setRepositoryId("");
                  setConfirmed(false);
                }}
                disabled={installations.isPending || create.isPending}
              >
                {installations.data?.items.length ? null : (
                  <option value="">No verified installation available</option>
                )}
                {installations.data?.items.map((installation) => (
                  <option key={installation.id} value={installation.id}>
                    {installation.accountLogin} (installation{" "}
                    {installation.githubInstallationId})
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Verified repository" required>
              <Select
                value={repositoryId}
                onChange={(event) => {
                  setRepositoryId(event.target.value);
                  setConfirmed(false);
                }}
                disabled={
                  !installationId || repositories.isPending || create.isPending
                }
              >
                {activeRepositories.length ? null : (
                  <option value="">No active repository available</option>
                )}
                {activeRepositories.map((repository) => (
                  <option key={repository.id} value={repository.id}>
                    {repository.ownerLogin}/{repository.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field
              label="Target branch"
              required
              hint="Enter the branch name. The protected path is derived by Kuberploy."
            >
              <input
                value={targetRef}
                onChange={(event) => {
                  setTargetRef(event.target.value);
                  setConfirmed(false);
                }}
                placeholder="main"
                autoComplete="off"
                spellCheck={false}
                maxLength={210}
                disabled={create.isPending}
              />
            </Field>
            <label className="grid grid-cols-[17px_1fr] items-start gap-2 text-ink-soft cursor-pointer text-meta leading-[1.5] [&_input]:w-[15px] [&_input]:min-h-[15px] [&_input]:m-0 [&_input]:accent-mint-dark">
              <input
                type="checkbox"
                checked={confirmed}
                onChange={(event) => setConfirmed(event.target.checked)}
                disabled={!repositoryId || create.isPending}
              />
              <span>
                I understand this repository and branch become the protected
                platform Git authority.
              </span>
            </label>
            {validationError ? (
              <Notice tone="error" role="alert">
                <div>
                  <strong>Review the authority selection</strong>
                  <p>{validationError}</p>
                </div>
              </Notice>
            ) : null}
            {create.error ? <ErrorPanel error={create.error} /> : null}
            <Button
              type="submit"
              busy={create.isPending}
              disabled={
                !confirmed ||
                !installationId ||
                !repositoryId ||
                installations.isPending ||
                repositories.isPending
              }
            >
              <Icon name="route" /> Create Git authority
            </Button>
          </FormGrid>
        </FormCard>
      )}
    </Page>
  );
}
