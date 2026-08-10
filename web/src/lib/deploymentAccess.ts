import type {
  Application,
  Capability,
  Environment,
  Project,
} from "../api/types";

export function hasDeploymentRollbackCapability(
  capabilities: Capability[],
  application: Application,
  environment: Environment,
  project?: Project,
) {
  if (
    environment.projectId !== application.projectId ||
    (project && project.id !== application.projectId)
  ) {
    return false;
  }
  return capabilities.some((capability) => {
    if (capability.actions?.includes("deployments:update") !== true)
      return false;
    switch (capability.scopeType) {
      case "platform":
        return capability.scopeId === "platform";
      case "team":
        return (
          project !== undefined &&
          project.teamId !== undefined &&
          project.teamId !== "" &&
          capability.scopeId === project.teamId
        );
      case "project":
        return capability.scopeId === application.projectId;
      case "environment":
        return capability.scopeId === environment.id;
      case "namespace":
        return capability.scopeId === environment.namespace;
      case "application":
        return capability.scopeId === application.id;
      default:
        return false;
    }
  });
}
