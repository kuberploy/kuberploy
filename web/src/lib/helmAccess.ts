import type {
  Application,
  Capability,
  Environment,
  Project,
} from "../api/types";

export type HelmAction =
  "helm.read" | "helm.deploy" | "helm.retry" | "helm.rollback";

const apiActions: Record<HelmAction, readonly string[]> = {
  "helm.read": [
    "helm-approvals:read",
    "helm-releases:read",
    "helm-values:preview",
  ],
  "helm.deploy": ["helm-releases:deploy", "helm-releases:disable"],
  "helm.retry": ["helm-releases:retry"],
  "helm.rollback": ["helm-releases:rollback"],
};

export function hasHelmCapability(
  capabilities: Capability[],
  action: HelmAction,
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
    const actionAllowed =
      capability.actions?.includes(action) === true ||
      apiActions[action]?.every((item) =>
        capability.actions?.includes(item),
      ) === true;
    if (!actionAllowed) return false;
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
