import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { ApiError, api, errorMessage } from "../api/client";
import type { CreateEnvironmentGitBinding, Environment } from "../api/types";
import {
  canonicalBranchRef,
  formatDate,
  gitRefLabel,
  shortId,
} from "../lib/format";
import { Icon } from "./Icon";
import {
  Select,
  Button,
  CardHeader,
  DetailList,
  ErrorPanel,
  Eyebrow,
  Field,
  MutedCopy,
  Skeleton,
  StatusPill,
} from "./ui";

const branchNamePattern =
  /^(?!refs\/)[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,198}[A-Za-z0-9])?$/;

function isStatus(error: unknown, status: number) {
  return error instanceof ApiError && error.status === status;
}

type StableAttempt = { signature: string; key: string };

export function EnvironmentGitBindingPanel({
  environment,
  humanSession,
  canManage,
  onClose,
}: {
  environment: Environment;
  humanSession: boolean;
  canManage: boolean;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const attempt = useRef<StableAttempt | null>(null);
  const binding = useQuery({
    queryKey: ["environment-git-binding", environment.id],
    queryFn: () => api.environmentGitBinding(environment.id),
    retry: false,
  });
  const missing = isStatus(binding.error, 404);
  const canCreate = humanSession && canManage && missing;
  const installations = useQuery({
    queryKey: ["github-installations"],
    queryFn: api.githubInstallations,
    enabled: canCreate,
    retry: false,
  });
  // The picked ids are preferences. What is actually selected is derived below
  // from the list that is loaded right now, so a list that drops the picked
  // entry falls back inside the same render rather than one render later.
  const [installationChoice, setInstallationId] = useState("");
  const [repositoryChoice, setRepositoryId] = useState("");
  const [targetRef, setTargetRef] = useState("main");
  const [confirmed, setConfirmed] = useState(false);
  const [validationError, setValidationError] = useState("");
  const environmentRef = useRef(environment.id);
  environmentRef.current = environment.id;

  const installationId = installations.data?.items.some(
    (item) => item.id === installationChoice,
  )
    ? installationChoice
    : (installations.data?.items[0]?.id ?? "");

  const repositories = useQuery({
    queryKey: ["github-installation-repositories", installationId],
    queryFn: () => api.githubInstallationRepositories(installationId),
    enabled: canCreate && installationId !== "",
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
    mutationFn: (input: {
      environmentId: string;
      value: CreateEnvironmentGitBinding;
      idempotencyKey: string;
    }) =>
      api.createEnvironmentGitBinding(
        input.environmentId,
        input.value,
        input.idempotencyKey,
      ),
    onSuccess: async (created, input) => {
      if (environmentRef.current !== input.environmentId) return;
      attempt.current = null;
      setConfirmed(false);
      queryClient.setQueryData(
        ["environment-git-binding", input.environmentId],
        created,
      );
      await queryClient.invalidateQueries({
        queryKey: ["environment-git-binding", input.environmentId],
      });
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const branch = gitRefLabel(targetRef.trim());
    const value = {
      installationId,
      repositoryId,
      targetRef: canonicalBranchRef(branch),
    };
    if (!value.installationId || !value.repositoryId) {
      setValidationError("Select a verified installation and repository.");
      return;
    }
    if (!branchNamePattern.test(branch)) {
      setValidationError("Use a branch name such as main with no spaces.");
      return;
    }
    if (!confirmed) {
      setValidationError(
        "Confirm the immutable environment Git authority before creating it.",
      );
      return;
    }
    const signature = JSON.stringify(value);
    const idempotencyKey =
      attempt.current?.signature === signature
        ? attempt.current.key
        : crypto.randomUUID();
    attempt.current = { signature, key: idempotencyKey };
    setValidationError("");
    create.mutate({
      environmentId: environment.id,
      value,
      idempotencyKey,
    });
  };

  return (
    <div
      className="p-6 border-t border-t-line bg-surface-soft"
      aria-label={`${environment.name} Git authority`}
    >
      <div className="flex items-start justify-between gap-5 mb-5 [&_h3]:my-1 [&_h3]:mx-0 [&_p]:max-w-[740px] [&_p]:m-0 [&_p]:text-ink-soft [&_p]:text-meta to-680:items-stretch to-680:flex-col">
        <div>
          <Eyebrow>Environment Git authority</Eyebrow>
          <h3>{environment.name}</h3>
          <p>
            Bind this namespace to one verified GitHub App repository and
            branch. Kuberploy derives the remote and protected tenant path.
          </p>
        </div>
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      </div>

      {binding.isPending ? <Skeleton lines={5} /> : null}
      {binding.data ? (
        <div>
          <CardHeader>
            <div>
              <Eyebrow>Immutable authority</Eyebrow>
              <h3>
                {binding.data.repository.owner}/{binding.data.repository.name}
              </h3>
            </div>
            <StatusPill value={binding.data.state} />
          </CardHeader>
          <DetailList>
            <div>
              <dt>Target branch</dt>
              <dd>
                <code>{gitRefLabel(binding.data.targetRef)}</code>
              </dd>
            </div>
            <div>
              <dt>Protected path</dt>
              <dd>
                <code>{binding.data.pathPrefix}</code>
              </dd>
            </div>
            <div>
              <dt>Indexed revision</dt>
              <dd>
                {binding.data.indexedRevision
                  ? shortId(binding.data.indexedRevision, 12)
                  : "Waiting for first exact index"}
              </dd>
            </div>
            <div>
              <dt>Projection generation</dt>
              <dd>{binding.data.projectionGeneration}</dd>
            </div>
            <div>
              <dt>Updated</dt>
              <dd>{formatDate(binding.data.updatedAt)}</dd>
            </div>
          </DetailList>
          <MutedCopy>
            Clone URLs, App credentials, Secret names, and provider tokens are
            never exposed in this view.
          </MutedCopy>
        </div>
      ) : binding.error && !missing ? (
        <ErrorPanel
          error={binding.error}
          title="Could not load environment Git authority"
          onRetry={() => void binding.refetch()}
        />
      ) : !canCreate ? (
        <MutedCopy>
          This environment has no Git authority yet. An interactive project
          administrator with build-management permission must create it before
          Apps can enter the protected GitOps path.
        </MutedCopy>
      ) : (
        <form
          className="grid grid-cols-[minmax(240px,_1.4fr)_minmax(180px,_0.7fr)_auto] items-end gap-3 p-4 border border-line rounded-[10px] bg-surface to-680:grid-cols-[1fr]"
          onSubmit={submit}
        >
          <Field label="Verified installation" required>
            <Select
              value={installationId}
              onChange={(event) => {
                setInstallationId(event.target.value);
                setRepositoryId("");
                setConfirmed(false);
              }}
            >
              <option value="">Select installation</option>
              {installations.data?.items.map((installation) => (
                <option key={installation.id} value={installation.id}>
                  {installation.accountLogin}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Verified repository" required>
            <Select
              value={repositoryId}
              disabled={!installationId || repositories.isPending}
              onChange={(event) => {
                setRepositoryId(event.target.value);
                setConfirmed(false);
              }}
            >
              <option value="">Select repository</option>
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
            hint="Enter the branch name. Kuberploy stores its canonical Git ref."
          >
            <input
              value={targetRef}
              onChange={(event) => {
                setTargetRef(event.target.value);
                setConfirmed(false);
              }}
              spellCheck={false}
              autoCapitalize="none"
            />
          </Field>
          <label className="grid grid-cols-[16px_minmax(0,_1fr)] items-start gap-3 text-ink-soft cursor-pointer text-meta leading-[1.5] [&_input]:w-4 [&_input]:min-h-4 [&_input]:mt-0.5 [&_input]:mx-0 [&_input]:mb-0 [&_input]:accent-mint">
            <input
              type="checkbox"
              checked={confirmed}
              disabled={!installationId || !repositoryId}
              onChange={(event) => setConfirmed(event.target.checked)}
            />
            <span>
              I understand this creates the immutable environment Git authority
              and cannot be silently rebound.
            </span>
          </label>
          <Button
            type="submit"
            busy={create.isPending}
            disabled={!installationId || !repositoryId || !confirmed}
          >
            <Icon name="git" /> Create Git authority
          </Button>
          {validationError ? (
            <div className="col-[1_/_-1] text-tone-bad text-meta">
              {validationError}
            </div>
          ) : null}
          {installations.error || repositories.error || create.error ? (
            <div className="col-[1_/_-1] text-tone-bad text-meta">
              {errorMessage(
                installations.error ?? repositories.error ?? create.error,
              )}
            </div>
          ) : null}
        </form>
      )}
    </div>
  );
}
