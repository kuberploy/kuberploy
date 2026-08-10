import type {
  AccessRole,
  Application,
  Capability,
  Environment,
  Project,
  ServiceAccount,
} from "../api/types";

const roleRank: Record<AccessRole, number> = {
  viewer: 10,
  developer: 20,
  "project-admin": 30,
  "organization-admin": 40,
  "platform-admin": 50,
};

function coversApplication(
  capability: Capability,
  application: Application,
  project: Project,
) {
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
      return false;
  }
}

function coversDeployment(
  capability: Capability,
  application: Application,
  environment: Environment,
  project: Project,
) {
  if (
    application.projectId !== project.id ||
    environment.projectId !== project.id
  )
    return false;
  switch (capability.scopeType) {
    case "platform":
      return capability.scopeId === "platform";
    case "team":
      return Boolean(project.teamId) && capability.scopeId === project.teamId;
    case "project":
      return capability.scopeId === project.id;
    case "environment":
      return capability.scopeId === environment.id;
    case "namespace":
      return capability.scopeId === environment.namespace;
    case "application":
      return capability.scopeId === application.id;
    default:
      return false;
  }
}

function canManageAccount(
  capability: Capability,
  account: ServiceAccount,
  project: Project,
) {
  if (
    account.projectId !== project.id ||
    account.disabledAt ||
    !capability.role ||
    !capability.actions?.includes("access-grants:create") ||
    !capability.actions.includes("access-grants:delete") ||
    roleRank[capability.role] < roleRank[account.role]
  )
    return false;
  switch (capability.scopeType) {
    case "platform":
      return capability.scopeId === "platform";
    case "team":
      return Boolean(project.teamId) && capability.scopeId === project.teamId;
    case "project":
      return capability.scopeId === project.id;
    default:
      return false;
  }
}

export function canMutateAutoDeployPolicy(
  humanSession: boolean,
  capabilities: Capability[],
  application: Application,
  environment: Environment,
  project: Project,
  account: ServiceAccount,
) {
  if (!humanSession) return false;
  const builds = capabilities.some(
    (capability) =>
      capability.actions?.includes("build-definitions:write") &&
      coversApplication(capability, application, project),
  );
  const deployment = capabilities.some(
    (capability) =>
      capability.actions?.includes("deployments:update") &&
      coversDeployment(capability, application, environment, project),
  );
  const grant = capabilities.some((capability) =>
    canManageAccount(capability, account, project),
  );
  return builds && deployment && grant;
}

export function hasPotentialAutoDeployManagement(
  humanSession: boolean,
  capabilities: Capability[],
  application: Application,
  project: Project,
) {
  if (!humanSession || application.projectId !== project.id) return false;
  const builds = capabilities.some(
    (capability) =>
      capability.actions?.includes("build-definitions:write") &&
      coversApplication(capability, application, project),
  );
  const grants = capabilities.some(
    (capability) =>
      capability.actions?.includes("access-grants:create") &&
      capability.actions.includes("access-grants:delete") &&
      capability.role !== undefined &&
      (
        ["platform", "team", "project"] as Array<Capability["scopeType"]>
      ).includes(capability.scopeType) &&
      ((capability.scopeType === "platform" &&
        capability.scopeId === "platform") ||
        (capability.scopeType === "team" &&
          Boolean(project.teamId) &&
          capability.scopeId === project.teamId) ||
        (capability.scopeType === "project" &&
          capability.scopeId === project.id)),
  );
  return builds && grants;
}
