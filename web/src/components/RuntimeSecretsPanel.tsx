import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api } from "../api/client";
import type {
  Application,
  Capability,
  Environment,
  Project,
  RuntimeSecretBindingDetail,
  RuntimeSecretDelivery,
  RuntimeSecretProvider,
  RuntimeSecretWriteValues,
} from "../api/types";
import { formatDate, titleCase } from "../lib/format";
import {
  hasRuntimeSecretCapability,
  runtimeSecretEnvironments,
} from "../lib/runtimeSecretAccess";
import { Icon } from "./Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  PlaceholderBadge,
  Skeleton,
  StatusPill,
} from "./ui";

const secretKeyPattern = /^[A-Za-z0-9._-]{1,253}$/;
const environmentNamePattern = /^[A-Za-z_][A-Za-z0-9_]{0,252}$/;
const bindingNamePattern = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/;
const filePathPattern =
  /^\/var\/run\/secrets\/kuberploy\/(?:[A-Za-z0-9._-]+\/)*[A-Za-z0-9._-]+$/;

type WritePayload = {
  values: RuntimeSecretWriteValues;
  deliveries: RuntimeSecretDelivery[];
};

type WriteCollection =
  { ok: true; payload: WritePayload } | { ok: false; error: string };

type DeliveryRow = { id: number; kind: "environment" | "file" };

function fieldValue(row: Element, selector: string) {
  return row.querySelector<HTMLInputElement | HTMLSelectElement>(selector)
    ?.value;
}

function destroyWritePayload(payload?: WritePayload) {
  if (!payload) return;
  for (const key of Object.keys(payload.values)) {
    payload.values[key] = "";
    delete payload.values[key];
  }
  payload.deliveries.splice(0, payload.deliveries.length);
}

function clearWriteOnlyForm(form: HTMLFormElement) {
  form
    .querySelectorAll<HTMLInputElement>(
      "[data-secret-material-key], [data-secret-material-value], [data-secret-delivery-source]",
    )
    .forEach((input) => {
      input.value = "";
    });
  form.reset();
}

function invalidWrite(error: string, payload?: WritePayload): WriteCollection {
  destroyWritePayload(payload);
  return { ok: false, error };
}

function collectWritePayload(form: HTMLFormElement): WriteCollection {
  const values: RuntimeSecretWriteValues = {};
  const deliveries: RuntimeSecretDelivery[] = [];
  const payload = { values, deliveries };
  const encoder = new TextEncoder();
  let totalBytes = 0;
  const materialRows = Array.from(
    form.querySelectorAll("[data-secret-material-row]"),
  );
  if (!materialRows.length || materialRows.length > 64) {
    return invalidWrite("Provide between 1 and 64 write-only values.", payload);
  }
  for (const row of materialRows) {
    const key = fieldValue(row, "[data-secret-material-key]")?.trim() ?? "";
    const value = fieldValue(row, "[data-secret-material-value]") ?? "";
    if (!secretKeyPattern.test(key)) {
      return invalidWrite(
        "Each secret key must use 1–253 letters, digits, dots, underscores, or hyphens.",
        payload,
      );
    }
    if (Object.hasOwn(values, key)) {
      return invalidWrite("Secret keys must be unique.", payload);
    }
    const valueBytes = encoder.encode(value).byteLength;
    if (valueBytes < 1 || valueBytes > 65_536) {
      return invalidWrite(
        "Each write-only value must contain 1–65,536 UTF-8 bytes.",
        payload,
      );
    }
    totalBytes += valueBytes;
    if (totalBytes > 262_144) {
      return invalidWrite(
        "Write-only values may contain at most 262,144 UTF-8 bytes in total.",
        payload,
      );
    }
    values[key] = value;
  }

  const deliveryRows = Array.from(
    form.querySelectorAll("[data-secret-delivery-row]"),
  );
  if (!deliveryRows.length || deliveryRows.length > 128) {
    return invalidWrite("Provide between 1 and 128 deliveries.", payload);
  }
  const usedKeys = new Set<string>();
  const environmentDestinations = new Set<string>();
  const fileDestinations = new Set<string>();
  for (const row of deliveryRows) {
    const sourceKey =
      fieldValue(row, "[data-secret-delivery-source]")?.trim() ?? "";
    const kind = fieldValue(row, "[data-secret-delivery-kind]");
    if (
      !secretKeyPattern.test(sourceKey) ||
      !Object.hasOwn(values, sourceKey)
    ) {
      return invalidWrite(
        "Every delivery source must exactly match one write-only secret key.",
        payload,
      );
    }
    usedKeys.add(sourceKey);
    if (kind === "environment") {
      const environmentName =
        fieldValue(row, "[data-secret-environment-name]")?.trim() ?? "";
      if (!environmentNamePattern.test(environmentName)) {
        return invalidWrite(
          "Environment deliveries require a valid environment variable name.",
          payload,
        );
      }
      if (environmentDestinations.has(environmentName)) {
        return invalidWrite(
          "Environment variable delivery names must be unique.",
          payload,
        );
      }
      environmentDestinations.add(environmentName);
      deliveries.push({
        sourceKey,
        kind: "environment",
        environmentName,
      });
      continue;
    }
    if (kind !== "file") {
      return invalidWrite("Choose an explicit delivery kind.", payload);
    }
    const filePath = fieldValue(row, "[data-secret-file-path]")?.trim() ?? "";
    const encodedMode = fieldValue(row, "[data-secret-file-mode]");
    const fileMode = encodedMode === "288" ? 288 : 256;
    if (filePath.length > 1024 || !filePathPattern.test(filePath)) {
      return invalidWrite(
        "File deliveries must stay below /var/run/secrets/kuberploy/ with safe path segments.",
        payload,
      );
    }
    if (fileDestinations.has(filePath)) {
      return invalidWrite("File delivery paths must be unique.", payload);
    }
    fileDestinations.add(filePath);
    deliveries.push({ sourceKey, kind: "file", filePath, fileMode });
  }
  if (usedKeys.size !== Object.keys(values).length) {
    return invalidWrite(
      "Every write-only value must have at least one explicit delivery.",
      payload,
    );
  }
  return { ok: true, payload };
}

function SecretWriteFields({ prefix }: { prefix: string }) {
  const [materialRows, setMaterialRows] = useState([0]);
  const [deliveryRows, setDeliveryRows] = useState<DeliveryRow[]>([
    { id: 0, kind: "environment" },
  ]);

  return (
    <div className="runtime-secret-write-fields">
      <section>
        <div className="runtime-secret-subhead">
          <div>
            <h4>Write-only values</h4>
            <p>Values leave this form once and are never readable again.</p>
          </div>
          <Button
            type="button"
            variant="secondary"
            onClick={() =>
              setMaterialRows((rows) => [...rows, (rows.at(-1) ?? -1) + 1])
            }
            disabled={materialRows.length >= 64}
          >
            <Icon name="plus" /> Value
          </Button>
        </div>
        <div className="runtime-secret-value-list">
          {materialRows.map((rowId, index) => (
            <div
              className="runtime-secret-value-row"
              data-secret-material-row
              key={rowId}
            >
              <Field label={`Secret key ${index + 1}`} required>
                <input
                  aria-label={`${prefix} secret key ${index + 1}`}
                  data-secret-material-key
                  autoComplete="off"
                  spellCheck={false}
                />
              </Field>
              <Field label={`Write-only value ${index + 1}`} required>
                <input
                  aria-label={`${prefix} write-only value ${index + 1}`}
                  data-secret-material-value
                  type="password"
                  autoComplete="new-password"
                  spellCheck={false}
                />
              </Field>
              <button
                type="button"
                className="icon-button"
                aria-label={`Remove ${prefix} write-only value ${index + 1}`}
                disabled={materialRows.length === 1}
                onClick={() =>
                  setMaterialRows((rows) =>
                    rows.filter((candidate) => candidate !== rowId),
                  )
                }
              >
                <Icon name="close" />
              </button>
            </div>
          ))}
        </div>
      </section>

      <section>
        <div className="runtime-secret-subhead">
          <div>
            <h4>Explicit deliveries</h4>
            <p>Map every key to an environment variable or confined file.</p>
          </div>
          <Button
            type="button"
            variant="secondary"
            onClick={() =>
              setDeliveryRows((rows) => [
                ...rows,
                { id: (rows.at(-1)?.id ?? -1) + 1, kind: "environment" },
              ])
            }
            disabled={deliveryRows.length >= 128}
          >
            <Icon name="plus" /> Delivery
          </Button>
        </div>
        <div className="runtime-secret-delivery-list">
          {deliveryRows.map((row, index) => (
            <div
              className="runtime-secret-delivery-row"
              data-secret-delivery-row
              key={row.id}
            >
              <Field label={`Source key ${index + 1}`} required>
                <input
                  aria-label={`${prefix} delivery source ${index + 1}`}
                  data-secret-delivery-source
                  autoComplete="off"
                  spellCheck={false}
                />
              </Field>
              <Field label="Delivery" required>
                <select
                  aria-label={`${prefix} delivery kind ${index + 1}`}
                  data-secret-delivery-kind
                  value={row.kind}
                  onChange={(event) =>
                    setDeliveryRows((rows) =>
                      rows.map((candidate) =>
                        candidate.id === row.id
                          ? {
                              ...candidate,
                              kind: event.target.value as DeliveryRow["kind"],
                            }
                          : candidate,
                      ),
                    )
                  }
                >
                  <option value="environment">Environment variable</option>
                  <option value="file">Read-only file</option>
                </select>
              </Field>
              {row.kind === "environment" ? (
                <Field label="Variable name" required>
                  <input
                    aria-label={`${prefix} environment variable ${index + 1}`}
                    data-secret-environment-name
                    autoComplete="off"
                    spellCheck={false}
                  />
                </Field>
              ) : (
                <>
                  <Field label="Confined file path" required>
                    <input
                      aria-label={`${prefix} file path ${index + 1}`}
                      data-secret-file-path
                      defaultValue="/var/run/secrets/kuberploy/"
                      autoComplete="off"
                      spellCheck={false}
                    />
                  </Field>
                  <Field label="Read-only mode" required>
                    <select
                      aria-label={`${prefix} file mode ${index + 1}`}
                      data-secret-file-mode
                      defaultValue="256"
                    >
                      <option value="256">0400 · owner</option>
                      <option value="288">0440 · owner/group</option>
                    </select>
                  </Field>
                </>
              )}
              <button
                type="button"
                className="icon-button"
                aria-label={`Remove ${prefix} delivery ${index + 1}`}
                disabled={deliveryRows.length === 1}
                onClick={() =>
                  setDeliveryRows((rows) =>
                    rows.filter((candidate) => candidate.id !== row.id),
                  )
                }
              >
                <Icon name="close" />
              </button>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}

function MutationNotice({
  tone,
  children,
}: {
  tone: "success" | "error";
  children: string;
}) {
  return (
    <div
      className={`notice notice--${tone === "success" ? "success" : "error"}`}
      role={tone === "error" ? "alert" : "status"}
    >
      <div>
        <strong>
          {tone === "success" ? "Request accepted" : "Request failed"}
        </strong>
        <p>{children}</p>
      </div>
    </div>
  );
}

function CreateSecretBindingForm({
  application,
  environment,
  onClose,
  onCreated,
}: {
  application: Application;
  environment: Environment;
  onClose: () => void;
  onCreated: (binding: RuntimeSecretBindingDetail) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [feedback, setFeedback] = useState<{
    tone: "success" | "error";
    message: string;
  }>();
  const [retryKey, setRetryKey] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const name =
      form
        .querySelector<HTMLInputElement>("[data-secret-binding-name]")
        ?.value.trim() ?? "";
    const provider = form.querySelector<HTMLSelectElement>(
      "[data-secret-provider]",
    )?.value;
    if (!bindingNamePattern.test(name)) {
      setFeedback({
        tone: "error",
        message:
          "Use a 1–63 character lowercase Kubernetes-style binding name.",
      });
      return;
    }
    if (provider !== "external-secrets" && provider !== "sealed-secrets") {
      setFeedback({ tone: "error", message: "Choose a supported provider." });
      return;
    }
    const collected = collectWritePayload(form);
    if (!collected.ok) {
      setFeedback({ tone: "error", message: collected.error });
      return;
    }
    const idempotencyKey = retryKey || crypto.randomUUID();
    setBusy(true);
    setFeedback(undefined);
    let result: RuntimeSecretBindingDetail | undefined;
    try {
      result = await api.createRuntimeSecretBinding(
        application.id,
        {
          environmentId: environment.id,
          name,
          provider: provider as RuntimeSecretProvider,
          deliveries: collected.payload.deliveries,
          values: collected.payload.values,
        },
        idempotencyKey,
      );
      setRetryKey("");
      setFeedback({
        tone: "success",
        message: "Safe binding metadata was accepted for provider staging.",
      });
    } catch {
      setRetryKey(idempotencyKey);
      setFeedback({
        tone: "error",
        message:
          "The write-only request failed. Values were cleared; re-enter the exact same request to retry safely.",
      });
    } finally {
      destroyWritePayload(collected.payload);
      clearWriteOnlyForm(form);
      setBusy(false);
    }
    if (result) onCreated(result);
  }

  return (
    <Card className="runtime-secret-form-card">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">One-way ingestion</span>
          <h3>Create runtime-secret binding</h3>
          <p>
            Destination: {environment.name}. Namespace and project identity are
            resolved by the server.
          </p>
        </div>
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
          Close
        </Button>
      </div>
      <form onSubmit={submit} autoComplete="off">
        <div className="runtime-secret-form-meta">
          <Field label="Binding name" required>
            <input
              aria-label="Runtime secret binding name"
              data-secret-binding-name
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field label="Provider" required>
            <select
              aria-label="Runtime secret provider"
              data-secret-provider
              defaultValue="external-secrets"
            >
              <option value="external-secrets">External Secrets</option>
              <option value="sealed-secrets">Sealed Secrets</option>
            </select>
          </Field>
        </div>
        <SecretWriteFields prefix="Create" />
        {feedback ? (
          <MutationNotice tone={feedback.tone}>
            {feedback.message}
          </MutationNotice>
        ) : null}
        {retryKey ? (
          <div className="runtime-secret-retry-note">
            <PlaceholderBadge>Stable retry protected</PlaceholderBadge>
            Re-enter the exact failed request or close this form to start a new
            mutation.
          </div>
        ) : null}
        <div className="runtime-secret-form-actions">
          <Button type="submit" busy={busy}>
            Ingest write-only values
          </Button>
        </div>
      </form>
    </Card>
  );
}

function deliveryLabel(delivery: RuntimeSecretDelivery) {
  return delivery.kind === "environment"
    ? `${delivery.sourceKey} → ${delivery.environmentName}`
    : `${delivery.sourceKey} → ${delivery.filePath} (${delivery.fileMode === 288 ? "0440" : "0400"})`;
}

function SecretBindingDetailPanel({
  binding,
  canRotate,
  canDelete,
  onChanged,
  onDeleted,
}: {
  binding: RuntimeSecretBindingDetail;
  canRotate: boolean;
  canDelete: boolean;
  onChanged: () => void;
  onDeleted: () => void;
}) {
  const [rotating, setRotating] = useState(false);
  const [rotateBusy, setRotateBusy] = useState(false);
  const [rotateRetry, setRotateRetry] = useState<{
    key: string;
    expectedActiveVersion: number;
  }>();
  const [rotateFeedback, setRotateFeedback] = useState<{
    tone: "success" | "error";
    message: string;
  }>();
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteRetryKey, setDeleteRetryKey] = useState("");
  const [deleteFeedback, setDeleteFeedback] = useState<string>();

  async function rotate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const collected = collectWritePayload(form);
    if (!collected.ok) {
      setRotateFeedback({ tone: "error", message: collected.error });
      return;
    }
    const expectedActiveVersion =
      rotateRetry?.expectedActiveVersion ?? binding.activeVersion;
    if (!expectedActiveVersion) {
      destroyWritePayload(collected.payload);
      clearWriteOnlyForm(form);
      setRotateFeedback({
        tone: "error",
        message: "Rotation requires an observed active version.",
      });
      return;
    }
    const idempotencyKey = rotateRetry?.key ?? crypto.randomUUID();
    setRotateBusy(true);
    setRotateFeedback(undefined);
    let succeeded = false;
    try {
      await api.rotateRuntimeSecretBinding(
        binding.id,
        {
          expectedActiveVersion,
          deliveries: collected.payload.deliveries,
          values: collected.payload.values,
        },
        idempotencyKey,
      );
      succeeded = true;
      setRotateRetry(undefined);
      setRotateFeedback({
        tone: "success",
        message: "A new immutable version was accepted for provider staging.",
      });
    } catch {
      setRotateRetry({ key: idempotencyKey, expectedActiveVersion });
      setRotateFeedback({
        tone: "error",
        message:
          "The write-only rotation failed. Values were cleared; re-enter the exact same request to retry safely.",
      });
    } finally {
      destroyWritePayload(collected.payload);
      clearWriteOnlyForm(form);
      setRotateBusy(false);
    }
    if (succeeded) onChanged();
  }

  async function remove(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const confirmation =
      form.querySelector<HTMLInputElement>("[data-secret-delete-confirmation]")
        ?.value ?? "";
    if (confirmation !== binding.name) {
      setDeleteFeedback("Type the exact binding name before deletion.");
      return;
    }
    const idempotencyKey = deleteRetryKey || crypto.randomUUID();
    setDeleteBusy(true);
    setDeleteFeedback(undefined);
    let succeeded = false;
    try {
      await api.deleteRuntimeSecretBinding(binding.id, idempotencyKey);
      succeeded = true;
      setDeleteRetryKey("");
    } catch {
      setDeleteRetryKey(idempotencyKey);
      setDeleteFeedback(
        "Deletion failed without exposing provider details. Exact confirmation was cleared.",
      );
    } finally {
      form.reset();
      setDeleteBusy(false);
    }
    if (succeeded) onDeleted();
  }

  return (
    <Card className="runtime-secret-detail">
      <div className="card__header card__header--inside">
        <div>
          <span className="eyebrow">Safe metadata only</span>
          <h3>{binding.name}</h3>
          <p>
            {titleCase(binding.provider)} · updated{" "}
            {formatDate(binding.updatedAt)}
          </p>
        </div>
        <StatusPill value={binding.state} />
      </div>
      <div className="runtime-secret-detail-grid">
        <div>
          <span>Environment ID</span>
          <code>{binding.environmentId}</code>
        </div>
        <div>
          <span>Active version</span>
          <strong>{binding.activeVersion ?? "Not active"}</strong>
        </div>
        <div>
          <span>Created</span>
          <strong>{formatDate(binding.createdAt)}</strong>
        </div>
      </div>
      <section className="runtime-secret-versions">
        <div className="runtime-secret-subhead">
          <div>
            <h4>Immutable versions</h4>
            <p>Only lifecycle and delivery descriptors are available.</p>
          </div>
          {canRotate && binding.activeVersion ? (
            <Button
              type="button"
              variant="secondary"
              onClick={() => setRotating((value) => !value)}
            >
              <Icon name="refresh" /> Rotate
            </Button>
          ) : null}
        </div>
        {binding.versions.length ? (
          <div className="runtime-secret-version-list">
            {binding.versions.map((version) => (
              <article key={version.id}>
                <div>
                  <strong>Version {version.number}</strong>
                  <StatusPill value={version.state} />
                </div>
                <small>Created {formatDate(version.createdAt)}</small>
                {version.failureCode ? (
                  <PlaceholderBadge>{version.failureCode}</PlaceholderBadge>
                ) : null}
                <ul>
                  {version.deliveries.map((delivery) => (
                    <li key={deliveryLabel(delivery)}>
                      <code>{deliveryLabel(delivery)}</code>
                    </li>
                  ))}
                </ul>
              </article>
            ))}
          </div>
        ) : (
          <p className="runtime-secret-empty-copy">No version metadata.</p>
        )}
      </section>

      {rotating ? (
        <form
          className="runtime-secret-rotation-form"
          onSubmit={rotate}
          autoComplete="off"
        >
          <div className="runtime-secret-subhead">
            <div>
              <h4>Rotate from version {binding.activeVersion}</h4>
              <p>The compare-and-swap version is sent exactly as observed.</p>
            </div>
          </div>
          <SecretWriteFields prefix="Rotate" />
          {rotateFeedback ? (
            <MutationNotice tone={rotateFeedback.tone}>
              {rotateFeedback.message}
            </MutationNotice>
          ) : null}
          {rotateRetry ? (
            <div className="runtime-secret-retry-note">
              <PlaceholderBadge>Stable retry protected</PlaceholderBadge>
              Retry remains bound to active version{" "}
              {rotateRetry.expectedActiveVersion}.
            </div>
          ) : null}
          <div className="runtime-secret-form-actions">
            <Button type="submit" busy={rotateBusy}>
              Ingest new version
            </Button>
          </div>
        </form>
      ) : null}

      {canDelete ? (
        <form className="runtime-secret-delete" onSubmit={remove}>
          <div>
            <span className="eyebrow">Exact destructive confirmation</span>
            <h4>Delete unreferenced binding</h4>
            <p>
              Type <code>{binding.name}</code>. Referenced or readiness-pending
              versions remain protected by the server.
            </p>
          </div>
          <Field label="Exact binding name">
            <input
              aria-label="Exact runtime secret binding name confirmation"
              data-secret-delete-confirmation
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Button type="submit" variant="danger" busy={deleteBusy}>
            Delete binding
          </Button>
          {deleteFeedback ? (
            <MutationNotice tone="error">{deleteFeedback}</MutationNotice>
          ) : null}
          {deleteRetryKey ? (
            <PlaceholderBadge>Stable delete retry protected</PlaceholderBadge>
          ) : null}
        </form>
      ) : null}
    </Card>
  );
}

export function RuntimeSecretsPanel({
  application,
  environments,
  project,
  capabilities,
  featureEnabled,
  humanSession,
}: {
  application: Application;
  environments: Environment[];
  project?: Project;
  capabilities: Capability[];
  featureEnabled: boolean;
  humanSession: boolean;
}) {
  const queryClient = useQueryClient();
  const [selectedEnvironmentId, setSelectedEnvironmentId] = useState("");
  const [selectedBindingId, setSelectedBindingId] = useState("");
  const [creating, setCreating] = useState(false);
  const readableEnvironments = runtimeSecretEnvironments(
    capabilities,
    "secret-bindings:read",
    application,
    environments,
    project,
  );
  const selectedEnvironment =
    readableEnvironments.find(
      (environment) => environment.id === selectedEnvironmentId,
    ) ?? readableEnvironments[0];
  const list = useQuery({
    queryKey: [
      "runtime-secret-bindings",
      application.id,
      selectedEnvironment?.id,
    ],
    queryFn: () =>
      api.runtimeSecretBindings(application.id, selectedEnvironment!.id),
    enabled: featureEnabled && Boolean(selectedEnvironment),
    retry: false,
  });
  const selectedListedBinding = list.data?.items.some(
    (binding) => binding.id === selectedBindingId,
  );
  const detail = useQuery({
    queryKey: ["runtime-secret-binding", selectedBindingId],
    queryFn: () => api.runtimeSecretBinding(selectedBindingId),
    enabled:
      featureEnabled && Boolean(selectedBindingId) && selectedListedBinding,
    retry: false,
  });

  if (!featureEnabled) return null;
  if (!selectedEnvironment) {
    return (
      <EmptyState
        icon="code"
        title="Secret metadata access not granted"
        description="An exact effective secret-bindings:read capability covering an application environment is required."
        action={<PlaceholderBadge>Scoped access required</PlaceholderBadge>}
      />
    );
  }

  const canCreate =
    humanSession &&
    hasRuntimeSecretCapability(
      capabilities,
      "secret-bindings:create",
      application,
      selectedEnvironment,
      project,
    );
  const canRotate =
    humanSession &&
    hasRuntimeSecretCapability(
      capabilities,
      "secret-bindings:rotate",
      application,
      selectedEnvironment,
      project,
    );
  const canDelete =
    humanSession &&
    hasRuntimeSecretCapability(
      capabilities,
      "secret-bindings:delete",
      application,
      selectedEnvironment,
      project,
    );

  async function refreshList() {
    await queryClient.invalidateQueries({
      queryKey: [
        "runtime-secret-bindings",
        application.id,
        selectedEnvironment.id,
      ],
    });
  }

  return (
    <div className="runtime-secrets-panel">
      <Card className="runtime-secret-toolbar">
        <div>
          <span className="runtime-secret-toolbar__icon">
            <Icon name="code" />
          </span>
          <span>
            <strong>Variables &amp; secrets</strong>
            <small>Metadata is readable; values are write-only.</small>
          </span>
        </div>
        <Field label="Application environment">
          <select
            aria-label="Runtime secret environment"
            value={selectedEnvironment.id}
            onChange={(event) => {
              setSelectedEnvironmentId(event.target.value);
              setSelectedBindingId("");
              setCreating(false);
            }}
          >
            {readableEnvironments.map((environment) => (
              <option key={environment.id} value={environment.id}>
                {environment.name} · {environment.namespace}
              </option>
            ))}
          </select>
        </Field>
        {canCreate ? (
          <Button type="button" onClick={() => setCreating((value) => !value)}>
            <Icon name="plus" /> New binding
          </Button>
        ) : null}
      </Card>

      {!humanSession ? (
        <div className="notice notice--warning" role="status">
          <Icon name="code" />
          <div>
            <strong>Metadata-only automation session</strong>
            <p>
              Create, rotate, and delete require an interactive human session.
            </p>
          </div>
        </div>
      ) : null}

      {creating && canCreate ? (
        <CreateSecretBindingForm
          application={application}
          environment={selectedEnvironment}
          onClose={() => setCreating(false)}
          onCreated={(binding) => {
            setCreating(false);
            setSelectedBindingId(binding.id);
            void refreshList();
          }}
        />
      ) : null}

      {list.error ? (
        <ErrorPanel
          error={list.error}
          title="Could not load runtime-secret metadata"
          onRetry={() => void list.refetch()}
        />
      ) : null}

      {list.isPending ? (
        <Card>
          <Skeleton lines={6} />
        </Card>
      ) : list.data?.items.length ? (
        <div className="runtime-secret-layout">
          <Card className="runtime-secret-binding-list">
            <div className="card__header card__header--inside">
              <div>
                <span className="eyebrow">Environment metadata</span>
                <h3>Runtime-secret bindings</h3>
              </div>
              <PlaceholderBadge>
                {list.data.items.length} bindings
              </PlaceholderBadge>
            </div>
            <div>
              {list.data.items.map((binding) => (
                <button
                  type="button"
                  key={binding.id}
                  className={
                    binding.id === selectedBindingId
                      ? "runtime-secret-binding runtime-secret-binding--active"
                      : "runtime-secret-binding"
                  }
                  onClick={() => setSelectedBindingId(binding.id)}
                >
                  <span>
                    <strong>{binding.name}</strong>
                    <small>{titleCase(binding.provider)}</small>
                  </span>
                  <span>
                    <StatusPill value={binding.state} />
                    <small>
                      {binding.activeVersion
                        ? `v${binding.activeVersion}`
                        : "No active version"}
                    </small>
                  </span>
                </button>
              ))}
            </div>
          </Card>

          <div>
            {detail.error ? (
              <ErrorPanel
                error={detail.error}
                title="Could not load version metadata"
                onRetry={() => void detail.refetch()}
              />
            ) : detail.isPending && selectedBindingId ? (
              <Card>
                <Skeleton lines={7} />
              </Card>
            ) : detail.data ? (
              <SecretBindingDetailPanel
                key={detail.data.id}
                binding={detail.data}
                canRotate={canRotate}
                canDelete={canDelete}
                onChanged={() => {
                  void refreshList();
                  void detail.refetch();
                }}
                onDeleted={() => {
                  setSelectedBindingId("");
                  void refreshList();
                }}
              />
            ) : (
              <EmptyState
                icon="code"
                title="Select a binding"
                description="Choose safe metadata to inspect immutable version and delivery status."
                compact
              />
            )}
          </div>
        </div>
      ) : list.data ? (
        <EmptyState
          icon="code"
          title="No runtime-secret bindings"
          description="This environment has no safe runtime-secret metadata yet. Secret values are never listed."
          action={
            canCreate ? (
              <Button type="button" onClick={() => setCreating(true)}>
                Create write-only binding
              </Button>
            ) : (
              <PlaceholderBadge>Read-only</PlaceholderBadge>
            )
          }
        />
      ) : null}
    </div>
  );
}
