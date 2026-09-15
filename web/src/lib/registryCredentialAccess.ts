import type {
  Application,
  Capability,
  Environment,
  Project,
} from "../api/types";

export type RegistryCredentialAction =
  | "registry-credential-bindings:read"
  | "registry-credential-bindings:create"
  | "registry-credential-bindings:rotate"
  | "registry-credential-bindings:delete";

export function hasRegistryCredentialCapability(
  capabilities: Capability[],
  action: RegistryCredentialAction,
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

export function registryCredentialEnvironments(
  capabilities: Capability[],
  action: RegistryCredentialAction,
  application: Application,
  environments: Environment[],
  project?: Project,
) {
  return environments.filter(
    (environment) =>
      environment.projectId === application.projectId &&
      hasRegistryCredentialCapability(
        capabilities,
        action,
        application,
        environment,
        project,
      ),
  );
}
