import { describe, expect, it } from "vitest";
import type {
  Application,
  Capability,
  Environment,
  Project,
  ServiceAccount,
} from "../api/types";
import {
  canMutateAutoDeployPolicy,
  hasPotentialAutoDeployManagement,
} from "./autoDeployAccess";

const project = { id: "project-a", teamId: "team-a" } as Project;
const application = { id: "app-a", projectId: project.id } as Application;
const environment = {
  id: "env-a",
  projectId: project.id,
  namespace: "ns-a",
} as Environment;
const account = {
  id: "sa-a",
  projectId: project.id,
  role: "developer",
} as ServiceAccount;
const exact: Capability[] = [
  {
    scopeType: "application",
    scopeId: application.id,
    role: "project-admin",
    actions: ["app-sources:write"],
  },
  {
    scopeType: "environment",
    scopeId: environment.id,
    role: "developer",
    actions: ["deployments:update"],
  },
  {
    scopeType: "project",
    scopeId: project.id,
    role: "project-admin",
    actions: ["access-grants:create", "access-grants:delete"],
  },
];

describe("auto-deploy mutation visibility", () => {
  it("allows only the exact human build/composite/grant path", () => {
    expect(
      hasPotentialAutoDeployManagement(true, exact, application, project),
    ).toBe(true);
    expect(
      hasPotentialAutoDeployManagement(false, exact, application, project),
    ).toBe(false);
    expect(
      hasPotentialAutoDeployManagement(
        true,
        exact.slice(1),
        application,
        project,
      ),
    ).toBe(false);
    expect(
      canMutateAutoDeployPolicy(
        true,
        exact,
        application,
        environment,
        project,
        account,
      ),
    ).toBe(true);
    expect(
      canMutateAutoDeployPolicy(
        false,
        exact,
        application,
        environment,
        project,
        account,
      ),
    ).toBe(false);
    expect(
      canMutateAutoDeployPolicy(
        true,
        exact.slice(1),
        application,
        environment,
        project,
        account,
      ),
    ).toBe(false);
  });
  it("rejects wrong environment and wrong project/team grant authority", () => {
    const wrongEnvironment = exact.map((item) =>
      item.scopeType === "environment" ? { ...item, scopeId: "env-b" } : item,
    );
    const wrongGrant = exact.map((item) =>
      item.scopeType === "project"
        ? { ...item, scopeType: "team" as const, scopeId: "team-b" }
        : item,
    );
    expect(
      canMutateAutoDeployPolicy(
        true,
        wrongEnvironment,
        application,
        environment,
        project,
        account,
      ),
    ).toBe(false);
    expect(
      canMutateAutoDeployPolicy(
        true,
        wrongGrant,
        application,
        environment,
        project,
        account,
      ),
    ).toBe(false);
  });
  it("rejects disabled, foreign, and stronger service accounts", () => {
    expect(
      canMutateAutoDeployPolicy(
        true,
        exact,
        application,
        environment,
        project,
        { ...account, disabledAt: new Date().toISOString() },
      ),
    ).toBe(false);
    expect(
      canMutateAutoDeployPolicy(
        true,
        exact,
        application,
        environment,
        project,
        { ...account, projectId: "project-b" },
      ),
    ).toBe(false);
    const underpoweredGrant = exact.map((item) =>
      item.scopeType === "project"
        ? { ...item, role: "developer" as const }
        : item,
    );
    expect(
      canMutateAutoDeployPolicy(
        true,
        underpoweredGrant,
        application,
        environment,
        project,
        { ...account, role: "project-admin" },
      ),
    ).toBe(false);
  });
});
