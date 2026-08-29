import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
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
import { writeOnlyRequestSignature } from "../lib/writeOnlyRequest";
import { Icon } from "./Icon";
import {
  Select,
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  Notice,
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
  const bindingName = form.querySelector<HTMLInputElement>(
    "[data-secret-binding-name]",
  );
  if (bindingName) bindingName.value = "";
  form
    .querySelectorAll<HTMLInputElement>(
      "[data-secret-material-key], [data-secret-material-value], [data-secret-delivery-source]",
    )
    .forEach((input) => {
      input.value = "";
    });
  form
    .querySelectorAll<HTMLInputElement>(
      "[data-secret-environment-name], [data-secret-file-path]",
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
    <div className="grid gap-5 [&_+_[data-slot='notice']]:mt-4">
      <section>
        <div className="flex items-start justify-between gap-4 mb-3 [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55]">
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
        <div className="grid gap-2">
          {materialRows.map((rowId, index) => (
            <div
              className="grid grid-cols-[minmax(140px,_0.8fr)_minmax(180px,_1.2fr)_32px] items-end gap-2 [&_.icon-button]:mb-0.5 to-580:grid-cols-[1fr]"
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
                className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
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
        <div className="flex items-start justify-between gap-4 mb-3 [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55]">
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
        <div className="grid gap-2">
          {deliveryRows.map((row, index) => (
            <div
              className="flex items-end gap-2 [&_.field]:min-w-[110px] [&_.field]:flex-1 [&_.field:nth-child(3)]:flex-[1.4] [&_.icon-button]:mb-0.5 to-820:grid to-820:grid-cols-[1fr_1fr_32px] to-820:[&_.field:nth-child(3)]:col-[1_/_span_2] to-820:[&_.field:nth-child(4)]:col-[1_/_span_2] to-820:[&_.icon-button]:row-[1] to-820:[&_.icon-button]:col-[3] to-580:grid-cols-[1fr] to-580:[&_.field:nth-child(3)]:row-[auto] to-580:[&_.field:nth-child(3)]:col-[auto] to-580:[&_.field:nth-child(4)]:row-[auto] to-580:[&_.field:nth-child(4)]:col-[auto] to-580:[&_.icon-button]:row-[auto] to-580:[&_.icon-button]:col-[auto]"
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
                <Select
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
                </Select>
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
                    <Select
                      aria-label={`${prefix} file mode ${index + 1}`}
                      data-secret-file-mode
                      defaultValue="256"
                    >
                      <option value="256">0400 · owner</option>
                      <option value="288">0440 · owner/group</option>
                    </Select>
                  </Field>
                </>
              )}
              <button
                type="button"
                className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
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
    <Notice
      tone={tone === "success" ? "success" : "error"}
      role={tone === "error" ? "alert" : "status"}
    >
      <div>
        <strong>
          {tone === "success" ? "Request accepted" : "Request failed"}
        </strong>
        <p>{children}</p>
      </div>
    </Notice>
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
  const submitBusyRef = useRef(false);
  const [feedback, setFeedback] = useState<{
    tone: "success" | "error";
    message: string;
  }>();
  const [retryKey, setRetryKey] = useState("");
  const [retrySignature, setRetrySignature] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy || submitBusyRef.current) return;
    const form = event.currentTarget;
    const name =
      form
        .querySelector<HTMLInputElement>("[data-secret-binding-name]")
        ?.value.trim() ?? "";
    const provider: RuntimeSecretProvider = "sealed-secrets";
    if (!bindingNamePattern.test(name)) {
      setFeedback({
        tone: "error",
        message:
          "Use a 1–63 character lowercase Kubernetes-style binding name.",
      });
      return;
    }
    const collected = collectWritePayload(form);
    if (!collected.ok) {
      setFeedback({ tone: "error", message: collected.error });
      return;
    }
    submitBusyRef.current = true;
    setBusy(true);
    let signature: string;
    try {
      signature = await writeOnlyRequestSignature({
        applicationId: application.id,
        environmentId: environment.id,
        name,
        provider,
        deliveries: collected.payload.deliveries,
        values: collected.payload.values,
      });
    } catch {
      destroyWritePayload(collected.payload);
      clearWriteOnlyForm(form);
      submitBusyRef.current = false;
      setBusy(false);
      setFeedback({
        tone: "error",
        message: "The write-only request could not be prepared. Try again.",
      });
      return;
    }
    const idempotencyKey =
      retryKey && retrySignature === signature ? retryKey : crypto.randomUUID();
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
      setRetrySignature("");
      setFeedback({
        tone: "success",
        message: "Safe binding metadata was accepted for provider staging.",
      });
    } catch {
      setRetryKey(idempotencyKey);
      setRetrySignature(signature);
      setFeedback({
        tone: "error",
        message:
          "The write-only request failed. Values were cleared; re-enter the exact same request to retry safely.",
      });
    } finally {
      destroyWritePayload(collected.payload);
      clearWriteOnlyForm(form);
      submitBusyRef.current = false;
      setBusy(false);
    }
    if (result) onCreated(result);
  }

  return (
    <Card className="p-0 [&>form]:p-5">
      <CardHeader>
        <div>
          <Eyebrow>One-way ingestion</Eyebrow>
          <h3>Create runtime-secret binding</h3>
          <p>
            Destination: {environment.name}. Namespace and project identity are
            resolved by the server.
          </p>
        </div>
        <Button type="button" variant="ghost" onClick={onClose} disabled={busy}>
          Close
        </Button>
      </CardHeader>
      <form onSubmit={submit} autoComplete="off">
        <fieldset disabled={busy}>
          <div className="grid grid-cols-[1fr_1fr] gap-3 mb-5 to-580:grid-cols-[1fr]">
            <Field label="Binding name" required>
              <input
                aria-label="Runtime secret binding name"
                data-secret-binding-name
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
            <Field label="Provider">
              <input
                aria-label="Runtime secret provider"
                value="Sealed Secrets"
                readOnly
              />
            </Field>
          </div>
          <SecretWriteFields prefix="Create" />
          {feedback ? (
            <MutationNotice tone={feedback.tone}>
              {feedback.message}
            </MutationNotice>
          ) : null}
          {retryKey ? (
            <div className="mt-4 flex items-center gap-2 text-ink-soft text-xs">
              <PlaceholderBadge>Stable retry protected</PlaceholderBadge>
              Re-enter the exact failed request or close this form to start a
              new mutation.
            </div>
          ) : null}
          <div className="flex justify-end mt-4">
            <Button type="submit" busy={busy}>
              Ingest write-only values
            </Button>
          </div>
        </fieldset>
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
  const rotateBusyRef = useRef(false);
  const [rotateRetry, setRotateRetry] = useState<{
    key: string;
    expectedActiveVersion: number;
    signature: string;
  }>();
  const [rotateFeedback, setRotateFeedback] = useState<{
    tone: "success" | "error";
    message: string;
  }>();
  const [deleteBusy, setDeleteBusy] = useState(false);
  const deleteBusyRef = useRef(false);
  const [deleteRetryKey, setDeleteRetryKey] = useState("");
  const [deleteFeedback, setDeleteFeedback] = useState<string>();

  useEffect(() => {
    // A failed retry is bound to the observed CAS version. Once another
    // observation changes that version, the old write must not be replayed.
    setRotateRetry(undefined);
    setRotateFeedback(undefined);
  }, [binding.id, binding.activeVersion]);

  async function rotate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (rotateBusy || rotateBusyRef.current) return;
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
    rotateBusyRef.current = true;
    setRotateBusy(true);
    let signature: string;
    try {
      signature = await writeOnlyRequestSignature({
        bindingId: binding.id,
        expectedActiveVersion,
        deliveries: collected.payload.deliveries,
        values: collected.payload.values,
      });
    } catch {
      destroyWritePayload(collected.payload);
      clearWriteOnlyForm(form);
      rotateBusyRef.current = false;
      setRotateBusy(false);
      setRotateFeedback({
        tone: "error",
        message: "The write-only rotation could not be prepared. Try again.",
      });
      return;
    }
    const idempotencyKey =
      rotateRetry?.signature === signature
        ? rotateRetry.key
        : crypto.randomUUID();
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
        message: "A new version was accepted for provider staging.",
      });
    } catch {
      setRotateRetry({ key: idempotencyKey, expectedActiveVersion, signature });
      setRotateFeedback({
        tone: "error",
        message:
          "The write-only rotation failed. Values were cleared; re-enter the exact same request to retry safely.",
      });
    } finally {
      destroyWritePayload(collected.payload);
      clearWriteOnlyForm(form);
      rotateBusyRef.current = false;
      setRotateBusy(false);
    }
    if (succeeded) onChanged();
  }

  async function remove(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (deleteBusy || deleteBusyRef.current) return;
    const form = event.currentTarget;
    const confirmation =
      form.querySelector<HTMLInputElement>("[data-secret-delete-confirmation]")
        ?.value ?? "";
    if (confirmation !== binding.name) {
      setDeleteFeedback("Type the exact binding name before deletion.");
      return;
    }
    const idempotencyKey = deleteRetryKey || crypto.randomUUID();
    deleteBusyRef.current = true;
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
      deleteBusyRef.current = false;
      setDeleteBusy(false);
    }
    if (succeeded) onDeleted();
  }

  return (
    <Card className="p-0">
      <CardHeader>
        <div>
          <Eyebrow>Safe metadata only</Eyebrow>
          <h3>{binding.name}</h3>
          <p>
            {titleCase(binding.provider)} · updated{" "}
            {formatDate(binding.updatedAt)}
          </p>
        </div>
        <StatusPill value={binding.state} />
      </CardHeader>
      <div className="grid grid-cols-[1.4fr_0.7fr_0.9fr] gap-px border-y border-y-line bg-line [&>div]:min-w-0 [&>div]:py-3 [&>div]:px-4 [&>div]:bg-surface-soft [&_span]:block [&_span]:mb-1.5 [&_span]:text-ink-faint [&_span]:text-[11px] [&_span]:font-semibold [&_span]:tracking-[0.06em] [&_span]:uppercase [&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-xs [&_strong]:text-ellipsis [&_code]:block [&_code]:overflow-hidden [&_code]:text-xs [&_code]:text-ellipsis to-580:grid-cols-[1fr]">
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
      <section className="p-5">
        <div className="flex items-start justify-between gap-4 mb-3 [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55]">
          <div>
            <h4>Versions</h4>
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
          <div className="grid gap-2 [&_article]:py-3 [&_article]:px-4 [&_article]:border [&_article]:border-line [&_article]:rounded-lg [&_article]:bg-surface-soft [&_article_>_div]:flex [&_article_>_div]:items-center [&_article_>_div]:justify-between [&_article_>_div]:gap-3 [&_strong]:text-meta [&_small]:block [&_small]:mt-1 [&_small]:text-ink-faint [&_small]:text-[11px] [&_ul]:grid [&_ul]:gap-1 [&_ul]:mt-2 [&_ul]:mx-0 [&_ul]:mb-0 [&_ul]:p-0 [&_ul]:list-none [&_code]:text-mint-dark [&_code]:text-[11px] [&_code]:break-words">
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
          <p className="mt-1 mx-0 mb-0 text-ink-soft text-xs leading-[1.55]">
            No version metadata.
          </p>
        )}
      </section>

      {rotating ? (
        <form
          className="p-5 border-t border-t-line"
          onSubmit={rotate}
          autoComplete="off"
        >
          <fieldset disabled={rotateBusy}>
            <div className="flex items-start justify-between gap-4 mb-3 [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55]">
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
              <div className="mt-4 flex items-center gap-2 text-ink-soft text-xs">
                <PlaceholderBadge>Stable retry protected</PlaceholderBadge>
                Retry remains bound to active version{" "}
                {rotateRetry.expectedActiveVersion}.
              </div>
            ) : null}
            <div className="flex justify-end mt-4">
              <Button type="submit" busy={rotateBusy}>
                Ingest new version
              </Button>
            </div>
          </fieldset>
        </form>
      ) : null}

      {canDelete ? (
        <form
          className="p-5 border-t border-t-line grid grid-cols-[minmax(240px,_1fr)_minmax(190px,_0.7fr)_auto] items-end gap-3 bg-tone-bad-surface [&_h4]:m-0 [&_h4]:text-[11px] [&_p]:mt-1 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-xs [&_p]:leading-[1.55] [&_[data-slot='notice']]:col-[1_/_-1] [&>[data-slot='placeholder-badge']]:col-[1_/_-1] to-820:grid-cols-[1fr_auto] to-820:[&>div:first-child]:col-[1_/_-1] to-580:grid-cols-[1fr] to-580:[&>div:first-child]:row-[auto] to-580:[&>div:first-child]:col-[auto]"
          onSubmit={remove}
        >
          <fieldset disabled={deleteBusy}>
            <div>
              <Eyebrow>Exact destructive confirmation</Eyebrow>
              <h4>Delete unreferenced binding</h4>
              <p>
                Type <code>{binding.name}</code>. Referenced or
                readiness-pending versions remain protected by the server.
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
          </fieldset>
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
  // The picked environment is a preference; the environment actually selected
  // is derived below from the environments readable in this render.
  const [environmentChoice, setSelectedEnvironmentId] = useState("");
  const [selectedBindingId, setSelectedBindingId] = useState("");
  const [creatingChoice, setCreating] = useState(false);
  const formScopeRef = useRef("");
  const selectedBindingRef = useRef("");
  selectedBindingRef.current = selectedBindingId;
  const readableEnvironments = runtimeSecretEnvironments(
    capabilities,
    "secret-bindings:read",
    application,
    environments,
    project,
  );
  const selectedEnvironmentId = readableEnvironments.some(
    (environment) => environment.id === environmentChoice,
  )
    ? environmentChoice
    : "";
  const selectedEnvironment =
    readableEnvironments.find(
      (environment) => environment.id === selectedEnvironmentId,
    ) ?? readableEnvironments[0];
  // The create form belongs to an environment; with no readable environment
  // left there is nothing to create against.
  const creating = creatingChoice && Boolean(selectedEnvironment);
  const formScope = `${application.id}:${selectedEnvironment?.id ?? ""}`;
  formScopeRef.current = formScope;
  useEffect(() => {
    formScopeRef.current = formScope;
  }, [formScope]);
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
    refetchInterval: (query) =>
      query.state.data?.items.some(
        (binding) =>
          binding.state === "provisioning" || binding.state === "deleting",
      )
        ? 1_000
        : false,
  });
  const selectedListedBinding = list.data?.items.find(
    (binding) => binding.id === selectedBindingId,
  );
  const detail = useQuery({
    queryKey: [
      "runtime-secret-binding",
      selectedBindingId,
      selectedListedBinding?.activeVersion ?? 0,
      selectedListedBinding?.state ?? "",
    ],
    queryFn: () => api.runtimeSecretBinding(selectedBindingId),
    enabled:
      featureEnabled &&
      Boolean(selectedBindingId) &&
      Boolean(selectedListedBinding),
    retry: false,
    refetchInterval: (query) => {
      const binding = query.state.data;
      if (!binding) return false;
      const pendingVersion =
        binding.state === "ready" &&
        binding.versions.some(
          (version) => version.number > (binding.activeVersion ?? 0),
        );
      return binding.state === "provisioning" ||
        binding.state === "deleting" ||
        pendingVersion
        ? 1_000
        : false;
    },
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
    <div className="grid gap-4">
      <Card className="grid grid-cols-[minmax(220px,_1fr)_minmax(230px,_330px)_auto] items-end gap-5 py-4 px-5 [&>div:first-child]:flex [&>div:first-child]:items-center [&>div:first-child]:gap-3 [&>div:first-child]:self-center [&_strong]:block [&_strong]:text-xs [&_small]:block [&_small]:mt-1 [&_small]:text-ink-faint [&_small]:text-xs [&_.field]:gap-1.5 to-820:grid-cols-[1fr_1fr] to-820:[&>div:first-child]:col-[1_/_-1] to-580:grid-cols-[1fr] to-580:[&>div:first-child]:row-[auto] to-580:[&>div:first-child]:col-[auto]">
        <div>
          <span className="grid w-9 h-9 place-items-center rounded-[9px] text-mint-dark bg-mint-soft [&_svg]:w-[17px]">
            <Icon name="code" />
          </span>
          <span>
            <strong>Variables &amp; secrets</strong>
            <small>Metadata is readable; values are write-only.</small>
          </span>
        </div>
        <Field label="Application environment">
          <Select
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
          </Select>
        </Field>
        {canCreate ? (
          <Button type="button" onClick={() => setCreating((value) => !value)}>
            <Icon name="plus" /> New binding
          </Button>
        ) : null}
      </Card>

      {!humanSession ? (
        <Notice tone="warning" role="status">
          <Icon name="code" />
          <div>
            <strong>Metadata-only automation session</strong>
            <p>
              Create, rotate, and delete require an interactive human session.
            </p>
          </div>
        </Notice>
      ) : null}

      {creating && canCreate ? (
        <CreateSecretBindingForm
          application={application}
          environment={selectedEnvironment}
          onClose={() => setCreating(false)}
          onCreated={(binding) => {
            if (formScopeRef.current !== formScope) return;
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
        <div className="grid grid-cols-[minmax(250px,_0.7fr)_minmax(420px,_1.3fr)] items-start gap-4 to-1120:grid-cols-[1fr]">
          <Card className="overflow-hidden p-0 [&>div:last-child]:grid">
            <CardHeader>
              <div>
                <Eyebrow>Environment metadata</Eyebrow>
                <h3>Runtime-secret bindings</h3>
              </div>
              <PlaceholderBadge>
                {list.data.items.length} bindings
              </PlaceholderBadge>
            </CardHeader>
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
                  if (
                    formScopeRef.current !== formScope ||
                    selectedBindingRef.current !== detail.data?.id
                  ) {
                    return;
                  }
                  void refreshList();
                  void detail.refetch();
                }}
                onDeleted={() => {
                  if (
                    formScopeRef.current !== formScope ||
                    selectedBindingRef.current !== detail.data?.id
                  ) {
                    return;
                  }
                  setSelectedBindingId("");
                  void refreshList();
                }}
              />
            ) : (
              <EmptyState
                icon="code"
                title="Select a binding"
                description="Choose safe metadata to inspect version and delivery status."
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
