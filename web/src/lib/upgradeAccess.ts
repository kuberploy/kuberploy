import type { Capability } from "../api/types";

export type PlatformUpgradeAction = "platform-releases:read";

export function hasPlatformUpgradeCapability(
  capabilities: Capability[],
  action: PlatformUpgradeAction,
) {
  return capabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes(action) === true,
  );
}
