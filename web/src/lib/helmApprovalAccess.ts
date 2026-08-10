import type { Capability, Principal } from "../api/types";

export function hasHelmApprovalManagementAccess(
  principal: Principal | undefined,
  features: Record<string, boolean> | undefined,
  capabilities: Capability[],
) {
  if (
    principal?.authentication.kind !== "session" ||
    features?.helmApprovals !== true
  )
    return false;
  return capabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes("helm-approvals:manage") === true,
  );
}
