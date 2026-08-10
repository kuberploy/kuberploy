import { describe, expect, it } from "vitest";
import type { Capability } from "../api/types";
import { hasDeploymentRollbackCapability } from "./deploymentAccess";

const application = { id: "app-a", projectId: "project-a", name: "API" };
const environment = {
  id: "env-a",
  projectId: "project-a",
  name: "Production",
  namespace: "team-production",
};
const project = { id: "project-a", name: "Team", teamId: "team-a" };

function grant(
  scopeType: Capability["scopeType"],
  scopeId: string,
): Capability {
  return { scopeType, scopeId, actions: ["deployments:update"] };
}

describe("deployment rollback access", () => {
  it.each([
    grant("platform", "platform"),
    grant("team", "team-a"),
    grant("project", "project-a"),
    grant("environment", "env-a"),
    grant("namespace", "team-production"),
    grant("application", "app-a"),
  ])("accepts an exact covering capability", (capability) => {
    expect(
      hasDeploymentRollbackCapability(
        [capability],
        application,
        environment,
        project,
      ),
    ).toBe(true);
  });

  it("rejects unrelated scope, wrong action, and cross-project resources", () => {
    expect(
      hasDeploymentRollbackCapability(
        [grant("environment", "env-b")],
        application,
        environment,
        project,
      ),
    ).toBe(false);
    expect(
      hasDeploymentRollbackCapability(
        [{ ...grant("environment", "env-a"), actions: ["deployments:read"] }],
        application,
        environment,
        project,
      ),
    ).toBe(false);
    expect(
      hasDeploymentRollbackCapability(
        [grant("platform", "platform")],
        application,
        { ...environment, projectId: "project-b" },
        project,
      ),
    ).toBe(false);
  });
});
