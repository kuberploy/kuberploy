import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  AutoDeployPolicy,
  BuildDefinition,
  Capability,
  Project,
} from "../api/types";
import { AutoDeployPoliciesPanel } from "./AutoDeployPoliciesPanel";

const application: Application = {
  id: "application-reader",
  projectId: "project-reader",
  name: "Reader app",
};
const project: Project = {
  id: application.projectId,
  name: "Reader project",
  teamId: "team-reader",
};
const definition = {
  id: "definition-reader",
  applicationId: application.id,
  projectId: project.id,
  triggerRef: "refs/heads/main",
  definitionDigest: `sha256:${"a".repeat(64)}`,
} as BuildDefinition;
const policy: AutoDeployPolicy = {
  id: "policy-reader",
  buildDefinitionId: definition.id,
  projectId: project.id,
  applicationId: application.id,
  environmentId: "environment-reader",
  currentRevision: 2,
  current: {
    revision: 2,
    enabled: true,
    sourceDeploymentId: "deployment-reader",
    sourceDeploymentGeneration: 4,
    sourceConfigETag: `"sha256:${"b".repeat(64)}"`,
    templateDigest: `sha256:${"c".repeat(64)}`,
    serviceActorId: "service-account-reader",
    createdBy: "user-reader",
    createdAt: "2026-08-09T00:00:00Z",
  },
  createdBy: "user-reader",
  createdAt: "2026-08-09T00:00:00Z",
  updateSemantics: "Config drift pauses automation until a new revision.",
};
const readOnlyCapabilities: Capability[] = [
  {
    scopeType: "application",
    scopeId: application.id,
    role: "viewer",
    actions: ["build-definitions:read", "builds:read"],
  },
];

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("auto-deploy policy read-only access", () => {
  it("loads policy history without querying mutation catalogs or rendering controls", async () => {
    const policies = vi
      .spyOn(api, "autoDeployPolicies")
      .mockResolvedValue({ items: [policy] });
    const revisions = vi
      .spyOn(api, "autoDeployPolicyRevisions")
      .mockResolvedValue({ items: [policy.current] });
    const runs = vi.spyOn(api, "autoDeployPolicyRuns").mockResolvedValue({
      items: [
        {
          attemptId: "attempt-reader",
          policyRevision: policy.currentRevision,
          releaseId: "release-reader",
          state: "submitted",
          attempts: 1,
          operationId: "operation-reader",
          deploymentId: "deployment-result-reader",
          availableAt: "2026-08-09T00:01:00Z",
          createdAt: "2026-08-09T00:01:00Z",
          updatedAt: "2026-08-09T00:02:00Z",
          completedAt: "2026-08-09T00:02:00Z",
        },
      ],
    });
    const deployments = vi.spyOn(api, "deployments");
    const environments = vi.spyOn(api, "environments");
    const serviceAccounts = vi.spyOn(api, "serviceAccounts");
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <AutoDeployPoliciesPanel
          application={application}
          project={project}
          definitions={[definition]}
          enabled
          humanSession
          capabilities={readOnlyCapabilities}
        />
      </QueryClientProvider>,
    );

    expect(await screen.findByText(/Revision 2/)).toBeInTheDocument();
    expect(
      await screen.findByText(/attempt attempt-read/i),
    ).toBeInTheDocument();
    await waitFor(() => {
      expect(policies).toHaveBeenCalledWith(application.id);
      expect(revisions).toHaveBeenCalledWith(policy.id);
      expect(runs).toHaveBeenCalledWith(policy.id);
    });
    expect(deployments).not.toHaveBeenCalled();
    expect(environments).not.toHaveBeenCalled();
    expect(serviceAccounts).not.toHaveBeenCalled();
    expect(
      screen.queryByRole("button", { name: "Enable pinned policy" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Disable" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Repin current config" }),
    ).not.toBeInTheDocument();
  });

  it("confirms disable and reuses the idempotency key after an ambiguous failure", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "autoDeployPolicies").mockResolvedValue({ items: [policy] });
    vi.spyOn(api, "autoDeployPolicyRevisions").mockResolvedValue({
      items: [policy.current],
    });
    vi.spyOn(api, "autoDeployPolicyRuns").mockResolvedValue({ items: [] });
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [
        {
          id: policy.current.sourceDeploymentId,
          applicationId: application.id,
          projectId: project.id,
          environmentId: policy.environmentId,
          name: "Production",
        },
      ],
    } as never);
    vi.spyOn(api, "environments").mockResolvedValue({
      items: [
        {
          id: policy.environmentId,
          projectId: project.id,
          name: "Production",
          namespace: "production",
        },
      ],
    });
    vi.spyOn(api, "serviceAccounts").mockResolvedValue({
      items: [
        {
          id: policy.current.serviceActorId,
          projectId: project.id,
          name: "deployer",
          role: "developer",
          createdBy: "user-admin",
          createdAt: "2026-08-09T00:00:00Z",
        },
      ],
    });
    const revise = vi
      .spyOn(api, "reviseAutoDeployPolicy")
      .mockRejectedValueOnce(new Error("response lost"))
      .mockResolvedValue(policy);
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <AutoDeployPoliciesPanel
          application={application}
          project={project}
          definitions={[definition]}
          enabled
          humanSession
          capabilities={[
            {
              scopeType: "application",
              scopeId: application.id,
              role: "project-admin",
              actions: ["build-definitions:write"],
            },
            {
              scopeType: "environment",
              scopeId: policy.environmentId,
              role: "developer",
              actions: ["deployments:update"],
            },
            {
              scopeType: "project",
              scopeId: project.id,
              role: "project-admin",
              actions: ["access-grants:create", "access-grants:delete"],
            },
          ]}
        />
      </QueryClientProvider>,
    );

    const disable = await screen.findByRole("button", { name: "Disable" });
    await user.click(disable);
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining(policy.id));
    expect(revise).not.toHaveBeenCalled();

    confirm.mockReturnValue(true);
    await user.click(disable);
    await waitFor(() => expect(revise).toHaveBeenCalledTimes(1));
    await user.click(disable);
    await waitFor(() => expect(revise).toHaveBeenCalledTimes(2));
    expect(revise.mock.calls[1]?.[2]).toBe(revise.mock.calls[0]?.[2]);
  });
});
