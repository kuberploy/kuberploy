import { describe, expect, it } from "vitest";
import type {
  Application,
  Capability,
  Environment,
  Project,
} from "../api/types";
import {
  hasRuntimeSecretCapability,
  runtimeSecretEnvironments,
} from "./runtimeSecretAccess";

const application: Application = {
  id: "application-payments",
  projectId: "project-payments",
  name: "Payments API",
};
const project: Project = {
  id: "project-payments",
  name: "Payments",
  teamId: "team-commerce",
};
const production: Environment = {
  id: "environment-production",
  projectId: project.id,
  name: "Production",
  namespace: "payments-production",
};
const staging: Environment = {
  id: "environment-staging",
  projectId: project.id,
  name: "Staging",
  namespace: "payments-staging",
};
const unrelated: Environment = {
  id: "environment-unrelated",
  projectId: "project-unrelated",
  name: "Unrelated",
  namespace: "unrelated-production",
};

function capability(
  action: string,
  scopeType: Capability["scopeType"],
  scopeId: string,
): Capability {
  return { actions: [action], scopeType, scopeId, role: "project-admin" };
}

describe("runtime-secret scoped actions", () => {
  it("matches every effective ancestor scope against the exact environment", () => {
    const cases: Capability[] = [
      capability("secret-bindings:read", "platform", "platform"),
      capability("secret-bindings:read", "team", project.teamId!),
      capability("secret-bindings:read", "project", project.id),
      capability("secret-bindings:read", "environment", production.id),
      capability("secret-bindings:read", "namespace", production.namespace),
      capability("secret-bindings:read", "application", application.id),
    ];

    for (const grant of cases) {
      expect(
        hasRuntimeSecretCapability(
          [grant],
          "secret-bindings:read",
          application,
          production,
          project,
        ),
      ).toBe(true);
    }
  });

  it("does not promote top-level actions, wrong scopes, or a different action", () => {
    const capabilities: Capability[] = [
      capability("secret-bindings:create", "environment", production.id),
      capability("secret-bindings:read", "environment", "other-environment"),
      capability("secret-bindings:read", "namespace", "other-namespace"),
      capability("secret-bindings:read", "project", "other-project"),
    ];

    expect(
      runtimeSecretEnvironments(
        capabilities,
        "secret-bindings:read",
        application,
        [production, staging, unrelated],
        project,
      ),
    ).toEqual([]);
  });

  it("returns only environments covered by the requested split action", () => {
    const capabilities = [
      capability("secret-bindings:read", "project", project.id),
      capability("secret-bindings:rotate", "environment", staging.id),
    ];

    expect(
      runtimeSecretEnvironments(
        capabilities,
        "secret-bindings:read",
        application,
        [production, staging, unrelated],
        project,
      ).map(({ id }) => id),
    ).toEqual([production.id, staging.id]);
    expect(
      runtimeSecretEnvironments(
        capabilities,
        "secret-bindings:rotate",
        application,
        [production, staging, unrelated],
        project,
      ).map(({ id }) => id),
    ).toEqual([staging.id]);
  });
});
