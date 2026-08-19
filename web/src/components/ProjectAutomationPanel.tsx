import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type {
  AutomationScope,
  Capability,
  Project,
  ServiceAccount,
  ServiceAccountRole,
  ServiceAccountToken,
} from "../api/types";
import { formatDate, titleCase } from "../lib/format";
import { Icon } from "./Icon";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "./shadcn/dialog";
import { Button, ErrorPanel, Field, Skeleton, StatusPill } from "./ui";

type AccountForm = {
  name: string;
  role: ServiceAccountRole;
};

type TokenForm = {
  name: string;
  expiresAt: string;
  appRead: boolean;
  appEdit: boolean;
  buildCreate: boolean;
  logsRead: boolean;
};

type StableAttempt = { signature: string; key: string };

const roleRank: Record<ServiceAccountRole, number> = {
  viewer: 10,
  developer: 20,
  "project-admin": 30,
};

const capabilityRoleRank: Record<string, number> = {
  viewer: 10,
  developer: 20,
  "project-admin": 30,
  "organization-admin": 40,
  "platform-admin": 50,
};

const serviceAccountRoles: ServiceAccountRole[] = [
  "viewer",
  "developer",
  "project-admin",
];

const scopeFields: Array<{
  field: keyof Pick<
    TokenForm,
    "appRead" | "appEdit" | "buildCreate" | "logsRead"
  >;
  value: AutomationScope;
  label: string;
  description: string;
}> = [
  {
    field: "appRead",
    value: "app.read",
    label: "Read applications",
    description: "Projects, applications, deployments, status, and metrics.",
  },
  {
    field: "appEdit",
    value: "app.edit",
    label: "Edit applications",
    description: "Create deployments and update Git-backed configuration.",
  },
  {
    field: "buildCreate",
    value: "build.create",
    label: "Create builds",
    description: "Start and manage source builds when builders are available.",
  },
  {
    field: "logsRead",
    value: "logs.read",
    label: "Read logs",
    description: "Read workload logs allowed by the current project grant.",
  },
];

function localDateTime(date: Date): string {
  const shifted = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return shifted.toISOString().slice(0, 16);
}

function defaultExpiry(): string {
  return localDateTime(new Date(Date.now() + 30 * 24 * 60 * 60 * 1_000));
}

function capabilityCoversProject(capability: Capability, project: Project) {
  return (
    (capability.scopeType === "platform" &&
      capability.scopeId === "platform") ||
    (capability.scopeType === "team" &&
      !!project.teamId &&
      capability.scopeId === project.teamId) ||
    (capability.scopeType === "project" && capability.scopeId === project.id)
  );
}

function scopesFromForm(value: TokenForm): AutomationScope[] {
  return scopeFields
    .filter(({ field }) => value[field])
    .map(({ value: scope }) => scope);
}

function tokenState(token: ServiceAccountToken) {
  if (token.revokedAt) return "revoked";
  if (new Date(token.expiresAt).getTime() <= Date.now()) return "expired";
  return "active";
}

export function ProjectAutomationPanel({
  project,
  capabilities,
  onClose,
}: {
  project: Project;
  capabilities: Capability[];
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const createAttempt = useRef<StableAttempt | null>(null);
  const [expandedAccountId, setExpandedAccountId] = useState<string | null>(
    null,
  );
  const [confirmAccount, setConfirmAccount] = useState<{
    account: ServiceAccount;
    key: string;
  } | null>(null);
  const [confirmation, setConfirmation] = useState("");
  const confirmAccountRef = useRef(confirmAccount);
  confirmAccountRef.current = confirmAccount;
  const accounts = useQuery({
    queryKey: ["service-accounts", project.id],
    queryFn: () => api.serviceAccounts(project.id),
  });
  const managingCapabilities = useMemo(
    () =>
      capabilities.filter(
        (capability) =>
          capability.actions?.includes("access-grants:create") &&
          capability.actions.includes("access-grants:delete") &&
          capabilityCoversProject(capability, project),
      ),
    [capabilities, project],
  );
  const canManageRole = (role: ServiceAccountRole) =>
    managingCapabilities.some(
      (capability) =>
        (capabilityRoleRank[capability.role ?? ""] ?? 0) >= roleRank[role],
    );
  const assignableRoles = serviceAccountRoles.filter(canManageRole);
  const form = useForm<AccountForm>({
    defaultValues: { name: "", role: "developer" },
  });
  const createAccount = useMutation({
    mutationFn: (input: {
      projectId: string;
      value: AccountForm;
      idempotencyKey: string;
    }) =>
      api.createServiceAccount(
        input.projectId,
        input.value,
        input.idempotencyKey,
      ),
    onSuccess: async (_result, input) => {
      if (input.projectId !== project.id) return;
      const current = form.getValues();
      const currentSignature = JSON.stringify({
        ...current,
        name: current.name.trim(),
      });
      if (currentSignature === JSON.stringify(input.value)) {
        createAttempt.current = null;
        form.reset({ name: "", role: "developer" });
      }
      await queryClient.invalidateQueries({
        queryKey: ["service-accounts", input.projectId],
      });
    },
  });
  const submitAccount = (value: AccountForm) => {
    const normalized = { ...value, name: value.name.trim() };
    const signature = JSON.stringify(normalized);
    const idempotencyKey =
      createAttempt.current?.signature === signature
        ? createAttempt.current.key
        : crypto.randomUUID();
    createAttempt.current = { signature, key: idempotencyKey };
    createAccount.mutate({
      projectId: project.id,
      value: normalized,
      idempotencyKey,
    });
  };
  const disableAccount = useMutation({
    mutationFn: (input: { account: ServiceAccount; idempotencyKey: string }) =>
      api.disableServiceAccount(input.account.id, input.idempotencyKey),
    onSuccess: async (_result, input) => {
      const current = confirmAccountRef.current;
      if (
        current?.account.id === input.account.id &&
        current.key === input.idempotencyKey
      ) {
        setConfirmAccount(null);
        setConfirmation("");
        setExpandedAccountId((expanded) =>
          expanded === input.account.id ? null : expanded,
        );
      }
      await queryClient.invalidateQueries({
        queryKey: ["service-accounts", project.id],
      });
    },
  });

  return (
    <div className="automation-panel" aria-label={`${project.name} automation`}>
      <div className="automation-panel__header">
        <div>
          <span className="eyebrow">Automation</span>
          <h3>Service accounts</h3>
          <p>
            Create project-bound identities for CI and AI agents. Every request
            must pass both the account&apos;s project role and its token&apos;s
            coarse scopes.
          </p>
        </div>
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      </div>

      {assignableRoles.length ? (
        <form
          className="automation-create-form"
          onSubmit={form.handleSubmit(submitAccount)}
        >
          <Field
            label="Service account name"
            required
            hint="Use a durable workload name, not a person or raw credential."
            error={form.formState.errors.name?.message}
          >
            <input
              autoComplete="off"
              placeholder="release-bot"
              {...form.register("name", {
                required: "Enter a service account name.",
                maxLength: {
                  value: 100,
                  message: "Use 100 characters or fewer.",
                },
                validate: (value) =>
                  value.trim().length > 0 || "Enter a service account name.",
              })}
            />
          </Field>
          <Field
            label="Project role"
            required
            hint="This remains the object-level authorization boundary."
          >
            <select {...form.register("role")}>
              {assignableRoles.map((role) => (
                <option key={role} value={role}>
                  {titleCase(role)}
                </option>
              ))}
            </select>
          </Field>
          <Button type="submit" busy={createAccount.isPending}>
            <Icon name="plus" /> Create account
          </Button>
          {createAccount.error ? (
            <div className="form-error">
              {errorMessage(createAccount.error)}
            </div>
          ) : null}
        </form>
      ) : (
        <p className="muted-copy">
          You can inspect these identities, but your current grant cannot create
          or manage them.
        </p>
      )}

      {accounts.isPending ? <Skeleton lines={4} /> : null}
      {accounts.error ? (
        <ErrorPanel
          error={accounts.error}
          title="Could not load service accounts"
          onRetry={() => void accounts.refetch()}
        />
      ) : null}
      {accounts.data?.items.length ? (
        <div className="automation-account-list">
          {accounts.data.items.map((account) => {
            const expanded = expandedAccountId === account.id;
            const manageable = canManageRole(account.role);
            return (
              <div className="automation-account" key={account.id}>
                <div className="automation-account__summary">
                  <span className="automation-account__icon">
                    <Icon name="terminal" />
                  </span>
                  <div>
                    <strong>{account.name}</strong>
                    <small>
                      {titleCase(account.role)} · created{" "}
                      {formatDate(account.createdAt)}
                    </small>
                  </div>
                  <StatusPill
                    value={account.disabledAt ? "disabled" : "active"}
                  />
                  <div className="automation-account__actions">
                    <Button
                      variant="secondary"
                      onClick={() =>
                        setExpandedAccountId(expanded ? null : account.id)
                      }
                      aria-expanded={expanded}
                    >
                      {expanded ? "Hide credentials" : "Credentials"}
                    </Button>
                    {!account.disabledAt && manageable ? (
                      <Button
                        variant="danger"
                        onClick={() => {
                          setConfirmAccount({
                            account,
                            key: crypto.randomUUID(),
                          });
                          setConfirmation("");
                        }}
                      >
                        Disable
                      </Button>
                    ) : null}
                  </div>
                </div>
                {expanded ? (
                  <AccountTokens
                    account={account}
                    canManage={manageable && !account.disabledAt}
                  />
                ) : null}
              </div>
            );
          })}
        </div>
      ) : !accounts.isPending && !accounts.error ? (
        <div className="automation-empty">
          <Icon name="terminal" />
          <div>
            <strong>No service accounts</strong>
            <p>
              Create one for CI or agent access without sharing a human session.
            </p>
          </div>
        </div>
      ) : null}

      {confirmAccount ? (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open && !disableAccount.isPending) {
              setConfirmAccount(null);
              setConfirmation("");
            }
          }}
        >
          <DialogContent
            className="confirmation-dialog max-w-none"
            role="alertdialog"
            showCloseButton={false}
          >
            <span className="confirmation-dialog__icon">
              <Icon name="terminal" />
            </span>
            <span className="eyebrow">Immediate revocation</span>
            <DialogTitle>Disable {confirmAccount.account.name}?</DialogTitle>
            <DialogDescription>
              This disables the identity and revokes all of its tokens. Type the
              exact service account name to continue.
            </DialogDescription>
            <Field label="Exact service account name" required>
              <input
                autoFocus
                value={confirmation}
                onChange={(event) => setConfirmation(event.target.value)}
              />
            </Field>
            <div className="confirmation-dialog__actions">
              <Button
                variant="danger"
                disabled={confirmation !== confirmAccount.account.name}
                busy={disableAccount.isPending}
                onClick={() =>
                  disableAccount.mutate({
                    account: confirmAccount.account,
                    idempotencyKey: confirmAccount.key,
                  })
                }
              >
                Disable and revoke tokens
              </Button>
              <Button
                variant="secondary"
                disabled={disableAccount.isPending}
                onClick={() => {
                  setConfirmAccount(null);
                  setConfirmation("");
                }}
              >
                Cancel
              </Button>
            </div>
            {disableAccount.error ? (
              <div className="form-error" role="alert">
                {errorMessage(disableAccount.error)}
              </div>
            ) : null}
          </DialogContent>
        </Dialog>
      ) : null}
    </div>
  );
}

function AccountTokens({
  account,
  canManage,
}: {
  account: ServiceAccount;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const issueAttempt = useRef<StableAttempt | null>(null);
  const [issuePending, setIssuePending] = useState(false);
  const [issueError, setIssueError] = useState<unknown>();
  const [issuedCredential, setIssuedCredential] = useState<{
    token: string;
    record: ServiceAccountToken;
  } | null>(null);
  const [copyState, setCopyState] = useState<"idle" | "copied" | "failed">(
    "idle",
  );
  const [replayNotice, setReplayNotice] = useState(false);
  const [confirmToken, setConfirmToken] = useState<{
    token: ServiceAccountToken;
    key: string;
  } | null>(null);
  const [confirmation, setConfirmation] = useState("");
  const confirmTokenRef = useRef(confirmToken);
  confirmTokenRef.current = confirmToken;
  const tokens = useQuery({
    queryKey: ["service-account-tokens", account.id],
    queryFn: () => api.serviceAccountTokens(account.id),
  });
  const form = useForm<TokenForm>({
    defaultValues: {
      name: "",
      expiresAt: defaultExpiry(),
      appRead: true,
      appEdit: false,
      buildCreate: false,
      logsRead: false,
    },
  });
  const submitToken = async (value: TokenForm) => {
    const scopes = scopesFromForm(value);
    if (!scopes.length) {
      setIssueError(new Error("Select at least one token scope."));
      return;
    }
    const expiry = new Date(value.expiresAt);
    if (Number.isNaN(expiry.getTime())) {
      setIssueError(new Error("Enter a valid expiration date and time."));
      return;
    }
    const normalized = {
      name: value.name.trim(),
      scopes,
      expiresAt: expiry.toISOString(),
    };
    const signature = JSON.stringify(normalized);
    const idempotencyKey =
      issueAttempt.current?.signature === signature
        ? issueAttempt.current.key
        : crypto.randomUUID();
    issueAttempt.current = { signature, key: idempotencyKey };
    setIssuePending(true);
    setIssueError(undefined);
    setReplayNotice(false);
    try {
      const issue = await api.createServiceAccountToken(
        account.id,
        normalized,
        idempotencyKey,
      );
      const current = form.getValues();
      const currentExpiry = new Date(current.expiresAt);
      const currentNormalized = {
        name: current.name.trim(),
        scopes: scopesFromForm(current),
        expiresAt: Number.isNaN(currentExpiry.getTime())
          ? current.expiresAt
          : currentExpiry.toISOString(),
      };
      if (JSON.stringify(currentNormalized) === JSON.stringify(normalized)) {
        issueAttempt.current = null;
        form.reset({
          name: "",
          expiresAt: defaultExpiry(),
          appRead: true,
          appEdit: false,
          buildCreate: false,
          logsRead: false,
        });
      }
      await queryClient.invalidateQueries({
        queryKey: ["service-account-tokens", account.id],
      });
      if (issue.token) {
        setIssuedCredential({ token: issue.token, record: issue.tokenRecord });
        setCopyState("idle");
      } else {
        setIssuedCredential(null);
        setReplayNotice(true);
      }
    } catch (error) {
      setIssueError(error);
    } finally {
      setIssuePending(false);
    }
  };
  const revokeToken = useMutation({
    mutationFn: (input: {
      token: ServiceAccountToken;
      idempotencyKey: string;
    }) =>
      api.revokeServiceAccountToken(
        account.id,
        input.token.id,
        input.idempotencyKey,
      ),
    onSuccess: async (_result, input) => {
      const current = confirmTokenRef.current;
      if (
        current?.token.id === input.token.id &&
        current.key === input.idempotencyKey
      ) {
        setConfirmToken(null);
        setConfirmation("");
      }
      await queryClient.invalidateQueries({
        queryKey: ["service-account-tokens", account.id],
      });
    },
  });
  const copyCredential = async () => {
    if (!issuedCredential) return;
    try {
      await navigator.clipboard.writeText(issuedCredential.token);
      setCopyState("copied");
    } catch {
      setCopyState("failed");
    }
  };

  return (
    <div className="automation-tokens">
      <div className="automation-tokens__heading">
        <div>
          <h4>Expiring bearer tokens</h4>
          <p>
            The full credential is shown once. Stored records contain only a
            non-secret prefix and SHA-256 hash.
          </p>
        </div>
      </div>

      {canManage ? (
        <form
          className="automation-token-form"
          onSubmit={form.handleSubmit((value) => void submitToken(value))}
        >
          <fieldset disabled={issuePending}>
            <div className="automation-token-form__identity">
              <Field
                label="Token name"
                required
                error={form.formState.errors.name?.message}
              >
                <input
                  autoComplete="off"
                  placeholder="production deploy"
                  {...form.register("name", {
                    required: "Enter a token name.",
                    maxLength: {
                      value: 100,
                      message: "Use 100 characters or fewer.",
                    },
                    validate: (value) =>
                      value.trim().length > 0 || "Enter a token name.",
                  })}
                />
              </Field>
              <Field
                label="Expires at"
                required
                hint="Must be more than 5 minutes and no more than 90 days away."
                error={form.formState.errors.expiresAt?.message}
              >
                <input
                  type="datetime-local"
                  min={localDateTime(new Date(Date.now() + 6 * 60 * 1_000))}
                  max={localDateTime(
                    new Date(Date.now() + 90 * 24 * 60 * 60 * 1_000),
                  )}
                  {...form.register("expiresAt", {
                    required: "Choose when this token expires.",
                    validate: (value) => {
                      const expiry = new Date(value).getTime();
                      const ttl = expiry - Date.now();
                      if (Number.isNaN(expiry))
                        return "Enter a valid date and time.";
                      if (ttl <= 5 * 60 * 1_000)
                        return "Expiration must be more than 5 minutes away.";
                      if (ttl > 90 * 24 * 60 * 60 * 1_000)
                        return "Expiration cannot be more than 90 days away.";
                      return true;
                    },
                  })}
                />
              </Field>
            </div>
            <fieldset className="automation-scopes">
              <legend>Token scopes</legend>
              {scopeFields.map((scope) => (
                <label key={scope.value} className="automation-scope">
                  <input type="checkbox" {...form.register(scope.field)} />
                  <span>
                    <strong>{scope.value}</strong>
                    <small>
                      {scope.label}. {scope.description}
                    </small>
                  </span>
                </label>
              ))}
            </fieldset>
            <div className="automation-token-form__actions">
              <Button type="submit" busy={issuePending}>
                Issue one-time token
              </Button>
              <small>
                Token scopes can only reduce the account&apos;s project role.
              </small>
            </div>
            {issueError ? (
              <div className="form-error">{errorMessage(issueError)}</div>
            ) : null}
          </fieldset>
        </form>
      ) : null}

      {replayNotice ? (
        <div className="notice notice--warning" role="alert">
          <div>
            <strong>Token created, credential no longer available</strong>
            <p>
              This was an idempotent replay, so the API did not disclose the raw
              token again. Revoke the record and issue a new token if it was not
              saved.
            </p>
          </div>
          <Button variant="secondary" onClick={() => setReplayNotice(false)}>
            Dismiss
          </Button>
        </div>
      ) : null}

      {tokens.isPending ? <Skeleton lines={3} /> : null}
      {tokens.error ? (
        <ErrorPanel
          error={tokens.error}
          title="Could not load token records"
          onRetry={() => void tokens.refetch()}
        />
      ) : null}
      {tokens.data?.items.length ? (
        <div className="automation-token-list">
          {tokens.data.items.map((token) => {
            const state = tokenState(token);
            return (
              <div className="automation-token" key={token.id}>
                <div>
                  <strong>{token.name}</strong>
                  <code>{token.prefix}••••••••</code>
                </div>
                <div className="automation-token__scopes">
                  {token.scopes.map((scope) => (
                    <span key={scope}>{scope}</span>
                  ))}
                </div>
                <small>
                  Expires {formatDate(token.expiresAt)} · Last used{" "}
                  {formatDate(token.lastUsedAt)}
                </small>
                <StatusPill value={state} />
                {canManage && state === "active" ? (
                  <Button
                    variant="danger"
                    onClick={() => {
                      setConfirmToken({ token, key: crypto.randomUUID() });
                      setConfirmation("");
                    }}
                  >
                    Revoke
                  </Button>
                ) : null}
              </div>
            );
          })}
        </div>
      ) : !tokens.isPending && !tokens.error ? (
        <p className="muted-copy">No token records for this account.</p>
      ) : null}

      {issuedCredential ? (
        <Dialog
          open
          // The raw credential is unrecoverable after dismissal. Keep this
          // dialog open for Escape and backdrop clicks; the explicit dismiss
          // action below is the only safe close path.
          onOpenChange={() => undefined}
        >
          <DialogContent
            className="confirmation-dialog token-issue-dialog max-w-none"
            role="alertdialog"
            showCloseButton={false}
          >
            <span className="confirmation-dialog__icon">
              <Icon name="check" />
            </span>
            <span className="eyebrow">Shown exactly once</span>
            <DialogTitle>Copy this token now</DialogTitle>
            <DialogDescription>
              Kuberploy cannot display this credential again. Store it in your
              CI secret manager, then dismiss this dialog.
            </DialogDescription>
            <code
              className="token-secret"
              aria-label="New service account token"
            >
              {issuedCredential.token}
            </code>
            <dl className="confirmation-identity">
              <div>
                <dt>Token</dt>
                <dd>{issuedCredential.record.name}</dd>
              </div>
              <div>
                <dt>Expires</dt>
                <dd>{formatDate(issuedCredential.record.expiresAt)}</dd>
              </div>
            </dl>
            <div className="confirmation-dialog__actions">
              <Button onClick={() => void copyCredential()}>
                {copyState === "copied" ? "Copied" : "Copy token"}
              </Button>
              <Button
                variant="secondary"
                onClick={() => {
                  setIssuedCredential(null);
                  setCopyState("idle");
                }}
              >
                I saved it — dismiss
              </Button>
            </div>
            {copyState === "failed" ? (
              <div className="form-error">
                Clipboard access failed. Select and copy the token manually
                before dismissing.
              </div>
            ) : null}
          </DialogContent>
        </Dialog>
      ) : null}

      {confirmToken ? (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open && !revokeToken.isPending) {
              setConfirmToken(null);
              setConfirmation("");
            }
          }}
        >
          <DialogContent
            className="confirmation-dialog max-w-none"
            role="alertdialog"
            showCloseButton={false}
          >
            <span className="confirmation-dialog__icon">
              <Icon name="terminal" />
            </span>
            <span className="eyebrow">Immediate revocation</span>
            <DialogTitle>Revoke {confirmToken.token.name}?</DialogTitle>
            <DialogDescription>
              Requests using this credential will fail immediately. Type its
              exact non-secret prefix to continue:{" "}
              <code>{confirmToken.token.prefix}</code>
            </DialogDescription>
            <Field label="Exact token prefix" required>
              <input
                autoFocus
                value={confirmation}
                onChange={(event) => setConfirmation(event.target.value)}
              />
            </Field>
            <div className="confirmation-dialog__actions">
              <Button
                variant="danger"
                disabled={confirmation !== confirmToken.token.prefix}
                busy={revokeToken.isPending}
                onClick={() =>
                  revokeToken.mutate({
                    token: confirmToken.token,
                    idempotencyKey: confirmToken.key,
                  })
                }
              >
                Revoke exact token
              </Button>
              <Button
                variant="secondary"
                disabled={revokeToken.isPending}
                onClick={() => {
                  setConfirmToken(null);
                  setConfirmation("");
                }}
              >
                Cancel
              </Button>
            </div>
            {revokeToken.error ? (
              <div className="form-error" role="alert">
                {errorMessage(revokeToken.error)}
              </div>
            ) : null}
          </DialogContent>
        </Dialog>
      ) : null}
    </div>
  );
}
