import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type { CreatePlatformArgoGitBinding } from "../api/types";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
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
  const bindingMissing = isStatus(binding.error, 404);
  const canSelectAuthority = humanPlatformAdmin && bindingMissing;
  const installations = useQuery({
    queryKey: ["github-installations"],
    queryFn: api.githubInstallations,
    enabled: canSelectAuthority,
    retry: false,
  });
  const [installationId, setInstallationId] = useState("");
  const [repositoryId, setRepositoryId] = useState("");
  const [targetRef, setTargetRef] = useState("platform");
  const [confirmed, setConfirmed] = useState(false);
  const [validationError, setValidationError] = useState("");
  const createAttempt = useRef<{ signature: string; key: string } | null>(null);

  useEffect(() => {
    if (
      installationId === "" ||
      !installations.data?.items.some((item) => item.id === installationId)
    ) {
      setInstallationId(installations.data?.items[0]?.id ?? "");
    }
  }, [installationId, installations.data]);

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

  useEffect(() => {
    if (
      repositoryId === "" ||
      !activeRepositories.some((item) => item.id === repositoryId)
    ) {
      setRepositoryId(activeRepositories[0]?.id ?? "");
    }
  }, [activeRepositories, repositoryId]);

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
      setValidationError("Use a branch name such as platform with no spaces.");
      return;
    }
    if (!confirmed) {
      setValidationError(
        "Confirm the immutable Git authority before creating it.",
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
    <div className="page">
      <PageHeader
        eyebrow="Platform settings"
        title="Argo Git authority"
        description="Bind protected platform desired state to one verified GitHub App repository. Kuberploy derives the cluster, canonical remote, protected path, and GitHub App identity."
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
          <div className="card__header">
            <div>
              <span className="eyebrow">Immutable authority</span>
              <h2>
                {binding.data.repository.owner}/{binding.data.repository.name}
              </h2>
            </div>
            <StatusPill value={binding.data.state} />
          </div>
          <dl className="detail-list">
            <div>
              <dt>Cluster</dt>
              <dd>
                <code>{binding.data.clusterId}</code>
              </dd>
            </div>
            <div>
              <dt>Protected path</dt>
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
          </dl>
          <div className="notice" role="status">
            <div>
              <strong>Authority recorded; Argo remains fail-closed</strong>
              <p>
                Capability stays disabled until repository credentials, the root
                Application, protected desired-state materialization, and exact
                Kubernetes observations are ready.
              </p>
            </div>
          </div>
        </Card>
      ) : isStatus(binding.error, 503) ? (
        <Card>
          <EmptyState
            icon="route"
            title="Platform Git binding is not configured"
            description="The operator must configure the server-owned cluster and GitHub App identity before this workflow is available. Argo remains disabled."
          />
        </Card>
      ) : binding.error && !bindingMissing ? (
        <ErrorPanel
          error={binding.error}
          onRetry={() => void binding.refetch()}
        />
      ) : (
        <Card className="form-card">
          <div className="card__header">
            <div>
              <span className="eyebrow">Create once</span>
              <h2>Select verified repository authority</h2>
              <p>
                Only opaque catalog IDs and a branch ref leave this form. Clone
                URLs, credential references, Secret names, and custom paths are
                not accepted.
              </p>
            </div>
          </div>

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

          <form className="form-grid" onSubmit={submit}>
            <Field label="Verified GitHub App installation" required>
              <select
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
              </select>
            </Field>
            <Field label="Verified repository" required>
              <select
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
              </select>
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
                placeholder="platform"
                autoComplete="off"
                spellCheck={false}
                maxLength={210}
                disabled={create.isPending}
              />
            </Field>
            <label className="confirmation-check">
              <input
                type="checkbox"
                checked={confirmed}
                onChange={(event) => setConfirmed(event.target.checked)}
                disabled={!repositoryId || create.isPending}
              />
              <span>
                I understand this repository and branch become the immutable
                protected platform Git authority.
              </span>
            </label>
            {validationError ? (
              <div className="notice notice--error" role="alert">
                <div>
                  <strong>Review the authority selection</strong>
                  <p>{validationError}</p>
                </div>
              </div>
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
              <Icon name="route" /> Create immutable authority
            </Button>
          </form>
        </Card>
      )}
    </div>
  );
}
