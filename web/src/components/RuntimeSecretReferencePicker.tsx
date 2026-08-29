import { useQuery } from "@tanstack/react-query";
import { api, errorMessage } from "../api/client";
import type {
  RuntimeSecretBindingDetail,
  RuntimeSecretBindingMetadata,
} from "../api/types";
import { Select, Field } from "./ui";

export type RuntimeSecretReferenceDraft = {
  bindingId: string;
  bindingName: string;
  key: string;
  version: number;
};

const bindingIDPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const bindingNamePattern = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/;
const sourceKeyPattern = /^[A-Za-z0-9._-]+$/;

function referenceKey(bindingId: string, version: number) {
  return `${bindingId}@${version}`;
}

function selectableBinding(
  binding: RuntimeSecretBindingMetadata,
  applicationId: string,
  environmentId: string,
) {
  return (
    binding.applicationId === applicationId &&
    binding.environmentId === environmentId &&
    bindingIDPattern.test(binding.id) &&
    binding.name.length <= 63 &&
    bindingNamePattern.test(binding.name) &&
    binding.provider === "sealed-secrets" &&
    binding.state === "ready" &&
    Number.isSafeInteger(binding.activeVersion) &&
    (binding.activeVersion ?? 0) > 0
  );
}

function activeEnvironmentKeys(
  detail: RuntimeSecretBindingDetail | undefined,
  reference: RuntimeSecretReferenceDraft,
  applicationId: string,
  environmentId: string,
  environmentName: string,
) {
  if (
    !detail ||
    detail.id !== reference.bindingId ||
    detail.name !== reference.bindingName ||
    detail.applicationId !== applicationId ||
    detail.environmentId !== environmentId ||
    detail.provider !== "sealed-secrets" ||
    detail.state !== "ready" ||
    detail.activeVersion !== reference.version
  ) {
    return [];
  }
  const versions = detail.versions.filter(
    (candidate) =>
      candidate.number === reference.version &&
      Number.isSafeInteger(candidate.number) &&
      candidate.state === "active",
  );
  if (versions.length !== 1) return [];
  const counts = new Map<string, number>();
  for (const delivery of versions[0]!.deliveries) {
    if (
      delivery.kind === "environment" &&
      delivery.environmentName === environmentName.trim() &&
      delivery.sourceKey.length <= 253 &&
      sourceKeyPattern.test(delivery.sourceKey)
    ) {
      counts.set(delivery.sourceKey, (counts.get(delivery.sourceKey) ?? 0) + 1);
    }
  }
  return [...counts.entries()]
    .filter(([, count]) => count === 1)
    .map(([key]) => key)
    .sort();
}

function DisabledReferencePicker({
  index,
  value,
  reason,
}: {
  index: number;
  value: RuntimeSecretReferenceDraft;
  reason: string;
}) {
  return (
    <>
      <Field label={index === 0 ? "Binding" : ""} hint={reason}>
        <Select
          aria-label={`Secret variable ${index + 1} binding`}
          value={value.bindingId}
          disabled
        >
          <option value={value.bindingId}>
            {value.bindingId
              ? `${value.bindingName} · v${value.version || "?"} (unavailable)`
              : "Runtime-secret picker unavailable"}
          </option>
        </Select>
      </Field>
      <Field label={index === 0 ? "Key" : ""}>
        <Select
          aria-label={`Secret variable ${index + 1} key`}
          value={value.key}
          disabled
        >
          <option value={value.key}>{value.key || "Select a binding"}</option>
        </Select>
      </Field>
      <Field label={index === 0 ? "Version" : ""}>
        <output aria-label={`Secret variable ${index + 1} version`}>
          {value.version > 0 ? `v${value.version}` : "—"}
        </output>
      </Field>
    </>
  );
}

function ActiveReferencePicker({
  index,
  applicationId,
  environmentId,
  environmentName,
  value,
  onChange,
  readOnly,
}: {
  index: number;
  applicationId: string;
  environmentId: string;
  environmentName: string;
  value: RuntimeSecretReferenceDraft;
  onChange: (value: RuntimeSecretReferenceDraft) => void;
  readOnly: boolean;
}) {
  const bindings = useQuery({
    queryKey: [
      "runtime-secret-reference-bindings",
      applicationId,
      environmentId,
    ],
    queryFn: () => api.runtimeSecretBindings(applicationId, environmentId),
    retry: false,
    staleTime: 30_000,
  });
  const choices = (bindings.data?.items ?? []).filter((binding) =>
    selectableBinding(binding, applicationId, environmentId),
  );
  const selected = choices.find((binding) => binding.id === value.bindingId);
  const selectedKey =
    value.bindingId && value.version > 0
      ? referenceKey(value.bindingId, value.version)
      : "";
  const selectedIsCurrent =
    selected?.activeVersion === value.version && value.version > 0;
  const detail = useQuery({
    queryKey: [
      "runtime-secret-reference-binding",
      value.bindingId,
      selected?.activeVersion ?? 0,
      selected?.state ?? "",
    ],
    queryFn: () => api.runtimeSecretBinding(value.bindingId),
    enabled: Boolean(selected),
    retry: false,
    staleTime: 30_000,
  });
  const keys = activeEnvironmentKeys(
    detail.data,
    value,
    applicationId,
    environmentId,
    environmentName,
  );
  const bindingHint = bindings.error
    ? errorMessage(bindings.error)
    : bindings.isPending
      ? "Loading authorized ready bindings…"
      : choices.length
        ? "The stable ID and current active version come from runtime-secret metadata."
        : "No ready Sealed Secret binding is available in this application and environment.";
  const keyHint = !environmentName.trim()
    ? "Enter the environment variable name before choosing its authorized key."
    : detail.error
      ? errorMessage(detail.error)
      : detail.isPending && selected
        ? "Loading active delivery metadata…"
        : selected && !keys.length
          ? `The active version has no delivery authorized for ${environmentName.trim()}.`
          : "Only keys delivered to this exact environment variable are selectable.";

  return (
    <>
      <Field label={index === 0 ? "Binding" : ""} hint={bindingHint}>
        <Select
          aria-label={`Secret variable ${index + 1} binding`}
          value={selectedKey}
          disabled={readOnly || bindings.isPending || Boolean(bindings.error)}
          onChange={(event) => {
            const binding = choices.find(
              (candidate) =>
                referenceKey(candidate.id, candidate.activeVersion!) ===
                event.target.value,
            );
            onChange(
              binding
                ? {
                    bindingId: binding.id,
                    bindingName: binding.name,
                    key: "",
                    version: binding.activeVersion!,
                  }
                : { bindingId: "", bindingName: "", key: "", version: 0 },
            );
          }}
        >
          <option value="">Select ready binding</option>
          {value.bindingId && !selectedIsCurrent ? (
            <option value={selectedKey} disabled>
              {value.bindingName} · v{value.version || "?"} (unavailable)
            </option>
          ) : null}
          {choices.map((binding) => (
            <option
              key={referenceKey(binding.id, binding.activeVersion!)}
              value={referenceKey(binding.id, binding.activeVersion!)}
            >
              {binding.name} · v{binding.activeVersion}
            </option>
          ))}
        </Select>
      </Field>
      <Field label={index === 0 ? "Key" : ""} hint={keyHint}>
        <Select
          aria-label={`Secret variable ${index + 1} key`}
          value={keys.includes(value.key) ? value.key : ""}
          disabled={readOnly || !selected || detail.isPending || !keys.length}
          onChange={(event) => onChange({ ...value, key: event.target.value })}
        >
          <option value="">Select authorized key</option>
          {keys.map((key) => (
            <option key={key} value={key}>
              {key}
            </option>
          ))}
        </Select>
      </Field>
      <Field label={index === 0 ? "Version" : ""}>
        <output aria-label={`Secret variable ${index + 1} version`}>
          {value.version > 0 ? `v${value.version}` : "—"}
        </output>
      </Field>
    </>
  );
}

export function RuntimeSecretReferencePicker({
  index,
  applicationId,
  environmentId,
  environmentName,
  value,
  onChange,
  enabled,
  readOnly = false,
  unavailableReason,
}: {
  index: number;
  applicationId?: string;
  environmentId?: string;
  environmentName: string;
  value: RuntimeSecretReferenceDraft;
  onChange: (value: RuntimeSecretReferenceDraft) => void;
  enabled: boolean;
  readOnly?: boolean;
  unavailableReason?: string;
}) {
  if (!enabled || !applicationId || !environmentId) {
    return (
      <DisabledReferencePicker
        index={index}
        value={value}
        reason={
          unavailableReason ??
          "Select an existing application and environment with runtime-secret support."
        }
      />
    );
  }
  return (
    <ActiveReferencePicker
      index={index}
      applicationId={applicationId}
      environmentId={environmentId}
      environmentName={environmentName}
      value={value}
      onChange={onChange}
      readOnly={readOnly}
    />
  );
}
