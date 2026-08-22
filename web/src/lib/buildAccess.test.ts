import { describe, expect, it } from "vitest";
import type { Application, Capability, Project } from "../api/types";
import {
  buildReadableApplications,
  canonicalBuildRepository,
  compatibleBuildRegistryTargets,
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

  it("offers only policies bound to the canonical build repository", () => {
    const target = {
      id: "target-managed",
      name: "Managed",
      mode: "managed" as const,
      endpoint: "https://registry.example.test",
      repositoryPrefix: "tenant",
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:00:00Z",
    };
    const repository = canonicalBuildRepository(
      target,
      project.id,
      application.id,
    );
    const item = {
      target,
      policy: {
        registryTargetId: target.id,
        serviceId: application.id,
        repository,
        keepLastSuccessful: 10,
        minimumSafetyAgeSeconds: 86_400,
        cacheKeepGenerations: 2,
        cacheUnusedExpirySeconds: 604_800,
        cacheByteQuota: 10_737_418_240,
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      },
      catalogObservations: [],
      catalogTruncated: false,
      releases: [],
      releasesTruncated: false,
      cacheGenerations: [],
      cacheGenerationsTruncated: false,
      observedAt: "2026-08-09T00:00:00Z",
    };

    expect(
      compatibleBuildRegistryTargets([item], project.id, application.id),
    ).toEqual([target]);
    expect(
      compatibleBuildRegistryTargets(
        [{ ...item, policy: { ...item.policy, repository: "tenant/other" } }],
        project.id,
        application.id,
      ),
    ).toEqual([]);
  });
});
