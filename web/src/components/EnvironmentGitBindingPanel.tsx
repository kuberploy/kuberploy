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
import { Button, ErrorPanel, Field, Skeleton, StatusPill } from "./ui";

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
  const [installationId, setInstallationId] = useState("");
  const [repositoryId, setRepositoryId] = useState("");
  const [targetRef, setTargetRef] = useState("main");
  const [confirmed, setConfirmed] = useState(false);
  const [validationError, setValidationError] = useState("");
  const environmentRef = useRef(environment.id);
  environmentRef.current = environment.id;

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

  useEffect(() => {
    if (
      repositoryId === "" ||
      !activeRepositories.some((item) => item.id === repositoryId)
    ) {
      setRepositoryId(activeRepositories[0]?.id ?? "");
    }
  }, [activeRepositories, repositoryId]);

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

  useEffect(() => {
    attempt.current = null;
    setInstallationId("");
    setRepositoryId("");
    setTargetRef("main");
    setConfirmed(false);
    setValidationError("");
    create.reset();
  }, [environment.id]);

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
      className="automation-panel"
      aria-label={`${environment.name} Git authority`}
    >
      <div className="automation-panel__header">
        <div>
          <span className="eyebrow">Environment Git authority</span>
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
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">Immutable authority</span>
              <h3>
                {binding.data.repository.owner}/{binding.data.repository.name}
              </h3>
            </div>
            <StatusPill value={binding.data.state} />
          </div>
          <dl className="detail-list">
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
          </dl>
          <p className="muted-copy">
            Clone URLs, App credentials, Secret names, and provider tokens are
            never exposed in this view.
          </p>
        </div>
      ) : binding.error && !missing ? (
        <ErrorPanel
          error={binding.error}
          title="Could not load environment Git authority"
          onRetry={() => void binding.refetch()}
        />
      ) : !canCreate ? (
        <p className="muted-copy">
          This environment has no Git authority yet. An interactive project
          administrator with build-management permission must create it before
          Apps can enter the protected GitOps path.
        </p>
      ) : (
        <form className="automation-create-form" onSubmit={submit}>
          <Field label="Verified installation" required>
            <select
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
            </select>
          </Field>
          <Field label="Verified repository" required>
            <select
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
            </select>
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
          <label className="checkbox-row">
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
            <div className="form-error">{validationError}</div>
          ) : null}
          {installations.error || repositories.error || create.error ? (
            <div className="form-error">
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
