import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { Field, PlaceholderBadge } from "./ui";

export function CertificateIssuerPicker({
  applicationId,
  environmentId,
  hostname,
  value,
  enabled,
  disabled,
  unavailableReason,
  onChange,
}: {
  applicationId?: string;
  environmentId?: string;
  hostname?: string;
  value: string;
  enabled: boolean;
  disabled?: boolean;
  unavailableReason?: string;
  onChange: (value: string) => void;
}) {
  const scoped = Boolean(applicationId && environmentId && hostname);
  const catalog = useQuery({
    queryKey: ["certificate-issuers", applicationId, environmentId, hostname],
    queryFn: () =>
      api.applicationCertificateIssuers(
        applicationId!,
        environmentId!,
        hostname!,
      ),
    enabled: enabled && scoped,
    retry: false,
  });
  const items = catalog.data?.items ?? [];
  const selectedIsListed = items.some((item) => item.name === value);

  return (
    <Field
      label="Approved certificate issuer"
      hint="Platform administrators own ACME accounts and solver credentials. Only a freshly observed issuer valid for this hostname can be selected."
    >
      <select
        aria-label="Approved certificate issuer"
        value={value}
        disabled={disabled || !enabled || !scoped || catalog.isPending}
        onChange={(event) => onChange(event.target.value)}
      >
        <option value="">Choose an approved issuer…</option>
        {value && !selectedIsListed ? (
          <option value={value} disabled>
            Current YAML issuer: {value}
          </option>
        ) : null}
        {items.map((item) => (
          <option key={item.name} value={item.name}>
            {item.name} · {item.environment} · {item.solverTypes.join(" + ")}
          </option>
        ))}
      </select>
      {!enabled ? (
        <small>
          {unavailableReason ?? "Certificate issuers are unavailable."}
        </small>
      ) : !scoped ? (
        <small>
          Choose an existing application, environment, and hostname first.
        </small>
      ) : catalog.error ? (
        <small>
          The approved issuer catalog is unavailable. The current YAML value is
          preserved but cannot be newly selected.
        </small>
      ) : catalog.data && items.length === 0 ? (
        <PlaceholderBadge>No issuer covers this hostname</PlaceholderBadge>
      ) : null}
    </Field>
  );
}
