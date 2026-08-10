import { describe, expect, it } from "vitest";
import type { Application, Capability, Project } from "../api/types";
import {
  buildReadableApplications,
  hasBuildApplicationCapability,
  hasPotentialBuildAccess,
} from "./buildAccess";

const project: Project = {
  id: "project-payments",
  name: "Payments",
  teamId: "team-commerce",
};
const application: Application = {
  id: "application-api",
  projectId: project.id,
  name: "API",
};

function capability(
  scopeType: Capability["scopeType"],
  scopeId: string,
  actions: string[],
): Capability {
  return { scopeType, scopeId, actions };
}

describe("source-build access", () => {
  it("requires an exact resource ancestry and never trusts coarse unions", () => {
    const grants = [
      capability("team", "different-team", ["builds:read"]),
      capability("project", "different-project", ["build-definitions:read"]),
      capability("application", "different-application", ["builds:read"]),
    ];

    expect(
      hasBuildApplicationCapability(
        grants,
        "builds:read",
        application,
        project,
      ),
    ).toBe(false);
    expect(buildReadableApplications(grants, [application], [project])).toEqual(
      [],
    );
  });

  it("rejects environment and namespace grants for application-wide builds", () => {
    for (const scopeType of ["environment", "namespace"] as const) {
      const grants = [
        capability(scopeType, "opaque-runtime-scope", [
          "build-definitions:read",
          "build-definitions:write",
          "builds:read",
          "builds:cancel",
          "builds:retry",
        ]),
      ];
      expect(
        hasBuildApplicationCapability(
          grants,
          "build-definitions:write",
          application,
          project,
        ),
      ).toBe(false);
      expect(hasPotentialBuildAccess(grants)).toBe(false);
    }
  });

  it("accepts exact platform, team, project, and application coverage", () => {
    const actions = ["build-definitions:read", "builds:read"];
    const grants = [
      capability("team", project.teamId ?? "", actions),
      capability("project", project.id, actions),
      capability("application", application.id, actions),
    ];
    expect(buildReadableApplications(grants, [application], [project])).toEqual(
      [application],
    );
    expect(hasPotentialBuildAccess(grants)).toBe(true);
    expect(
      hasBuildApplicationCapability(
        [capability("platform", "not-platform", actions)],
        "builds:read",
        application,
        project,
      ),
    ).toBe(false);
    expect(
      hasBuildApplicationCapability(
        [capability("platform", "platform", actions)],
        "builds:read",
        application,
        project,
      ),
    ).toBe(true);
  });
});
