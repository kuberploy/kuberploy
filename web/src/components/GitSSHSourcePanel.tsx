import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useRef, useState } from "react";
import { api } from "../api/client";
import type {
  Application,
  CreateBuildDefinition,
  GitSSHKey,
  Project,
  RegistryTarget,
} from "../api/types";
import { Icon } from "./Icon";
import {
  Button,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Field,
  StatusPill,
} from "./ui";

type KeyScope = "app" | "project";
type Confirmation = "rotate" | "revoke" | null;

export function GitSSHSourcePanel({
  application,
  project,
  enabled,
  buildEnabled = false,
  canManageBuilds = false,
  registryTargets = [],
}: {
  application: Application;
  project: Project;
  enabled: boolean;
  buildEnabled?: boolean;
  canManageBuilds?: boolean;
  registryTargets?: RegistryTarget[];
}) {
  const client = useQueryClient();
  const [scope, setScope] = useState<KeyScope>("app");
  const [confirmation, setConfirmation] = useState<Confirmation>(null);
  const [copied, setCopied] = useState(false);
  const [repositoryURL, setRepositoryURL] = useState("");
  const [hostKey, setHostKey] = useState("");
  const [branch, setBranch] = useState("main");
  const [registryTargetID, setRegistryTargetID] = useState("");
  const [contextPath, setContextPath] = useState(".");
  const [dockerfilePath, setDockerfilePath] = useState("Dockerfile");
  const [commitSHA, setCommitSHA] = useState("");
  const [formError, setFormError] = useState("");
  const attempt = useRef<{
    operation: "create" | "rotate" | "revoke";
    scope: KeyScope;
    key: string;
  } | null>(null);
  const projectKeys = useQuery({
    queryKey: ["git-ssh-keys", "project", project.id],
    queryFn: () => api.projectGitSSHKeys(project.id),
    enabled,
    retry: false,
  });
  const appKeys = useQuery({
    queryKey: ["git-ssh-keys", "app", application.id],
    queryFn: () => api.applicationGitSSHKeys(application.id),
    enabled,
    retry: false,
  });
  const selectedQuery = scope === "project" ? projectKeys : appKeys;
  const selectedOwner = scope === "project" ? project.id : application.id;
  const activeKey = useMemo(
    () => selectedQuery.data?.items.find((item) => item.status === "active"),
    [selectedQuery.data?.items],
  );
  const definitions = useQuery({
    queryKey: ["build-definitions", application.id],
    queryFn: () => api.buildDefinitions(application.id),
    enabled,
    retry: false,
  });
  const activeDefinition = useMemo(
    () =>
      definitions.data?.items.find(
        (definition) =>
          definition.sourceKind === "git_ssh" && definition.enabled,
      ),
    [definitions.data?.items],
  );
  const mutation = useMutation({
    mutationFn: (input: {
      operation: "create" | "rotate" | "revoke";
      scope: KeyScope;
      ownerId: string;
      idempotencyKey: string;
    }) => {
      if (input.operation === "create") {
        return api.createGitSSHKey(
          input.scope,
          input.ownerId,
          input.idempotencyKey,
        );
      }
      if (input.operation === "rotate") {
        return api.rotateGitSSHKey(
          input.scope,
          input.ownerId,
          input.idempotencyKey,
        );
      }
      return api.revokeGitSSHKey(
        input.scope,
        input.ownerId,
        input.idempotencyKey,
      );
    },
    onSuccess: async (_, input) => {
      if (attempt.current?.key === input.idempotencyKey) attempt.current = null;
      setConfirmation(null);
      setCopied(false);
      await client.invalidateQueries({
        queryKey: ["git-ssh-keys", input.scope, input.ownerId],
      });
    },
  });
  const mutate = (operation: "create" | "rotate" | "revoke") => {
    const idempotencyKey =
      attempt.current?.operation === operation &&
      attempt.current.scope === scope
        ? attempt.current.key
        : crypto.randomUUID();
    attempt.current = { operation, scope, key: idempotencyKey };
    mutation.mutate({
      operation,
      scope,
      ownerId: selectedOwner,
      idempotencyKey,
    });
  };
  const createDefinition = useMutation({
    mutationFn: (input: CreateBuildDefinition) =>
      api.createBuildDefinition(application.id, input, crypto.randomUUID()),
    onSuccess: async () => {
      setFormError("");
      await client.invalidateQueries({
        queryKey: ["build-definitions", application.id],
      });
    },
  });
  const createBuild = useMutation({
    mutationFn: (input: { definitionId: string; commitSha: string }) =>
      api.createManualBuildAttempt(
        input.definitionId,
        input.commitSha,
        crypto.randomUUID(),
      ),
    onSuccess: async () => {
      setCommitSHA("");
      setFormError("");
      await client.invalidateQueries({
        queryKey: ["build-attempts", application.id],
      });
    },
  });

  const submitDefinition = () => {
    setFormError("");
    if (!activeKey) return;
    try {
      const parsed = new URL(repositoryURL.trim());
      if (parsed.protocol !== "ssh:" || !parsed.username || !parsed.hostname) {
        throw new Error("Use an ssh://user@host/path repository URL.");
      }
      const port = parsed.port || "22";
      if (!registryTargetID) throw new Error("Select a registry target.");
      if (!hostKey.trim())
        throw new Error("Paste the provider SSH host public key.");
      createDefinition.mutate({
        sourceKind: "git_ssh",
        repositoryUrl: parsed.toString(),
        gitSSHKeyScope: scope,
        gitSSHKeyRevision: activeKey.revision,
        hostKeyPins: [
          {
            endpoint: `${parsed.hostname}:${port}`,
            publicKey: hostKey.trim(),
          },
        ],
        registryTargetId: registryTargetID,
        triggerRef: `refs/heads/${branch.trim()}`,
        contextPath: contextPath.trim(),
        dockerfilePath: dockerfilePath.trim(),
        platforms: ["linux/amd64"],
        cacheTrustLane: "protected",
        cacheImports: 2,
        profile: {
          resource: "standard",
          timeoutSeconds: 900,
          egress: "registry-and-source",
        },
        maxAttempts: 3,
      });
    } catch (error) {
      setFormError(
        error instanceof Error ? error.message : "Invalid Git SSH source.",
      );
    }
  };

  const submitBuild = () => {
    const commit = commitSHA.trim();
    if (!activeDefinition || !/^[a-f0-9]{40}$/.test(commit)) {
      setFormError("Enter the exact lowercase 40-character commit SHA.");
      return;
    }
    createBuild.mutate({
      definitionId: activeDefinition.id,
      commitSha: commit,
    });
  };

  if (!enabled) {
    return (
      <EmptyState
        icon="terminal"
        title="Git SSH is unavailable"
        description="The installation must configure encrypted Git SSH key storage before Apps can use generic Git repositories."
      />
    );
  }

  return (
    <div className="page-stack git-ssh-source-panel">
      <div>
        <span className="eyebrow">Deploy key scope</span>
        <h3>Choose who reuses this key</h3>
        <p className="muted">
          App keys isolate one repository. Project keys can be reused by Apps in{" "}
          {project.name}.
        </p>
      </div>
      <div
        className="app-source-grid"
        role="radiogroup"
        aria-label="Deploy key scope"
      >
        <button
          type="button"
          role="radio"
          aria-checked={scope === "app"}
          className="app-source-option"
          onClick={() => setScope("app")}
        >
          <span className="app-source-option__icon">
            <Icon name="apps" />
          </span>
          <span>
            <strong>App key</strong>
            <small>Recommended isolation for {application.name}.</small>
          </span>
        </button>
        <button
          type="button"
          role="radio"
          aria-checked={scope === "project"}
          className="app-source-option"
          onClick={() => setScope("project")}
        >
          <span className="app-source-option__icon">
            <Icon name="layers" />
          </span>
          <span>
            <strong>Project key</strong>
            <small>Reuse across trusted repositories.</small>
          </span>
        </button>
      </div>

      {selectedQuery.error ? (
        <ErrorPanel
          error={selectedQuery.error}
          onRetry={() => void selectedQuery.refetch()}
          title="Git SSH keys could not be loaded"
        />
      ) : activeKey ? (
        <KeyCard
          value={activeKey}
          copied={copied}
          busy={mutation.isPending}
          onCopy={async () => {
            await navigator.clipboard.writeText(activeKey.publicKey);
            setCopied(true);
          }}
          onRotate={() => setConfirmation("rotate")}
          onRevoke={() => setConfirmation("revoke")}
        />
      ) : selectedQuery.isPending ? (
        <p className="muted">Loading deploy keys…</p>
      ) : (
        <EmptyState
          compact
          icon="terminal"
          title={`No active ${scope === "app" ? "App" : "Project"} key`}
          description="Generate an Ed25519 public key, then add it as a read-only deploy key in the Git provider."
          action={
            <Button busy={mutation.isPending} onClick={() => mutate("create")}>
              <Icon name="plus" /> Generate deploy key
            </Button>
          }
        />
      )}
      {mutation.error ? (
        <ErrorPanel
          error={mutation.error}
          title="Git SSH key operation failed"
        />
      ) : null}
      {activeKey && buildEnabled && canManageBuilds ? (
        <section className="service-settings-section">
          <div className="service-settings-section__header">
            <div>
              <span className="eyebrow">Repository binding</span>
              <h3>Configure Git SSH build</h3>
              <p>
                Add this deploy key to the repository, then bind its exact SSH
                host key. Kuberploy never trusts ambient known_hosts.
              </p>
            </div>
          </div>
          <div className="build-definition-form__grid">
            <Field label="Repository URL" required>
              <input
                value={repositoryURL}
                onChange={(event) => setRepositoryURL(event.target.value)}
                placeholder="ssh://git@git.example.com/team/repository.git"
              />
            </Field>
            <Field label="Branch" required>
              <input
                value={branch}
                onChange={(event) => setBranch(event.target.value)}
              />
            </Field>
            <Field label="Registry target" required>
              <select
                value={registryTargetID}
                onChange={(event) => setRegistryTargetID(event.target.value)}
              >
                <option value="">Select registry</option>
                {registryTargets.map((target) => (
                  <option key={target.id} value={target.id}>
                    {target.name}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Build context" required>
              <input
                value={contextPath}
                onChange={(event) => setContextPath(event.target.value)}
              />
            </Field>
            <Field label="Dockerfile" required>
              <input
                value={dockerfilePath}
                onChange={(event) => setDockerfilePath(event.target.value)}
              />
            </Field>
          </div>
          <Field
            label="SSH host public key"
            required
            hint="Paste the provider-published ssh-ed25519, ecdsa, or RSA host public key. This is not your deploy key."
          >
            <textarea
              rows={3}
              value={hostKey}
              onChange={(event) => setHostKey(event.target.value)}
              placeholder="ssh-ed25519 AAAA…"
            />
          </Field>
          <Button
            busy={createDefinition.isPending}
            disabled={!registryTargets.length}
            onClick={submitDefinition}
          >
            <Icon name="plus" />{" "}
            {activeDefinition ? "Replace binding" : "Create binding"}
          </Button>
        </section>
      ) : null}
      {activeDefinition && buildEnabled && canManageBuilds ? (
        <section className="service-settings-section">
          <div>
            <span className="eyebrow">Manual build</span>
            <h3>Build exact commit</h3>
            <p className="muted">
              Git SSH has no provider webhook. Paste the commit SHA after the
              provider accepts this deploy key.
            </p>
          </div>
          <Field label="Commit SHA" required>
            <input
              value={commitSHA}
              onChange={(event) => setCommitSHA(event.target.value)}
              placeholder="40 lowercase hexadecimal characters"
            />
          </Field>
          <Button busy={createBuild.isPending} onClick={submitBuild}>
            <Icon name="deploy" /> Build commit
          </Button>
        </section>
      ) : null}
      {formError ? (
        <ErrorPanel
          error={new Error(formError)}
          title="Git SSH source is invalid"
        />
      ) : null}
      {createDefinition.error ? (
        <ErrorPanel
          error={createDefinition.error}
          title="Git SSH binding failed"
        />
      ) : null}
      {createBuild.error ? (
        <ErrorPanel error={createBuild.error} title="Git SSH build failed" />
      ) : null}
      <div className="notice">
        <div>
          <strong>Next: authorize and verify the repository</strong>
          <p>
            Add the public key to the repository. Kuberploy pins the SSH host
            key before checkout. Git SSH uses manual or API-triggered builds
            because it has no provider webhook.
          </p>
        </div>
      </div>
      {confirmation ? (
        <ConfirmDialog
          title={
            confirmation === "rotate"
              ? "Rotate deploy key?"
              : "Revoke deploy key?"
          }
          description={
            confirmation === "rotate"
              ? "The current public key stops working immediately. Add the new public key to every linked repository before the next checkout."
              : "The private key is disabled immediately and future repository checkouts fail until a new key is configured."
          }
          confirmLabel={confirmation === "rotate" ? "Rotate key" : "Revoke key"}
          busy={mutation.isPending}
          onCancel={() => setConfirmation(null)}
          onConfirm={() => mutate(confirmation)}
        />
      ) : null}
    </div>
  );
}

function KeyCard({
  value,
  copied,
  busy,
  onCopy,
  onRotate,
  onRevoke,
}: {
  value: GitSSHKey;
  copied: boolean;
  busy: boolean;
  onCopy: () => Promise<void>;
  onRotate: () => void;
  onRevoke: () => void;
}) {
  return (
    <div className="git-ssh-key-card">
      <div className="git-ssh-key-card__header">
        <div>
          <strong>
            {value.scope === "app" ? "App" : "Project"} deploy key
          </strong>
          <small>
            Revision {value.revision} · {value.fingerprint}
          </small>
        </div>
        <StatusPill value={value.status} />
      </div>
      <textarea
        aria-label="SSH public key"
        readOnly
        rows={3}
        value={value.publicKey}
      />
      <div className="git-ssh-key-card__actions">
        <Button
          variant="secondary"
          disabled={busy}
          onClick={() => void onCopy()}
        >
          <Icon name="code" /> {copied ? "Copied" : "Copy public key"}
        </Button>
        <Button variant="secondary" disabled={busy} onClick={onRotate}>
          <Icon name="refresh" /> Rotate
        </Button>
        <Button variant="danger" disabled={busy} onClick={onRevoke}>
          Revoke
        </Button>
      </div>
    </div>
  );
}
