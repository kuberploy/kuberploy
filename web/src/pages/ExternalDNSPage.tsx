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
    errors.txtOwnerId = "Use a safe immutable TXT owner identifier.";
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
  });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
    enabled: featureEnabled && canRead,
    retry: false,
  });
  const [editing, setEditing] = useState<ExternalDNSIntegration>();
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
    <div className="page">
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
            <div className="card__header card__header--inside">
              <div>
                <span className="eyebrow">Readiness boundary</span>
                <h2>Controller readiness</h2>
              </div>
              <StatusPill
                value={status.data?.controllerReadiness ?? "unobserved"}
              />
            </div>
            {status.isPending ? <Skeleton lines={3} /> : null}
            {status.error ? (
              <ErrorPanel
                error={status.error}
                onRetry={() => void status.refetch()}
              />
            ) : null}
            {status.data ? (
              <div
                className={
                  status.data.runtimeAvailable
                    ? "notice notice--success"
                    : "notice notice--warning"
                }
              >
                <div>
                  <strong>
                    {status.data.configurationState === "configured"
                      ? "Integration metadata is configured"
                      : "No integration metadata is configured"}
                  </strong>
                  <p>{status.data.detail}</p>
                </div>
                <span className="placeholder-badge">
                  {status.data.runtimeAvailable
                    ? "Runtime available"
                    : "Runtime unavailable"}
                </span>
              </div>
            ) : null}
          </Card>

          <div className="registry-layout">
            <Card>
              <div className="card__header card__header--inside">
                <div>
                  <span className="eyebrow">Profiles</span>
                  <h2>Authorized integration catalog</h2>
                </div>
                {integrations.data?.truncated ? (
                  <span className="placeholder-badge">First 100</span>
                ) : null}
              </div>
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
                <div className="registry-target-list">
                  {integrations.data?.items.map((integration) => (
                    <article
                      className="registry-target-row"
                      key={integration.id}
                    >
                      <div className="registry-target-row__heading">
                        <div>
                          <strong>{integration.name}</strong>
                          <code>{integration.slug}</code>
                        </div>
                        <StatusPill value={integration.mode} />
                      </div>
                      <dl className="detail-list detail-list--compact">
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
                      </dl>
                      {canWrite &&
                      me.data?.authentication.kind === "session" ? (
                        <div className="form-actions">
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
                            onClick={() => {
                              if (
                                window.confirm(
                                  `Deactivate ${integration.name}? Its exact protected Git bundle will be removed.`,
                                )
                              )
                                deactivateIntegration(integration.id);
                            }}
                          >
                            <Icon name="close" /> Deactivate
                          </Button>
                        </div>
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
                      {editing ? "Update" : "Add profile"}
                    </span>
                    <h2>{editing?.name ?? "Integration metadata"}</h2>
                  </div>
                  {editing ? (
                    <Button variant="ghost" onClick={closeEditor}>
                      Cancel
                    </Button>
                  ) : null}
                </div>
                <form className="form-grid" onSubmit={submit}>
                  {editing && !editingIsCurrent ? (
                    <div className="notice notice--warning">
                      This integration changed, was deactivated, or is no longer
                      available. Reload the catalog before saving it.
                    </div>
                  ) : null}
                  <Field label="Immutable slug" required error={errors.slug}>
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
                    <select
                      value={draft.mode}
                      onChange={(event) =>
                        updateMode(
                          event.target.value as ExternalDNSIntegrationMode,
                        )
                      }
                    >
                      <option value="adopted">Operator-adopted</option>
                      <option value="managed">Kuberploy-managed</option>
                    </select>
                  </Field>
                  <Field label="Provider" required>
                    <select
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
                    </select>
                  </Field>
                  <Field
                    label="Immutable TXT owner"
                    required
                    error={errors.txtOwnerId}
                  >
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
                    <select
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
                    </select>
                  </Field>
                  {draft.syncPolicy === "sync" ? (
                    <Field
                      label="Destructive sync confirmation"
                      required
                      error={errors.destructiveSyncConfirmed}
                    >
                      <label className="switch">
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
                    <div className="checkbox-list">
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
                  <div className="form-actions">
                    <Button
                      type="submit"
                      busy={save.isPending}
                      disabled={Boolean(editing && !editingIsCurrent)}
                    >
                      <Icon name="check" />{" "}
                      {editing ? "Save profile" : "Add profile"}
                    </Button>
                  </div>
                </form>
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
    </div>
  );
}
