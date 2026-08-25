import { describe, expect, it } from "vitest";
import type { Capability } from "../api/types";
import { hasHelmCapability } from "./helmAccess";

const application = {
  id: "application-payments",
  projectId: "project-payments",
  name: "Payments",
};
const environment = {
  id: "environment-production",
  projectId: "project-payments",
  name: "Production",
  namespace: "payments-production",
};
const project = {
  id: "project-payments",
  teamId: "team-commerce",
  name: "Payments",
};

describe("Helm effective access", () => {
  it("requires an exact covering capability instead of top-level actions", () => {
    expect(
      hasHelmCapability([], "helm.read", application, environment, project),
    ).toBe(false);
    expect(
      hasHelmCapability(
        [
          {
            scopeType: "environment",
            scopeId: environment.id,
            actions: ["helm.read"],
          },
        ],
        "helm.read",
        application,
        environment,
        project,
      ),
    ).toBe(true);
  });

  it("accepts the action names emitted by the capabilities API", () => {
    expect(
      hasHelmCapability(
        [
          {
            scopeType: "platform",
            scopeId: "platform",
            actions: ["helm-releases:read"],
          },
        ],
        "helm.read",
        application,
        environment,
        project,
      ),
    ).toBe(true);
    expect(
      hasHelmCapability(
        [
          {
            scopeType: "platform",
            scopeId: "platform",
            actions: ["helm-releases:deploy", "helm-releases:disable"],
          },
        ],
        "helm.deploy",
        application,
        environment,
        project,
      ),
    ).toBe(true);
  });

  it("rejects cross-project and unrelated namespace grants", () => {
    const capabilities: Capability[] = [
      {
        scopeType: "namespace",
        scopeId: "other-production",
        actions: ["helm.deploy"],
      },
      {
        scopeType: "project",
        scopeId: "other-project",
        actions: ["helm.deploy"],
      },
    ];
    expect(
      hasHelmCapability(
        capabilities,
        "helm.deploy",
        application,
        environment,
        project,
      ),
    ).toBe(false);
    expect(
      hasHelmCapability(
        [
          {
            scopeType: "platform",
            scopeId: "platform",
            actions: ["helm.deploy"],
          },
        ],
        "helm.deploy",
        application,
        { ...environment, projectId: "other-project" },
        project,
      ),
    ).toBe(false);
  });
});
