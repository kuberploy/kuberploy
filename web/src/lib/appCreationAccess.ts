import type {
  Capabilities,
  Capability,
  Environment,
  Project,
} from "../api/types";

export type AppSourceKind = "oci" | "github" | "git-ssh" | "helm";

function coversProject(capability: Capability, project: Project) {
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

function coversEnvironment(
  capability: Capability,
  project: Project,
  environment: Environment,
) {
  if (coversProject(capability, project)) return true;
  if (capability.scopeType === "environment") {
    return capability.scopeId === environment.id;
  }
  return (
    capability.scopeType === "namespace" &&
    capability.scopeId === environment.namespace
  );
}

function grants(
  capabilities: Capabilities | undefined,
  actions: readonly string[],
  covers: (capability: Capability) => boolean,
) {
  return (capabilities?.capabilities ?? []).some(
    (capability) =>
      covers(capability) &&
      actions.every((action) => capability.actions?.includes(action) === true),
  );
}

export function canCreateAppIdentity(
  capabilities: Capabilities | undefined,
  project: Project,
) {
  return grants(capabilities, ["applications:create"], (capability) =>
    coversProject(capability, project),
  );
}

export function canDeleteApplication(
  capabilities: Capabilities | undefined,
  project: Project,
) {
  return grants(capabilities, ["applications:delete"], (capability) =>
    coversProject(capability, project),
  );
}

export function canDeleteEnvironment(
  capabilities: Capabilities | undefined,
  project: Project,
) {
  return grants(capabilities, ["environments:delete"], (capability) =>
    coversProject(capability, project),
  );
}

export function canUseAppSource(
  source: AppSourceKind,
  capabilities: Capabilities | undefined,
  project: Project,
  environment: Environment,
) {
  switch (source) {
    case "oci":
      return grants(capabilities, ["deployments:create"], (capability) =>
        coversEnvironment(capability, project, environment),
      );
    case "github":
      return (
        capabilities?.features?.githubAppSetup === true &&
        grants(capabilities, ["build-definitions:write"], (capability) =>
          coversProject(capability, project),
        )
      );
    case "git-ssh":
      return (
        capabilities?.features?.gitSSH === true &&
        grants(capabilities, ["build-definitions:write"], (capability) =>
          coversProject(capability, project),
        )
      );
    case "helm":
      return (
        capabilities?.features?.helmDeployments === true &&
        grants(
          capabilities,
          ["helm-releases:deploy", "helm-releases:disable"],
          (capability) => coversEnvironment(capability, project, environment),
        )
      );
  }
}

export function canCreateAppInEnvironment(
  capabilities: Capabilities | undefined,
  project: Project,
  environment: Environment,
) {
  return (
    canCreateAppIdentity(capabilities, project) &&
    (["oci", "github", "git-ssh", "helm"] as const).some((source) =>
      canUseAppSource(source, capabilities, project, environment),
    )
  );
}
