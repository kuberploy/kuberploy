import { useQuery } from "@tanstack/react-query";
import { api, errorMessage } from "../api/client";
import type { BasicAuthConfig } from "../lib/traefikMiddleware";
import { Field } from "./ui";

const usersPath = "/var/run/secrets/kuberploy/traefik-basic-auth/users";

export function BasicAuthBindingPicker({
  applicationId,
  environmentId,
  value,
  onChange,
  readOnly = false,
}: {
  applicationId?: string;
  environmentId?: string;
  value: BasicAuthConfig["secretBindingRef"];
  onChange: (value: BasicAuthConfig["secretBindingRef"]) => void;
  readOnly?: boolean;
}) {
  const bindings = useQuery({
    queryKey: ["basic-auth-bindings", applicationId, environmentId],
    queryFn: () => api.runtimeSecretBindings(applicationId!, environmentId!),
    enabled: Boolean(applicationId) && Boolean(environmentId),
    retry: false,
  });
  const metadata = (bindings.data?.items ?? []).filter(
    (binding) =>
      binding.applicationId === applicationId &&
      binding.environmentId === environmentId &&
      binding.provider === "sealed-secrets" &&
      binding.state === "ready" &&
      Number.isSafeInteger(binding.activeVersion) &&
      (binding.activeVersion ?? 0) > 0,
  );
  const details = useQuery({
    queryKey: [
      "basic-auth-binding-details",
      ...metadata
        .map(
          ({ id, activeVersion, state }) =>
            `${id}@${activeVersion ?? 0}@${state}`,
        )
        .sort(),
    ],
    queryFn: () =>
      Promise.all(metadata.map(({ id }) => api.runtimeSecretBinding(id))),
    enabled: metadata.length > 0,
    retry: false,
  });
  const choices = (details.data ?? []).filter((detail) => {
    const currentMetadata = metadata.find(({ id }) => id === detail.id);
    if (
      !currentMetadata ||
      currentMetadata.state !== detail.state ||
      currentMetadata.activeVersion !== detail.activeVersion
    ) {
      return false;
    }
    const version = detail.versions.find(
      (candidate) =>
        candidate.number === detail.activeVersion &&
        candidate.state === "active",
    );
    return version?.deliveries.some(
      (delivery) =>
        delivery.kind === "file" &&
        delivery.sourceKey === "users" &&
        delivery.filePath === usersPath &&
        delivery.fileMode === 256,
    );
  });
  const selected = choices.find(
    (choice) =>
      choice.id === value.bindingId &&
      choice.name === value.name &&
      choice.activeVersion === value.version,
  );
  const unavailable = Boolean(value.bindingId) && !selected;
  const hint = bindings.error
    ? errorMessage(bindings.error)
    : details.error
      ? errorMessage(details.error)
      : "Only ready exact-scope bindings whose active version delivers key users as 0400 to the fixed BasicAuth runtime path are selectable.";

  return (
    <Field label="BasicAuth users binding" hint={hint}>
      <select
        aria-label="BasicAuth users binding"
        value={selected ? selected.id : ""}
        disabled={
          readOnly ||
          !applicationId ||
          !environmentId ||
          bindings.isPending ||
          details.isPending ||
          Boolean(bindings.error || details.error)
        }
        onChange={(event) => {
          const choice = choices.find(({ id }) => id === event.target.value);
          if (!choice?.activeVersion) return;
          onChange({
            bindingId: choice.id,
            name: choice.name,
            key: "users",
            version: choice.activeVersion,
          });
        }}
      >
        <option value="">
          {unavailable
            ? `${value.name || value.bindingId} · v${value.version} (unavailable)`
            : "Select exact users binding"}
        </option>
        {choices.map((choice) => (
          <option key={choice.id} value={choice.id}>
            {choice.name} · v{choice.activeVersion}
          </option>
        ))}
      </select>
    </Field>
  );
}
