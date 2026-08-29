import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type {
  RegistryTarget,
  RegistryTargetInput,
  RegistryTargetMode,
} from "../api/types";
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
  FormActions,
  FormGrid,
  Notice,
  Page,
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
  const saveAttempt = useRef<{ signature: string; key: string } | null>(null);
  const editorSessionRef = useRef(0);
  const editorScope = JSON.stringify({ targetId: editing?.id ?? null, draft });
  const editorScopeRef = useRef(editorScope);
  const [saveError, setSaveError] = useState<unknown>(null);
  editorScopeRef.current = editorScope;

  const currentEditingTarget = editing
    ? targets.data?.items.find((target) => target.id === editing.id)
    : undefined;
  const editingIsCurrent =
    !editing ||
    Boolean(
      currentEditingTarget &&
      JSON.stringify(currentEditingTarget) === JSON.stringify(editing),
    );

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
      editorScope: string;
      editorSession: number;
    }) =>
      targetId
        ? api.updateRegistryTarget(targetId, input, idempotencyKey)
        : api.createRegistryTarget(input, idempotencyKey),
    retry: retryNetworkOnce,
    onMutate: () => setSaveError(null),
    onSuccess: async (_value, input) => {
      const isCurrentEditor =
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current;
      if (
        isCurrentEditor &&
        saveAttempt.current?.key === input.idempotencyKey
      ) {
        saveAttempt.current = null;
      }
      if (isCurrentEditor) {
        editorSessionRef.current += 1;
        setEditing(undefined);
        setDraft({ ...emptyTarget });
      }
      await queryClient.invalidateQueries({ queryKey: ["registry-targets"] });
    },
    onError: (error, input) => {
      if (
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current
      )
        setSaveError(error);
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (editing && !editingIsCurrent) return;
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
    const signature = JSON.stringify({ targetId: editing?.id, input });
    const idempotencyKey =
      saveAttempt.current?.signature === signature
        ? saveAttempt.current.key
        : crypto.randomUUID();
    saveAttempt.current = { signature, key: idempotencyKey };
    save.mutate({
      targetId: editing?.id,
      input,
      idempotencyKey,
      editorScope,
      editorSession: editorSessionRef.current,
    });
  };

  const openEditor = (target: RegistryTarget) => {
    editorSessionRef.current += 1;
    setEditing(target);
  };

  const closeEditor = () => {
    editorSessionRef.current += 1;
    setEditing(undefined);
  };

  return (
    <Page>
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
        <div className="grid grid-cols-[minmax(0,_1.4fr)_minmax(340px,_0.8fr)] gap-5 items-start to-900:grid-cols-[1fr]">
          <Card>
            <CardHeader>
              <div>
                <Eyebrow>Targets</Eyebrow>
                <h2>Configured OCI endpoints</h2>
              </div>
              {targets.data?.truncated ? (
                <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
                  First 100
                </span>
              ) : null}
            </CardHeader>
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
              <div className="grid gap-4">
                {targets.data?.items.map((target) => (
                  <article
                    className="grid justify-items-start gap-4 p-5 border border-line rounded-panel bg-surface [&>[data-slot='detail-list']]:justify-self-stretch [&>[data-slot='detail-list']]:w-full"
                    key={target.id}
                  >
                    <div className="flex items-start justify-between gap-5 [&>div]:grid [&>div]:gap-1.5 [&>div]:min-w-0 [&_code]:overflow-hidden [&_code]:text-ellipsis to-580:items-start to-580:flex-col">
                      <div>
                        <strong>{target.name}</strong>
                        <code>{target.endpoint}</code>
                      </div>
                      <StatusPill value={target.mode} />
                    </div>
                    <DetailList className="[&>div]:py-2">
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
                    </DetailList>
                    {canWrite && me.data?.authentication.kind === "session" ? (
                      <Button
                        variant="secondary"
                        onClick={() => openEditor(target)}
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
              <CardHeader>
                <div>
                  <Eyebrow>{editing ? "Update" : "Add target"}</Eyebrow>
                  <h2>{editing ? editing.name : "Registry metadata"}</h2>
                </div>
                {editing ? (
                  <Button variant="ghost" onClick={closeEditor}>
                    Cancel
                  </Button>
                ) : null}
              </CardHeader>
              <FormGrid as="form" onSubmit={submit}>
                {editing && !editingIsCurrent ? (
                  <Notice tone="warning">
                    This registry target changed or is no longer available.
                    Reload the catalog before saving it.
                  </Notice>
                ) : null}
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
                    editing
                      ? "Mode cannot be changed after creation."
                      : undefined
                  }
                >
                  <Select
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
                  </Select>
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
                {saveError ? <ErrorPanel error={saveError} /> : null}
                <FormActions>
                  <Button
                    type="submit"
                    busy={save.isPending}
                    disabled={Boolean(editing && !editingIsCurrent)}
                  >
                    <Icon name="check" />{" "}
                    {editing ? "Save target" : "Add target"}
                  </Button>
                </FormActions>
              </FormGrid>
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
    </Page>
  );
}
