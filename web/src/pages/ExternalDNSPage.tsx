import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type {
  ExternalDNSIntegration,
  ExternalDNSIntegrationInput,
  ExternalDNSIntegrationMode,
  ExternalDNSProviderKind,
  ExternalDNSSyncPolicy,
} from "../api/types";
import { Icon } from "../components/Icon";
import {
  Select,
  Button,
  Card,
  CardHeader,
  ConfirmDialog,
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
import { hasExternalDNSPlatformCapability } from "../lib/externalDNSAccess";

type IntegrationDraft = {
  slug: string;
  name: string;
  mode: ExternalDNSIntegrationMode;
  providerKind: ExternalDNSProviderKind;
  txtOwnerId: string;
  suffixes: string;
  syncPolicy: ExternalDNSSyncPolicy;
  destructiveSyncConfirmed: boolean;
  credentialSecretRef: string;
  providerConfigRef: string;
  egressConfigRef: string;
  operatorProfileRef: string;
  environmentIds: string[];
};

function emptyDraft(): IntegrationDraft {
  return {
    slug: "",
    name: "",
    mode: "adopted",
    providerKind: "cloudflare",
    txtOwnerId: "",
    suffixes: "",
    syncPolicy: "upsert-only",
    destructiveSyncConfirmed: false,
    credentialSecretRef: "",
    providerConfigRef: "",
    egressConfigRef: "",
    operatorProfileRef: "",
    environmentIds: [],
  };
}

function integrationDraft(
  integration?: ExternalDNSIntegration,
): IntegrationDraft {
  if (!integration) return emptyDraft();
  return {
    slug: integration.slug,
    name: integration.name,
    mode: integration.mode,
    providerKind: integration.providerKind,
    txtOwnerId: integration.txtOwnerId,
    suffixes: integration.allowedDomainSuffixes.join("\n"),
    syncPolicy: integration.syncPolicy,
    destructiveSyncConfirmed: integration.destructiveSyncConfirmed,
    credentialSecretRef: integration.credentialSecretRef ?? "",
    providerConfigRef: integration.providerConfigRef ?? "",
    egressConfigRef: integration.egressConfigRef ?? "",
    operatorProfileRef: integration.operatorProfileRef ?? "",
    environmentIds: [...integration.environmentIds],
  };
}

const slugPattern = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/;
const ownerPattern = /^[a-z0-9](?:[-a-z0-9._]{0,126}[a-z0-9])?$/;
const refPattern = /^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$/;
const domainPattern =
  /^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

function suffixList(value: string) {
  return value
    .split(/\r?\n/)
    .map((suffix) => suffix.trim().toLowerCase().replace(/\.$/, ""))
    .filter(Boolean);
}

function validateDraft(draft: IntegrationDraft) {
  const errors: Partial<Record<keyof IntegrationDraft, string>> = {};
  const suffixes = suffixList(draft.suffixes);
  if (!slugPattern.test(draft.slug))
    errors.slug = "Use a lowercase Kubernetes-safe slug (1–63 characters).";
  if (
    !draft.name.trim() ||
    draft.name.trim().length > 100 ||
    /[\u0000-\u001f\u007f]/.test(draft.name)
  )
    errors.name = "Enter a display name of 1–100 characters.";
  if (!ownerPattern.test(draft.txtOwnerId))
    errors.txtOwnerId = "Use a safe stable TXT owner identifier.";
  if (
    suffixes.length === 0 ||
    suffixes.length > 64 ||
    new Set(suffixes).size !== suffixes.length ||
    suffixes.some(
      (suffix) => suffix.length > 253 || !domainPattern.test(suffix),
    )
  ) {
    errors.suffixes = "Enter 1–64 unique lowercase DNS suffixes, one per line.";
  }
  if (draft.environmentIds.length === 0)
    errors.environmentIds = "Select at least one exact environment.";
  if (
    (draft.syncPolicy === "sync" && !draft.destructiveSyncConfirmed) ||
    (draft.syncPolicy === "upsert-only" && draft.destructiveSyncConfirmed)
  ) {
    errors.destructiveSyncConfirmed =
      "Destructive sync requires explicit confirmation; upsert-only must leave it off.";
  }
  if (draft.mode === "managed") {
    for (const key of [
      "credentialSecretRef",
      "providerConfigRef",
      "egressConfigRef",
    ] as const) {
      if (!refPattern.test(draft[key]))
        errors[key] = "Enter a Kubernetes-safe opaque reference name.";
    }
  } else if (!refPattern.test(draft.operatorProfileRef)) {
    errors.operatorProfileRef =
      "Enter the Kubernetes-safe operator profile reference.";
  }
  return errors;
}

function inputFromDraft(draft: IntegrationDraft): ExternalDNSIntegrationInput {
  const base: ExternalDNSIntegrationInput = {
    slug: draft.slug.trim(),
    name: draft.name.trim(),
    mode: draft.mode,
    providerKind: draft.providerKind,
    txtOwnerId: draft.txtOwnerId.trim(),
    allowedDomainSuffixes: suffixList(draft.suffixes),
    syncPolicy: draft.syncPolicy,
    destructiveSyncConfirmed: draft.destructiveSyncConfirmed,
    environmentIds: [...draft.environmentIds].sort(),
  };
  return draft.mode === "managed"
    ? {
        ...base,
        credentialSecretRef: draft.credentialSecretRef.trim(),
        providerConfigRef: draft.providerConfigRef.trim(),
        egressConfigRef: draft.egressConfigRef.trim(),
      }
    : { ...base, operatorProfileRef: draft.operatorProfileRef.trim() };
}

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

export function ExternalDNSPage() {
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
  const featureEnabled =
    capabilities.data?.features?.externalDNSConfiguration === true;
  const effectiveCapabilities = capabilities.data?.capabilities ?? [];
  const canRead = hasExternalDNSPlatformCapability(
    effectiveCapabilities,
    "external-dns-integrations:read",
  );
  const canWrite = hasExternalDNSPlatformCapability(
    effectiveCapabilities,
    "external-dns-integrations:write",
  );
  const integrations = useQuery({
    queryKey: ["external-dns-integrations", 100],
    queryFn: () => api.externalDNSIntegrations(100),
    enabled: featureEnabled && canRead,
    retry: false,
  });
  const status = useQuery({
    queryKey: ["external-dns-status"],
    queryFn: api.externalDNSStatus,
    enabled: featureEnabled && canRead,
    retry: false,
    refetchInterval: (query) =>
      query.state.data?.configurationState === "configured" &&
      query.state.data.runtimeAvailable !== true
        ? 2_000
        : false,
  });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    enabled: featureEnabled && canRead,
    retry: false,
  });
  const [editing, setEditing] = useState<ExternalDNSIntegration>();
  const [deactivationCandidate, setDeactivationCandidate] =
    useState<ExternalDNSIntegration>();
  const [draft, setDraft] = useState<IntegrationDraft>(emptyDraft);
  const [errors, setErrors] = useState<
    Partial<Record<keyof IntegrationDraft, string>>
  >({});
  const saveAttempt = useRef<{ signature: string; key: string } | null>(null);
  const deactivateAttempt = useRef<{
    signature: string;
    key: string;
  } | null>(null);
  const editorSessionRef = useRef(0);
  const editorScope = JSON.stringify({
    integrationId: editing?.id ?? null,
    draft,
  });
  const editorScopeRef = useRef(editorScope);
  const [saveError, setSaveError] = useState<unknown>(null);
  editorScopeRef.current = editorScope;

  const currentEditingIntegration = editing
    ? integrations.data?.items.find(
        (integration) => integration.id === editing.id,
      )
    : undefined;
  const editingIsCurrent =
    !editing ||
    Boolean(
      currentEditingIntegration &&
      currentEditingIntegration.lifecycle !== "deactivated" &&
      JSON.stringify(currentEditingIntegration) === JSON.stringify(editing),
    );

  useEffect(() => {
    setDraft(integrationDraft(editing));
    setErrors({});
  }, [editing]);

  const save = useMutation({
    mutationFn: ({
      integrationId,
      input,
      idempotencyKey,
    }: {
      integrationId?: string;
      input: ExternalDNSIntegrationInput;
      idempotencyKey: string;
      editorScope: string;
      editorSession: number;
    }) =>
      integrationId
        ? api.updateExternalDNSIntegration(integrationId, input, idempotencyKey)
        : api.createExternalDNSIntegration(input, idempotencyKey),
    retry: retryNetworkOnce,
    onMutate: () => setSaveError(null),
    onSuccess: async (_value, input) => {
      const isCurrentEditor =
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current;
      if (!isCurrentEditor) {
        await Promise.all([
          queryClient.invalidateQueries({
            queryKey: ["external-dns-integrations"],
          }),
          queryClient.invalidateQueries({ queryKey: ["external-dns-status"] }),
        ]);
        return;
      }
      if (saveAttempt.current?.key === input.idempotencyKey)
        saveAttempt.current = null;
      editorSessionRef.current += 1;
      setEditing(undefined);
      setDraft(emptyDraft());
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ["external-dns-integrations"],
        }),
        queryClient.invalidateQueries({ queryKey: ["external-dns-status"] }),
      ]);
    },
    onError: (error, input) => {
      if (
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current
      )
        setSaveError(error);
    },
  });

  const deactivate = useMutation({
    mutationFn: ({
      integrationId,
      idempotencyKey,
      editorSession,
    }: {
      integrationId: string;
      idempotencyKey: string;
      editorSession: number;
    }) => api.deactivateExternalDNSIntegration(integrationId, idempotencyKey),
    retry: retryNetworkOnce,
    onSuccess: async (_value, input) => {
      const isCurrentAttempt =
        deactivateAttempt.current?.key === input.idempotencyKey;
      if (isCurrentAttempt) {
        deactivateAttempt.current = null;
        if (editorSessionRef.current === input.editorSession) {
          editorSessionRef.current += 1;
          setEditing(undefined);
        }
      }
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ["external-dns-integrations"],
        }),
        queryClient.invalidateQueries({ queryKey: ["external-dns-status"] }),
      ]);
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (editing && !editingIsCurrent) return;
    const nextErrors = validateDraft(draft);
    setErrors(nextErrors);
    if (
      Object.keys(nextErrors).length > 0 ||
      !canWrite ||
      me.data?.authentication.kind !== "session"
    )
      return;
    const input = inputFromDraft(draft);
    const signature = JSON.stringify({ integrationId: editing?.id, input });
    const idempotencyKey =
      saveAttempt.current?.signature === signature
        ? saveAttempt.current.key
        : crypto.randomUUID();
    saveAttempt.current = { signature, key: idempotencyKey };
    save.mutate({
      integrationId: editing?.id,
      input,
      idempotencyKey,
      editorScope,
      editorSession: editorSessionRef.current,
    });
  };

  const deactivateIntegration = (integrationId: string) => {
    const signature = JSON.stringify({ integrationId });
    const idempotencyKey =
      deactivateAttempt.current?.signature === signature
        ? deactivateAttempt.current.key
        : crypto.randomUUID();
    deactivateAttempt.current = { signature, key: idempotencyKey };
    deactivate.mutate({
      integrationId,
      idempotencyKey,
      editorSession: editorSessionRef.current,
    });
  };

  const openEditor = (integration: ExternalDNSIntegration) => {
    editorSessionRef.current += 1;
    setEditing(integration);
  };

  const closeEditor = () => {
    editorSessionRef.current += 1;
    setEditing(undefined);
  };

  const updateMode = (mode: ExternalDNSIntegrationMode) =>
    setDraft((current) => ({
      ...current,
      mode,
      credentialSecretRef: "",
      providerConfigRef: "",
      egressConfigRef: "",
      operatorProfileRef: "",
    }));

  return (
    <Page>
      <PageHeader
        eyebrow="Platform networking"
        title="External DNS integrations"
        description="Configure protected managed or operator-adopted integrations. Managed runtime changes are published through the platform Git authority; credential values and provider API endpoints are never accepted here."
      />

      {capabilities.isPending ? (
        <Card>
          <Skeleton lines={5} />
        </Card>
      ) : capabilities.error ? (
        <Card>
          <ErrorPanel
            error={capabilities.error}
            onRetry={() => void capabilities.refetch()}
            title="Could not verify External DNS access"
          />
        </Card>
      ) : !featureEnabled ? (
        <Card>
          <EmptyState
            icon="route"
            title="External DNS configuration is not enabled"
            description="The metadata service must be configured before integration profiles can be managed. Runtime availability remains separate."
          />
        </Card>
      ) : !canRead ? (
        <Card>
          <EmptyState
            icon="settings"
            title="Platform External DNS access required"
            description="An exact external-dns-integrations:read capability at platform scope is required. Broad action lists do not grant access."
          />
        </Card>
      ) : (
        <>
          <Card>
            <CardHeader>
              <div>
                <Eyebrow>Readiness boundary</Eyebrow>
                <h2>Controller readiness</h2>
              </div>
              <StatusPill
                value={status.data?.controllerReadiness ?? "unobserved"}
              />
            </CardHeader>
            {status.isPending ? <Skeleton lines={3} /> : null}
            {status.error ? (
              <ErrorPanel
                error={status.error}
                onRetry={() => void status.refetch()}
              />
            ) : null}
            {status.data ? (
              <Notice
                tone={status.data.runtimeAvailable ? "success" : "warning"}
              >
                <div>
                  <strong>
                    {status.data.configurationState === "configured"
                      ? "Integration metadata is configured"
                      : "No integration metadata is configured"}
                  </strong>
                  <p>{status.data.detail}</p>
                </div>
                <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
                  {status.data.runtimeAvailable
                    ? "Runtime available"
                    : "Runtime unavailable"}
                </span>
              </Notice>
            ) : null}
          </Card>

          <div className="grid grid-cols-[minmax(0,_1.4fr)_minmax(340px,_0.8fr)] gap-5 items-start to-900:grid-cols-[1fr]">
            <Card>
              <CardHeader>
                <div>
                  <Eyebrow>Profiles</Eyebrow>
                  <h2>Authorized integration catalog</h2>
                </div>
                {integrations.data?.truncated ? (
                  <span className="inline-flex w-max min-h-[22px] items-center py-0 px-2 border border-line rounded-md text-ink-soft bg-surface-soft text-xs font-semibold whitespace-nowrap">
                    First 100
                  </span>
                ) : null}
              </CardHeader>
              {deactivate.error ? (
                <ErrorPanel
                  error={deactivate.error}
                  title="Integration was not deactivated"
                />
              ) : null}
              {integrations.isPending ? <Skeleton lines={7} /> : null}
              {integrations.error ? (
                <ErrorPanel
                  error={integrations.error}
                  onRetry={() => void integrations.refetch()}
                />
              ) : null}
              {integrations.data?.items.length === 0 ? (
                <EmptyState
                  compact
                  icon="route"
                  title="No External DNS integrations"
                  description="Add a profile and assign at least one exact environment. This does not advertise controller readiness."
                />
              ) : (
                <div className="grid gap-4">
                  {integrations.data?.items.map((integration) => (
                    <article
                      className="grid justify-items-start gap-4 p-5 border border-line rounded-panel bg-surface [&>[data-slot='detail-list']]:justify-self-stretch [&>[data-slot='detail-list']]:w-full"
                      key={integration.id}
                    >
                      <div className="flex items-start justify-between gap-5 [&>div]:grid [&>div]:gap-1.5 [&>div]:min-w-0 [&_code]:overflow-hidden [&_code]:text-ellipsis to-580:items-start to-580:flex-col">
                        <div>
                          <strong>{integration.name}</strong>
                          <code>{integration.slug}</code>
                        </div>
                        <StatusPill value={integration.mode} />
                      </div>
                      <DetailList className="[&>div]:py-2">
                        <div>
                          <dt>Provider</dt>
                          <dd>{integration.providerKind}</dd>
                        </div>
                        <div>
                          <dt>TXT owner</dt>
                          <dd>
                            <code>{integration.txtOwnerId}</code>
                          </dd>
                        </div>
                        <div>
                          <dt>Allowed suffixes</dt>
                          <dd>
                            {integration.allowedDomainSuffixes.join(", ")}
                          </dd>
                        </div>
                        <div>
                          <dt>Sync policy</dt>
                          <dd>{integration.syncPolicy}</dd>
                        </div>
                        <div>
                          <dt>Environments</dt>
                          <dd>{integration.environmentIds.length}</dd>
                        </div>
                        <div>
                          <dt>Profile reference</dt>
                          <dd>
                            {integration.mode === "managed"
                              ? integration.providerConfigRef
                              : integration.operatorProfileRef}
                          </dd>
                        </div>
                        <div>
                          <dt>Updated</dt>
                          <dd>{formatDate(integration.updatedAt)}</dd>
                        </div>
                        <div>
                          <dt>Runtime revision</dt>
                          <dd>{integration.runtimeRevision}</dd>
                        </div>
                        <div>
                          <dt>Protected Git</dt>
                          <dd>
                            {integration.protectedGitState ?? "pending"}
                            {integration.protectedGitRevision
                              ? ` · revision ${integration.protectedGitRevision}`
                              : ""}
                          </dd>
                        </div>
                      </DetailList>
                      {canWrite &&
                      me.data?.authentication.kind === "session" ? (
                        <FormActions>
                          <Button
                            variant="secondary"
                            disabled={integration.lifecycle === "deactivated"}
                            onClick={() => openEditor(integration)}
                          >
                            <Icon name="settings" /> Edit profile
                          </Button>
                          <Button
                            variant="secondary"
                            disabled={
                              integration.lifecycle === "deactivated" ||
                              deactivate.isPending
                            }
                            busy={deactivate.isPending}
                            onClick={() =>
                              setDeactivationCandidate(integration)
                            }
                          >
                            <Icon name="close" /> Deactivate
                          </Button>
                        </FormActions>
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
                    <Eyebrow>{editing ? "Update" : "Add profile"}</Eyebrow>
                    <h2>{editing?.name ?? "Integration metadata"}</h2>
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
                      This integration changed, was deactivated, or is no longer
                      available. Reload the catalog before saving it.
                    </Notice>
                  ) : null}
                  <Field label="DNS slug" required error={errors.slug}>
                    <input
                      value={draft.slug}
                      disabled={Boolean(editing)}
                      placeholder="cloudflare-primary"
                      onChange={(event) =>
                        setDraft((current) => ({
                          ...current,
                          slug: event.target.value,
                        }))
                      }
                    />
                  </Field>
                  <Field label="Display name" required error={errors.name}>
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
                  <Field label="Mode" required>
                    <Select
                      value={draft.mode}
                      onChange={(event) =>
                        updateMode(
                          event.target.value as ExternalDNSIntegrationMode,
                        )
                      }
                    >
                      <option value="adopted">Operator-adopted</option>
                      <option value="managed">Kuberploy-managed</option>
                    </Select>
                  </Field>
                  <Field label="Provider" required>
                    <Select
                      value={draft.providerKind}
                      onChange={(event) =>
                        setDraft((current) => ({
                          ...current,
                          providerKind: event.target
                            .value as ExternalDNSProviderKind,
                        }))
                      }
                    >
                      {(
                        [
                          "aws",
                          "azure",
                          "cloudflare",
                          "google",
                          "rfc2136",
                        ] as const
                      ).map((provider) => (
                        <option value={provider} key={provider}>
                          {provider}
                        </option>
                      ))}
                    </Select>
                  </Field>
                  <Field label="TXT owner" required error={errors.txtOwnerId}>
                    <input
                      value={draft.txtOwnerId}
                      disabled={Boolean(editing)}
                      placeholder="kuberploy.production"
                      onChange={(event) =>
                        setDraft((current) => ({
                          ...current,
                          txtOwnerId: event.target.value,
                        }))
                      }
                    />
                  </Field>
                  <Field
                    label="Allowed domain suffixes"
                    required
                    error={errors.suffixes}
                    hint="One lowercase suffix per line; every route host must be inside one suffix."
                  >
                    <textarea
                      rows={4}
                      value={draft.suffixes}
                      placeholder={"example.com\napps.example.net"}
                      onChange={(event) =>
                        setDraft((current) => ({
                          ...current,
                          suffixes: event.target.value,
                        }))
                      }
                    />
                  </Field>
                  <Field label="Sync policy" required>
                    <Select
                      value={draft.syncPolicy}
                      onChange={(event) =>
                        setDraft((current) => ({
                          ...current,
                          syncPolicy: event.target
                            .value as ExternalDNSSyncPolicy,
                          destructiveSyncConfirmed: false,
                        }))
                      }
                    >
                      <option value="upsert-only">
                        Upsert only (safe default)
                      </option>
                      <option value="sync">Destructive sync</option>
                    </Select>
                  </Field>
                  {draft.syncPolicy === "sync" ? (
                    <Field
                      label="Destructive sync confirmation"
                      required
                      error={errors.destructiveSyncConfirmed}
                    >
                      <label className="flex min-h-[39px] items-center gap-2 text-meta cursor-pointer [&_input]:absolute [&_input]:w-px [&_input]:h-px [&_input]:opacity-0 [&_span]:relative [&_span]:w-8 [&_span]:h-[18px] [&_span]:rounded-full [&_span]:bg-line-strong [&_span]:transition [&_span]:duration-(--motion-fast) [&_span]:ease-(--ease-standard) [&_span::after]:absolute [&_span::after]:top-[3px] [&_span::after]:left-[3px] [&_span::after]:w-3 [&_span::after]:h-3 [&_span::after]:content-[''] [&_span::after]:rounded-full [&_span::after]:bg-surface [&_span::after]:shadow-[0_1px_3px_rgba(0_0_0_0.2)] [&_span::after]:transition [&_span::after]:duration-(--motion-fast) [&_span::after]:ease-(--ease-standard) [&_input:checked_+_span]:bg-mint [&_input:checked_+_span::after]:transform-[translateX(14px)]">
                        <input
                          type="checkbox"
                          checked={draft.destructiveSyncConfirmed}
                          onChange={(event) =>
                            setDraft((current) => ({
                              ...current,
                              destructiveSyncConfirmed: event.target.checked,
                            }))
                          }
                        />
                        <span /> I explicitly allow record deletion within these
                        suffixes
                      </label>
                    </Field>
                  ) : null}
                  {draft.mode === "managed" ? (
                    <>
                      {(
                        [
                          [
                            "credentialSecretRef",
                            "Credential Secret reference",
                          ],
                          ["providerConfigRef", "Provider config reference"],
                          ["egressConfigRef", "Egress config reference"],
                        ] as const
                      ).map(([key, label]) => (
                        <Field
                          key={key}
                          label={label}
                          required
                          error={errors[key]}
                          hint="Opaque Kubernetes reference only; never enter a secret or URL."
                        >
                          <input
                            value={draft[key]}
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
                    </>
                  ) : (
                    <Field
                      label="Operator profile reference"
                      required
                      error={errors.operatorProfileRef}
                      hint="References an operator-owned profile; tenant endpoints and provider JSON are not accepted."
                    >
                      <input
                        value={draft.operatorProfileRef}
                        autoComplete="off"
                        onChange={(event) =>
                          setDraft((current) => ({
                            ...current,
                            operatorProfileRef: event.target.value,
                          }))
                        }
                      />
                    </Field>
                  )}
                  <Field
                    label="Exact environments"
                    required
                    error={errors.environmentIds}
                  >
                    <div className="grid gap-[0.55rem] max-h-[16rem] overflow-auto p-[0.75rem] border border-line rounded-[9px] bg-surface-soft [&_label]:grid [&_label]:grid-cols-[auto_minmax(0,_1fr)_auto] [&_label]:items-center [&_label]:gap-[0.65rem] [&_code]:text-[0.75rem]">
                      {environments.isPending ? (
                        <span>Loading environments…</span>
                      ) : null}
                      {environments.data?.items.map((environment) => (
                        <label key={environment.id}>
                          <input
                            type="checkbox"
                            checked={draft.environmentIds.includes(
                              environment.id,
                            )}
                            onChange={(event) =>
                              setDraft((current) => ({
                                ...current,
                                environmentIds: event.target.checked
                                  ? [...current.environmentIds, environment.id]
                                  : current.environmentIds.filter(
                                      (id) => id !== environment.id,
                                    ),
                              }))
                            }
                          />
                          <span>{environment.name}</span>
                          <code>{environment.namespace}</code>
                        </label>
                      ))}
                    </div>
                  </Field>
                  {environments.error ? (
                    <ErrorPanel error={environments.error} />
                  ) : null}
                  {saveError ? <ErrorPanel error={saveError} /> : null}
                  <FormActions>
                    <Button
                      type="submit"
                      busy={save.isPending}
                      disabled={Boolean(editing && !editingIsCurrent)}
                    >
                      <Icon name="check" />{" "}
                      {editing ? "Save profile" : "Add profile"}
                    </Button>
                  </FormActions>
                </FormGrid>
              </Card>
            ) : (
              <Card>
                <EmptyState
                  compact
                  icon="settings"
                  title="External DNS metadata is read-only"
                  description="A human session with external-dns-integrations:write at exact platform scope is required to change profiles."
                />
              </Card>
            )}
          </div>
        </>
      )}
      {deactivationCandidate ? (
        <ConfirmDialog
          title={`Deactivate ${deactivationCandidate.name}?`}
          description="Its exact protected Git bundle will be removed."
          confirmLabel="Deactivate integration"
          icon="close"
          busy={deactivate.isPending}
          onCancel={() => setDeactivationCandidate(undefined)}
          onConfirm={() => {
            const integration = deactivationCandidate;
            setDeactivationCandidate(undefined);
            deactivateIntegration(integration.id);
          }}
        />
      ) : null}
    </Page>
  );
}
