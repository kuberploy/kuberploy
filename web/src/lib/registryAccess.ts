import type { Application, Capability, Project } from "../api/types";

export type RegistryApplicationAction =
  | "registry:read"
  | "registry-policies:write"
  | "registry-cleanup:preview"
  | "registry-cleanup:execute";

export type RegistryPlatformAction =
  "registry-targets:read" | "registry-targets:write";

export function hasRegistryApplicationCapability(
  capabilities: Capability[],
  action: RegistryApplicationAction,
  application: Application,
  project?: Project,
) {
  if (project && project.id !== application.projectId) return false;
  return capabilities.some((capability) => {
    if (capability.actions?.includes(action) !== true) return false;
    switch (capability.scopeType) {
      case "platform":
        return capability.scopeId === "platform";
      case "team":
        return (
          Boolean(project?.teamId) && capability.scopeId === project?.teamId
        );
      case "project":
        return capability.scopeId === application.projectId;
      case "application":
        return capability.scopeId === application.id;
      default:
        // An application registry aggregates releases across environments, so
        // environment and namespace grants are intentionally insufficient.
        return false;
    }
  });
}

export function hasRegistryPlatformCapability(
  capabilities: Capability[],
  action: RegistryPlatformAction,
) {
  return capabilities.some(
    (capability) =>
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      capability.actions?.includes(action) === true,
  );
}
