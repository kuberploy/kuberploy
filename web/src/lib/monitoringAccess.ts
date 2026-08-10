import type { Capability, Environment, Project } from "../api/types";

const namespaceCoveringScopes = new Set([
  "platform",
  "team",
  "project",
  "environment",
  "namespace",
]);

function canReadMetrics(capability: Capability) {
  return capability.actions?.includes("metrics:read") === true;
}

export function hasPotentialNamespaceMetricsAccess(capabilities: Capability[]) {
  return capabilities.some((capability) => {
    if (
      !canReadMetrics(capability) ||
      !namespaceCoveringScopes.has(capability.scopeType ?? "")
    ) {
      return false;
    }
    return capability.scopeType === "platform"
      ? capability.scopeId === "platform"
      : Boolean(capability.scopeId);
  });
}

export function hasGlobalMetricsAccess(capabilities: Capability[]) {
  return capabilities.some(
    (capability) =>
      capability.role === "platform-admin" &&
      capability.scopeType === "platform" &&
      capability.scopeId === "platform" &&
      canReadMetrics(capability),
  );
}

export function monitoringEnvironments(
  capabilities: Capability[],
  projects: Project[],
  environments: Environment[],
) {
  const projectsById = new Map(
    projects.map((project) => [project.id, project]),
  );
  return environments.filter((environment) => {
    const project = projectsById.get(environment.projectId);
    if (!project) return false;
    return capabilities.some((capability) => {
      if (!canReadMetrics(capability)) return false;
      switch (capability.scopeType) {
        case "platform":
          return capability.scopeId === "platform";
        case "team":
          return (
            Boolean(project.teamId) && capability.scopeId === project.teamId
          );
        case "project":
          return capability.scopeId === project.id;
        case "environment":
          return capability.scopeId === environment.id;
        case "namespace":
          return capability.scopeId === environment.namespace;
        default:
          return false;
      }
    });
  });
}

export function hasMonitoringNavigationAccess(capabilities: Capability[]) {
  return (
    hasGlobalMetricsAccess(capabilities) ||
    hasPotentialNamespaceMetricsAccess(capabilities)
  );
}
