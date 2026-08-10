import { describe, expect, it } from "vitest";
import type { Application, Capability, Project } from "../api/types";
import {
  hasRegistryApplicationCapability,
  hasRegistryPlatformCapability,
} from "./registryAccess";

const application: Application = {
  id: "application-a",
  projectId: "project-a",
  name: "API",
};
const project: Project = {
  id: "project-a",
  teamId: "team-a",
  name: "Payments",
};
const capability = (
  action: string,
  scopeType: Capability["scopeType"],
  scopeId: string,
): Capability => ({ actions: [action], scopeType, scopeId });

describe("registry capability matching", () => {
  it("accepts only effective application ancestry", () => {
    for (const grant of [
      capability("registry:read", "platform", "platform"),
      capability("registry:read", "team", "team-a"),
      capability("registry:read", "project", "project-a"),
      capability("registry:read", "application", "application-a"),
    ]) {
      expect(
        hasRegistryApplicationCapability(
          [grant],
          "registry:read",
          application,
          project,
        ),
      ).toBe(true);
    }
  });

  it("rejects environment, namespace, unrelated, and broad action claims", () => {
    for (const grant of [
      capability("registry:read", "environment", "environment-a"),
      capability("registry:read", "namespace", "namespace-a"),
      capability("registry:read", "project", "project-b"),
      capability("registry-policies:write", "application", "application-a"),
    ]) {
      expect(
        hasRegistryApplicationCapability(
          [grant],
          "registry:read",
          application,
          project,
        ),
      ).toBe(false);
    }
    expect(
      hasRegistryApplicationCapability(
        [],
        "registry:read",
        application,
        project,
      ),
    ).toBe(false);
  });

  it("requires the exact platform target capability", () => {
    expect(
      hasRegistryPlatformCapability(
        [capability("registry-targets:read", "platform", "platform")],
        "registry-targets:read",
      ),
    ).toBe(true);
    expect(
      hasRegistryPlatformCapability(
        [capability("registry-targets:read", "project", "project-a")],
        "registry-targets:read",
      ),
    ).toBe(false);
  });
});
