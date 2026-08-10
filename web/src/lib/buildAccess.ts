import type { Application, Capability, Project } from "../api/types";

export type BuildApplicationAction =
  | "build-definitions:read"
  | "build-definitions:write"
  | "builds:read"
  | "builds:cancel"
  | "builds:retry"
  | "logs:read";

const applicationCoveringScopes = new Set([
  "platform",
  "team",
  "project",
  "application",
]);

export function hasPotentialBuildAccess(capabilities: Capability[]) {
  return capabilities.some(
    (capability) =>
      applicationCoveringScopes.has(capability.scopeType ?? "") &&
      (capability.scopeType !== "platform" ||
        capability.scopeId === "platform") &&
      Boolean(capability.scopeId) &&
      (capability.actions?.includes("build-definitions:read") === true ||
        capability.actions?.includes("builds:read") === true),
  );
}

export function hasBuildApplicationCapability(
  capabilities: Capability[],
  action: BuildApplicationAction,
  application: Application,
  project?: Project,
) {
  if (!project || project.id !== application.projectId) return false;
  return capabilities.some((capability) => {
    if (capability.actions?.includes(action) !== true) return false;
    switch (capability.scopeType) {
      case "platform":
        return capability.scopeId === "platform";
      case "team":
        return Boolean(project.teamId) && capability.scopeId === project.teamId;
      case "project":
        return capability.scopeId === project.id;
      case "application":
        return capability.scopeId === application.id;
      default:
        // Builds are application-scoped across environments. An environment or
        // namespace grant must never expand into source or registry authority.
        return false;
    }
  });
}

export function buildReadableApplications(
  capabilities: Capability[],
  applications: Application[],
  projects: Project[],
) {
  const projectsById = new Map(
    projects.map((project) => [project.id, project]),
  );
  return applications.filter((application) => {
    const project = projectsById.get(application.projectId);
    return (
      hasBuildApplicationCapability(
        capabilities,
        "build-definitions:read",
        application,
        project,
      ) &&
      hasBuildApplicationCapability(
        capabilities,
        "builds:read",
        application,
        project,
      )
    );
  });
}
