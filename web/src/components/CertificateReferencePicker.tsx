import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import type { CertificateBindingReference } from "../api/types";
import { Select, Field, PlaceholderBadge } from "./ui";

function referenceKey(reference: CertificateBindingReference) {
  return `${reference.bindingId}@${reference.version}`;
}

export function CertificateReferencePicker({
  applicationId,
  environmentId,
  value,
  enabled,
  disabled,
  unavailableReason,
  onChange,
}: {
  applicationId?: string;
  environmentId?: string;
  value: CertificateBindingReference | null;
  enabled: boolean;
  disabled?: boolean;
  unavailableReason?: string;
  onChange: (value: CertificateBindingReference | null) => void;
}) {
  const bindings = useQuery({
    queryKey: ["certificate-bindings", applicationId, environmentId],
    queryFn: () => api.certificateBindings(applicationId!, environmentId!),
    enabled: enabled && Boolean(applicationId && environmentId),
    retry: false,
  });
  const ready = (bindings.data?.items ?? []).filter(
    (binding) =>
      binding.state === "ready" &&
      Number.isSafeInteger(binding.activeVersion) &&
      (binding.activeVersion ?? 0) > 0,
  );
  const selectedKey = value ? referenceKey(value) : "";
  const selectedIsListed = ready.some(
    (binding) =>
      binding.id === value?.bindingId &&
      binding.name === value.name &&
      binding.activeVersion === value.version,
  );

  return (
    <Field
      label="Certificate binding and version"
      hint="Only the reviewed binding identity is stored in Git. Kubernetes Secret names and private keys are never selectable."
    >
      <Select
        aria-label="Certificate binding and version"
        value={selectedKey}
        disabled={disabled || !enabled || bindings.isPending}
        onChange={(event) => {
          if (!event.target.value) {
            onChange(null);
            return;
          }
          const selected = ready.find(
            (binding) =>
              referenceKey({
                bindingId: binding.id,
                name: binding.name,
                version: binding.activeVersion!,
              }) === event.target.value,
          );
          if (selected) {
            onChange({
              bindingId: selected.id,
              name: selected.name,
              version: selected.activeVersion!,
            });
          }
        }}
      >
        <option value="">Choose a ready certificate…</option>
        {value && !selectedIsListed ? (
          <option value={selectedKey} disabled>
            Current YAML reference: {value.name} · v{value.version}
          </option>
        ) : null}
        {ready.map((binding) => (
          <option
            key={`${binding.id}-${binding.activeVersion}`}
            value={referenceKey({
              bindingId: binding.id,
              name: binding.name,
              version: binding.activeVersion!,
            })}
          >
            {binding.name} · v{binding.activeVersion}
          </option>
        ))}
      </Select>
      {!enabled ? (
        <small>
          {unavailableReason ?? "Certificate management is unavailable."}
        </small>
      ) : bindings.error ? (
        <small>
          Certificate metadata could not be loaded. The current YAML reference
          is preserved; use Advanced YAML to inspect it.
        </small>
      ) : bindings.data && ready.length === 0 ? (
        <PlaceholderBadge>No ready certificates</PlaceholderBadge>
      ) : null}
    </Field>
  );
}
