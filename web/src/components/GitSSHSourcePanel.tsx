import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
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
  Select,
  Button,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  Notice,
  PageStack,
  StatusPill,
} from "./ui";
import { useCopyToClipboard } from "../lib/clipboard";

type KeyScope = "app" | "project";
type Confirmation = "rotate" | "revoke" | null;

function publicKeyFromKnownHosts(value: string | undefined): string {
  const fields = value?.trim().split(/\s+/) ?? [];
  const keyTypeIndex = fields.findIndex(
    (field) =>
      field.startsWith("ssh-") ||
      field.startsWith("ecdsa-") ||
      field.startsWith("sk-"),
  );
  return keyTypeIndex >= 0 && fields[keyTypeIndex + 1]
    ? `${fields[keyTypeIndex]} ${fields[keyTypeIndex + 1]}`
    : "";
}

function normalizeTriggerRef(value: string): string {
  const ref = value.trim();
  return ref.startsWith("refs/heads/") || ref.startsWith("refs/tags/")
    ? ref
    : `refs/heads/${ref}`;
}

export function GitSSHSourcePanel({
  application,
  project,
  enabled,
  buildConfigured = false,
  buildReady = false,
  canManageBuilds = false,
  defaultBuildPlatform = "linux/amd64",
  registryTargets = [],
}: {
  application: Application;
  project: Project;
  enabled: boolean;
  buildConfigured?: boolean;
  buildReady?: boolean;
  canManageBuilds?: boolean;
  defaultBuildPlatform?: "linux/amd64" | "linux/arm64";
  registryTargets?: RegistryTarget[];
}) {
  const client = useQueryClient();
  const [scope, setScope] = useState<KeyScope>("app");
  const [confirmation, setConfirmation] = useState<Confirmation>(null);
  const [copied, setCopied] = useState(false);
  const { copy } = useCopyToClipboard();
  const [repositoryURL, setRepositoryURL] = useState("");
  const [hostKey, setHostKey] = useState("");
  const [branch, setBranch] = useState("main");
  const [registryTargetID, setRegistryTargetID] = useState("");
  const [contextPath, setContextPath] = useState(".");
  const [dockerfilePath, setDockerfilePath] = useState("Dockerfile");
  const [amd64, setAMD64] = useState(defaultBuildPlatform === "linux/amd64");
  const [arm64, setARM64] = useState(defaultBuildPlatform === "linux/arm64");
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
    queryKey: ["app-source", application.id],
    queryFn: () => api.buildDefinitions(application.id),
    enabled: enabled && buildConfigured,
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
  useEffect(() => {
    if (!activeDefinition) return;
    setScope(activeDefinition.gitSSHKeyScope ?? "app");
    setRepositoryURL(activeDefinition.repositoryUrl ?? "");
    setHostKey(publicKeyFromKnownHosts(activeDefinition.gitSSHKnownHosts));
    setBranch(activeDefinition.triggerRef.replace(/^refs\/heads\//, ""));
    setRegistryTargetID(activeDefinition.registry.targetId);
    setContextPath(activeDefinition.contextPath);
    setDockerfilePath(activeDefinition.dockerfilePath);
    setAMD64(activeDefinition.platforms.includes("linux/amd64"));
    setARM64(activeDefinition.platforms.includes("linux/arm64"));
  }, [activeDefinition]);
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
    if (selectedQuery.error) return;
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
        queryKey: ["app-source", application.id],
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
      const platforms: CreateBuildDefinition["platforms"] = [];
      if (amd64) platforms.push("linux/amd64");
      if (arm64) platforms.push("linux/arm64");
      if (!platforms.length) throw new Error("Select at least one platform.");
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
        triggerRef: normalizeTriggerRef(branch),
        contextPath: contextPath.trim(),
        dockerfilePath: dockerfilePath.trim(),
        platforms,
        cacheTrustLane: "protected",
        cacheImports: 1,
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
    <PageStack className="grid gap-5">
      <div>
        <Eyebrow>Deploy key scope</Eyebrow>
        <h3>Choose who reuses this key</h3>
        <p className="">
          App keys isolate one repository. Project keys can be reused by Apps in{" "}
          {project.name}.
        </p>
      </div>
      <div
        className="grid grid-cols-[repeat(2,_minmax(0,_1fr))] gap-3 mb-5 to-760:grid-cols-[1fr]"
        role="radiogroup"
        aria-label="Deploy key scope"
      >
        <button
          type="button"
          role="radio"
          aria-checked={scope === "app"}
          className="grid grid-cols-[40px_minmax(0,_1fr)_18px] items-start gap-4 min-h-[132px] p-5 border border-line rounded-[10px] text-ink text-left bg-surface cursor-pointer transition-[border-color,background] duration-(--motion-fast) ease-(--ease-standard) hover:border-line-strong hover:bg-surface-soft aria-checked:border-mint aria-checked:bg-mint-soft aria-checked:shadow-[inset_0_0_0_1px_var(--mint)] [&>span:nth-child(2)]:grid [&>span:nth-child(2)]:gap-1.5 [&_strong]:text-sm [&_small]:text-ink-soft [&_small]:text-xs [&_small]:not-italic [&_small]:leading-[1.45] [&_em]:text-xs [&_em]:not-italic [&_em]:leading-[1.45] [&>svg]:w-[18px] [&>svg]:self-center [&>svg]:text-ink-faint"
          onClick={() => setScope("app")}
        >
          <span className="grid w-[38px] h-[38px] place-items-center border border-line rounded-lg text-mint-dark bg-surface-soft [&_svg]:w-[18px]">
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
          className="grid grid-cols-[40px_minmax(0,_1fr)_18px] items-start gap-4 min-h-[132px] p-5 border border-line rounded-[10px] text-ink text-left bg-surface cursor-pointer transition-[border-color,background] duration-(--motion-fast) ease-(--ease-standard) hover:border-line-strong hover:bg-surface-soft aria-checked:border-mint aria-checked:bg-mint-soft aria-checked:shadow-[inset_0_0_0_1px_var(--mint)] [&>span:nth-child(2)]:grid [&>span:nth-child(2)]:gap-1.5 [&_strong]:text-sm [&_small]:text-ink-soft [&_small]:text-xs [&_small]:not-italic [&_small]:leading-[1.45] [&_em]:text-xs [&_em]:not-italic [&_em]:leading-[1.45] [&>svg]:w-[18px] [&>svg]:self-center [&>svg]:text-ink-faint"
          onClick={() => setScope("project")}
        >
          <span className="grid w-[38px] h-[38px] place-items-center border border-line rounded-lg text-mint-dark bg-surface-soft [&_svg]:w-[18px]">
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
            setCopied(await copy(activeKey.publicKey));
          }}
          onRotate={() => setConfirmation("rotate")}
          onRevoke={() => setConfirmation("revoke")}
        />
      ) : selectedQuery.isPending ? (
        <p className="">Loading deploy keys…</p>
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
      {definitions.error ? (
        <ErrorPanel
          error={definitions.error}
          title="Git SSH source configuration could not be loaded"
          onRetry={() => void definitions.refetch()}
        />
      ) : null}
      {mutation.error ? (
        <ErrorPanel
          error={mutation.error}
          title="Git SSH key operation failed"
        />
      ) : null}
      {activeKey && buildConfigured && canManageBuilds && !definitions.error ? (
        <section className="grid gap-5 p-7 [&_+_.service-settings-section]:border-t [&_+_.service-settings-section]:border-t-line [&>.field]:max-w-[calc(50%_-_7px)] to-760:[&>.field]:max-w-[none]">
          <div className="[&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-lg [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_p]:mt-1.5 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.5] flex items-start justify-between gap-5">
            <div>
              <Eyebrow>Repository binding</Eyebrow>
              <h3>Configure Git SSH build</h3>
              <p>
                Add this deploy key to the repository, then bind its exact SSH
                host key. Kuberploy never trusts ambient known_hosts.
              </p>
            </div>
          </div>
          <div className="grid grid-cols-[repeat(2,_minmax(0,_1fr))] gap-4 to-760:grid-cols-[1fr]">
            <Field label="Repository URL" required>
              <input
                value={repositoryURL}
                onChange={(event) => setRepositoryURL(event.target.value)}
                placeholder="ssh://git@git.example.com/team/repository.git"
              />
            </Field>
            <Field
              label="Branch or tag"
              required
              hint="Use a branch name or a full refs/tags/* ref."
            >
              <input
                value={branch}
                onChange={(event) => setBranch(event.target.value)}
              />
            </Field>
            <Field label="Registry target" required>
              <Select
                value={registryTargetID}
                onChange={(event) => setRegistryTargetID(event.target.value)}
              >
                <option value="">Select registry</option>
                {registryTargets.map((target) => (
                  <option key={target.id} value={target.id}>
                    {target.name}
                  </option>
                ))}
              </Select>
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
          <fieldset className="flex items-center flex-wrap gap-y-2 gap-x-5 m-0 py-3 px-4 border border-line rounded-lg bg-surface-soft [&_legend]:float-left [&_legend]:w-full [&_legend]:p-0 [&_legend]:mb-2 [&_legend]:text-ink [&_legend]:text-meta [&_legend]:font-semibold [&_label]:inline-flex [&_label]:items-center [&_label]:gap-2 [&_label]:text-meta [&>small]:basis-full [&>small]:text-ink-faint [&>small]:text-xs [&>small]:leading-[1.45]">
            <legend>Platforms</legend>
            <label>
              <input
                type="checkbox"
                checked={amd64}
                onChange={(event) => setAMD64(event.target.checked)}
              />{" "}
              linux/amd64
            </label>
            <label>
              <input
                type="checkbox"
                checked={arm64}
                onChange={(event) => setARM64(event.target.checked)}
              />{" "}
              linux/arm64
            </label>
            <small>
              Defaults to this Kuberploy installation's CPU architecture. Select
              both only when the image must run on both architectures.
            </small>
          </fieldset>
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
            {activeDefinition ? "Save App source" : "Connect App source"}
          </Button>
        </section>
      ) : null}
      {buildConfigured && !buildReady ? (
        <Notice tone="warning">
          <div>
            <strong>Builder runtime unavailable</strong>
            <p>
              Deploy keys and repository bindings remain editable. Manual build
              execution resumes when builder capacity reports Ready.
            </p>
          </div>
        </Notice>
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
          title="Could not save Git SSH source"
        />
      ) : null}
      <Notice>
        <div>
          <strong>Next: authorize and verify the repository</strong>
          <p>
            Add the public key to the repository. Kuberploy pins the SSH host
            key before checkout. Deploy resolves the configured branch or tag
            head. Git SSH has no automatic provider webhook by default.
          </p>
        </div>
      </Notice>
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
          onConfirm={() => {
            if (selectedQuery.error) return;
            mutate(confirmation);
          }}
        />
      ) : null}
    </PageStack>
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
    <div className="grid gap-4 p-4 border border-[var(--border)] rounded-lg bg-[var(--surface-raised)] [&_textarea]:w-full [&_textarea]:resize-y [&_textarea]:font-mono [&_textarea]:text-xs">
      <div className="flex items-center justify-between gap-3 flex-wrap [&>div]:grid [&>div]:gap-1">
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
      <div className="flex items-center justify-between gap-3 flex-wrap">
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
