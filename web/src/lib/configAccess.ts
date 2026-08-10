import type {
  Application,
  Capability,
  Deployment,
  Environment,
  Project,
} from "../api/types";

export type DeploymentConfigAction =
  "deployment-config:read" | "deployment-config:write";

export function hasDeploymentConfigCapability(
  capabilities: Capability[],
  action: DeploymentConfigAction,
  application: Application,
  deployment: Deployment,
  project?: Project,
  environment?: Environment,
) {
  if (
    application.id !== deployment.applicationId ||
    (project && project.id !== application.projectId) ||
    (environment &&
      (environment.id !== deployment.environmentId ||
        environment.projectId !== application.projectId))
  ) {
    return false;
  }
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
      case "environment":
        return capability.scopeId === deployment.environmentId;
      case "namespace":
        return (
          Boolean(environment?.namespace) &&
          capability.scopeId === environment?.namespace
        );
      case "application":
        return capability.scopeId === application.id;
      default:
        return false;
    }
  });
}
