import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type {
  RegistryTarget,
  RegistryTargetInput,
  RegistryTargetMode,
} from "../api/types";
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
import { formatDate } from "../lib/format";
import { hasRegistryPlatformCapability } from "../lib/registryAccess";

type TargetDraft = RegistryTargetInput;

const emptyTarget: TargetDraft = {
  name: "",
  mode: "external",
  endpoint: "",
  repositoryPrefix: "",
  pullCredentialRef: "",
  pushCredentialRef: "",
  cacheCredentialRef: "",
};

function targetDraft(target?: RegistryTarget): TargetDraft {
  if (!target) return { ...emptyTarget };
  return {
    name: target.name,
    mode: target.mode,
    endpoint: target.endpoint,
    repositoryPrefix: target.repositoryPrefix,
    pullCredentialRef: target.pullCredentialRef ?? "",
    pushCredentialRef: target.pushCredentialRef ?? "",
    cacheCredentialRef: target.cacheCredentialRef ?? "",
  };
}

function validateTarget(draft: TargetDraft) {
  const errors: Partial<Record<keyof TargetDraft, string>> = {};
  if (!draft.name.trim()) errors.name = "Enter a target name.";
  if (!draft.endpoint.trim()) errors.endpoint = "Enter a registry endpoint.";
  if (!draft.repositoryPrefix.trim())
    errors.repositoryPrefix = "Enter the repository prefix Kuberploy owns.";
  if (draft.mode !== "managed" && draft.mode !== "external")
    errors.mode = "Select a supported target mode.";
  return errors;
}

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

export function RegistryTargetsPage() {
  const queryClient = useQueryClient();
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    staleTime: 60_000,
  });
  const featureEnabled = capabilities.data?.features?.registry === true;
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const canRead = hasRegistryPlatformCapability(
    effectiveCapabilities,
    "registry-targets:read",
  );
  const canWrite = hasRegistryPlatformCapability(
    effectiveCapabilities,
    "registry-targets:write",
  );
  const targets = useQuery({
    queryKey: ["registry-targets", 100],
    queryFn: () => api.registryTargets(100),
    enabled: featureEnabled && canRead,
    retry: false,
  });
  const [editing, setEditing] = useState<RegistryTarget | undefined>();
  const [draft, setDraft] = useState<TargetDraft>(emptyTarget);
  const [errors, setErrors] = useState<
    Partial<Record<keyof TargetDraft, string>>
  >({});

  useEffect(() => {
    setDraft(targetDraft(editing));
    setErrors({});
  }, [editing]);

  const save = useMutation({
    mutationFn: ({
      targetId,
      input,
      idempotencyKey,
    }: {
      targetId?: string;
      input: RegistryTargetInput;
      idempotencyKey: string;
    }) =>
      targetId
        ? api.updateRegistryTarget(targetId, input, idempotencyKey)
        : api.createRegistryTarget(input, idempotencyKey),
    retry: retryNetworkOnce,
    onSuccess: async () => {
      setEditing(undefined);
      setDraft({ ...emptyTarget });
      await queryClient.invalidateQueries({ queryKey: ["registry-targets"] });
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const nextErrors = validateTarget(draft);
    setErrors(nextErrors);
    if (Object.keys(nextErrors).length > 0 || !canWrite) return;
    const input: RegistryTargetInput = {
      name: draft.name.trim(),
      mode: draft.mode,
      endpoint: draft.endpoint.trim(),
      repositoryPrefix: draft.repositoryPrefix.trim(),
      ...(draft.pullCredentialRef?.trim()
        ? { pullCredentialRef: draft.pullCredentialRef.trim() }
        : {}),
      ...(draft.pushCredentialRef?.trim()
        ? { pushCredentialRef: draft.pushCredentialRef.trim() }
        : {}),
      ...(draft.cacheCredentialRef?.trim()
        ? { cacheCredentialRef: draft.cacheCredentialRef.trim() }
        : {}),
    };
    save.mutate({
      targetId: editing?.id,
      input,
      idempotencyKey: crypto.randomUUID(),
    });
  };

  return (
    <div className="page">
      <PageHeader
        eyebrow="Artifact infrastructure"
        title="Registry targets"
        description="Configure managed or external OCI targets using credential references only. Registry secret material is never accepted or displayed here."
      />

      {capabilities.isPending ? (
        <Card>
          <Skeleton lines={5} />
        </Card>
      ) : !featureEnabled ? (
        <Card>
          <EmptyState
            icon="layers"
            title="Registry management is not enabled"
            description="The registry observer and executor must be production-wired before this feature is advertised."
          />
        </Card>
      ) : !canRead ? (
        <Card>
          <EmptyState
            icon="settings"
            title="Platform registry access required"
            description="An exact registry-targets:read capability at platform scope is required."
          />
        </Card>
      ) : (
        <div className="registry-layout">
          <Card>
            <div className="card__header card__header--inside">
              <div>
                <span className="eyebrow">Targets</span>
                <h2>Configured OCI endpoints</h2>
              </div>
              {targets.data?.truncated ? (
                <span className="placeholder-badge">First 100</span>
              ) : null}
            </div>
            {targets.isPending ? <Skeleton lines={7} /> : null}
            {targets.error ? (
              <ErrorPanel
                error={targets.error}
                onRetry={() => void targets.refetch()}
              />
            ) : null}
            {targets.data?.items.length === 0 ? (
              <EmptyState
                compact
                icon="layers"
                title="No registry targets"
                description="Add an approved OCI endpoint before attaching an application policy."
              />
            ) : (
              <div className="registry-target-list">
                {targets.data?.items.map((target) => (
                  <article className="registry-target-row" key={target.id}>
                    <div className="registry-target-row__heading">
                      <div>
                        <strong>{target.name}</strong>
                        <code>{target.endpoint}</code>
                      </div>
                      <StatusPill value={target.mode} />
                    </div>
                    <dl className="detail-list detail-list--compact">
                      <div>
                        <dt>Repository prefix</dt>
                        <dd>
                          <code>{target.repositoryPrefix}</code>
                        </dd>
                      </div>
                      <div>
                        <dt>Pull credential reference</dt>
                        <dd>{target.pullCredentialRef || "Not configured"}</dd>
                      </div>
                      <div>
                        <dt>Push credential reference</dt>
                        <dd>{target.pushCredentialRef || "Not configured"}</dd>
                      </div>
                      <div>
                        <dt>Cache credential reference</dt>
                        <dd>{target.cacheCredentialRef || "Not configured"}</dd>
                      </div>
                      <div>
                        <dt>Lifecycle owner</dt>
                        <dd>
                          {target.mode === "managed"
                            ? "Kuberploy managed"
                            : "Registry operator"}
                        </dd>
                      </div>
                      <div>
                        <dt>Updated</dt>
                        <dd>{formatDate(target.updatedAt)}</dd>
                      </div>
                    </dl>
                    {canWrite && me.data?.authentication.kind === "session" ? (
                      <Button
                        variant="secondary"
                        onClick={() => setEditing(target)}
                      >
                        <Icon name="settings" /> Edit metadata
                      </Button>
                    ) : null}
                  </article>
                ))}
              </div>
            )}
          </Card>

          {canWrite && me.data?.authentication.kind === "session" ? (
            <Card>
              <div className="card__header card__header--inside">
                <div>
                  <span className="eyebrow">
                    {editing ? "Update" : "Add target"}
                  </span>
                  <h2>{editing ? editing.name : "Registry metadata"}</h2>
                </div>
                {editing ? (
                  <Button variant="ghost" onClick={() => setEditing(undefined)}>
                    Cancel
                  </Button>
                ) : null}
              </div>
              <form className="form-grid" onSubmit={submit}>
                <Field label="Name" required error={errors.name}>
                  <input
                    value={draft.name}
                    onChange={(event) =>
                      setDraft((current) => ({
                        ...current,
                        name: event.target.value,
                      }))
                    }
                  />
                </Field>
                <Field
                  label="Mode"
                  required
                  error={errors.mode}
                  hint={
                    editing ? "Mode is immutable after creation." : undefined
                  }
                >
                  <select
                    value={draft.mode}
                    disabled={Boolean(editing)}
                    onChange={(event) =>
                      setDraft((current) => ({
                        ...current,
                        mode: event.target.value as RegistryTargetMode,
                      }))
                    }
                  >
                    <option value="external">External</option>
                    <option value="managed">Managed</option>
                  </select>
                </Field>
                <Field label="Endpoint" required error={errors.endpoint}>
                  <input
                    value={draft.endpoint}
                    placeholder="registry.example.com"
                    onChange={(event) =>
                      setDraft((current) => ({
                        ...current,
                        endpoint: event.target.value,
                      }))
                    }
                  />
                </Field>
                <Field
                  label="Repository prefix"
                  required
                  error={errors.repositoryPrefix}
                >
                  <input
                    value={draft.repositoryPrefix}
                    placeholder="organization/team"
                    onChange={(event) =>
                      setDraft((current) => ({
                        ...current,
                        repositoryPrefix: event.target.value,
                      }))
                    }
                  />
                </Field>
                {(
                  [
                    ["pullCredentialRef", "Pull credential reference"],
                    ["pushCredentialRef", "Push credential reference"],
                    ["cacheCredentialRef", "Cache credential reference"],
                  ] as const
                ).map(([key, label]) => (
                  <Field
                    key={key}
                    label={label}
                    hint="Reference name only; never enter a username, token, or password."
                  >
                    <input
                      value={draft[key] ?? ""}
                      autoComplete="off"
                      onChange={(event) =>
                        setDraft((current) => ({
                          ...current,
                          [key]: event.target.value,
                        }))
                      }
                    />
                  </Field>
                ))}
                {save.error ? <ErrorPanel error={save.error} /> : null}
                <div className="form-actions">
                  <Button type="submit" busy={save.isPending}>
                    <Icon name="check" />{" "}
                    {editing ? "Save target" : "Add target"}
                  </Button>
                </div>
              </form>
            </Card>
          ) : (
            <Card>
              <EmptyState
                compact
                icon="settings"
                title="Registry target metadata is read-only"
                description="A human session with registry-targets:write at platform scope is required to change targets."
              />
            </Card>
          )}
        </div>
      )}
    </div>
  );
}
