import { describe, expect, it } from "vitest";
import type { Capability, Environment, Project } from "../api/types";
import {
  hasGlobalMetricsAccess,
  hasMonitoringNavigationAccess,
  monitoringEnvironments,
} from "./monitoringAccess";

const projects: Project[] = [
  { id: "project-payments", name: "Payments", teamId: "team-commerce" },
  { id: "project-search", name: "Search", teamId: "team-discovery" },
];

const environments: Environment[] = [
  {
    id: "environment-production",
    projectId: "project-payments",
    name: "Production",
    namespace: "payments-production",
  },
  {
    id: "environment-staging",
    projectId: "project-payments",
    name: "Staging",
    namespace: "payments-staging",
  },
  {
    id: "environment-search",
    projectId: "project-search",
    name: "Production",
    namespace: "search-production",
  },
];

function capability(
  scopeType: Capability["scopeType"],
  scopeId: string,
  role: Capability["role"] = "viewer",
): Capability {
  return { scopeType, scopeId, role, actions: ["metrics:read"] };
}

describe("monitoring scope access", () => {
  it("maps only exact effective team, project, environment, and namespace grants", () => {
    expect(
      monitoringEnvironments(
        [
          capability("team", "team-commerce"),
          capability("environment", "environment-search"),
        ],
        projects,
        environments,
      ).map((environment) => environment.id),
    ).toEqual([
      "environment-production",
      "environment-staging",
      "environment-search",
    ]);

    expect(
      monitoringEnvironments(
        [capability("namespace", "payments-staging")],
        projects,
        environments,
      ).map((environment) => environment.id),
    ).toEqual(["environment-staging"]);
  });

  it("does not infer scope from coarse actions, feature flags, or application grants", () => {
    const unscoped: Capability[] = [
      { actions: ["metrics:read"] },
      capability("application", "application-payments"),
      capability("platform", "not-platform"),
    ];

    expect(monitoringEnvironments(unscoped, projects, environments)).toEqual(
      [],
    );
    expect(hasMonitoringNavigationAccess(unscoped)).toBe(false);
  });

  it("requires the explicit platform-admin capability for global metrics", () => {
    const platformViewer = capability("platform", "platform", "viewer");
    const platformAdmin = capability("platform", "platform", "platform-admin");

    expect(hasGlobalMetricsAccess([platformViewer])).toBe(false);
    expect(hasGlobalMetricsAccess([platformAdmin])).toBe(true);
    expect(
      monitoringEnvironments([platformViewer], projects, environments),
    ).toHaveLength(3);
  });
});
