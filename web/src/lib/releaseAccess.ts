import type { Capability } from "../api/types";

export type PlatformReleaseAction = "platform-releases:read";

export function hasPlatformReleaseCapability(
  capabilities: Capability[],
  action: PlatformReleaseAction,
) {
  return capabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes(action) === true,
  );
}
