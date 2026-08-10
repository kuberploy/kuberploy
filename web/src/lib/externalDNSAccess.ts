import type { Capability } from "../api/types";

export type ExternalDNSPlatformAction =
  "external-dns-integrations:read" | "external-dns-integrations:write";

export function hasExternalDNSPlatformCapability(
  capabilities: Capability[],
  action: ExternalDNSPlatformAction,
) {
  return capabilities.some(
    (capability) =>
      capability.role === "platform-admin" &&
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes(action) === true,
  );
}

export function externalDNSHostnameAllowed(
  hostname: string,
  suffixes: string[],
) {
  const host = hostname.trim().toLowerCase().replace(/\.$/, "");
  if (!host) return false;
  return suffixes.some((rawSuffix) => {
    const suffix = rawSuffix.trim().toLowerCase().replace(/\.$/, "");
    return Boolean(suffix) && (host === suffix || host.endsWith(`.${suffix}`));
  });
}
