import { describe, expect, it } from "vitest";
import type {
  Application,
  Capability,
  Deployment,
  Environment,
  Project,
} from "../api/types";
import { hasDeploymentConfigCapability } from "./configAccess";

const application: Application = {
  id: "application-payments",
  projectId: "project-payments",
  name: "Payments",
};
const deployment: Deployment = {
  id: "deployment-production",
  applicationId: application.id,
  environmentId: "environment-production",
  runtime: {
    replicas: 1,
    ports: [{ name: "http", containerPort: 3000 }],
    resources: { requests: { cpu: "50m", memory: "100Mi" } },
  },
};
const project: Project = {
  id: application.projectId,
  name: "Payments",
  teamId: "team-commerce",
};
const environment: Environment = {
  id: deployment.environmentId,
  projectId: application.projectId,
  name: "Production",
  namespace: "payments-production",
};

function capability(
  scopeType: Capability["scopeType"],
  scopeId: string,
): Capability {
  return {
    scopeType,
    scopeId,
    actions: ["deployment-config:write"],
  };
}

describe("deployment config capability boundary", () => {
  it("accepts only exact covering effective capabilities", () => {
    for (const value of [
      capability("platform", "platform"),
      capability("team", project.teamId!),
      capability("project", project.id),
      capability("environment", environment.id),
      capability("namespace", environment.namespace),
      capability("application", application.id),
    ]) {
      expect(
        hasDeploymentConfigCapability(
          [value],
          "deployment-config:write",
          application,
          deployment,
          project,
          environment,
        ),
      ).toBe(true);
    }
  });

  it("rejects broad summaries, sibling scopes, and missing team/namespace context", () => {
    expect(
      hasDeploymentConfigCapability(
        [],
        "deployment-config:write",
        application,
        deployment,
      ),
    ).toBe(false);
    for (const value of [
      capability("platform", "another-platform"),
      capability("team", "team-other"),
      capability("project", "project-other"),
      capability("environment", "environment-other"),
      capability("namespace", "other-production"),
      capability("application", "application-other"),
    ]) {
      expect(
        hasDeploymentConfigCapability(
          [value],
          "deployment-config:write",
          application,
          deployment,
          project,
          environment,
        ),
      ).toBe(false);
    }
    expect(
      hasDeploymentConfigCapability(
        [capability("team", project.teamId!)],
        "deployment-config:write",
        application,
        deployment,
      ),
    ).toBe(false);
    expect(
      hasDeploymentConfigCapability(
        [capability("namespace", environment.namespace)],
        "deployment-config:write",
        application,
        deployment,
      ),
    ).toBe(false);
  });
});
