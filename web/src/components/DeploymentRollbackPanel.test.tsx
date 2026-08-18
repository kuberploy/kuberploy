import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  ConfigBundle,
  Deployment,
  DeploymentRollbackCandidate,
  Environment,
  Operation,
  Project,
} from "../api/types";
import { DeploymentRollbackPanel } from "./DeploymentRollbackPanel";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const application: Application = {
  id: "application-safe",
  projectId: "project-safe",
  name: "API",
};
const environment: Environment = {
  id: "environment-safe",
  projectId: application.projectId,
  name: "Production",
  namespace: "production",
};
const project: Project = {
  id: application.projectId,
  name: "Payments",
};
const deployment = {
  id: "deployment-safe",
  applicationId: application.id,
  environmentId: environment.id,
  runtime: {},
} as Deployment;
const candidates: DeploymentRollbackCandidate[] = [
  {
    sourceOperationId: "11111111-1111-4111-8111-111111111111",
    generation: 1,
    image: "registry.example.test/api@sha256:" + "a".repeat(64),
    artifactAssurance: "managed-release-verified",
    managedReleaseVerified: true,
    createdAt: "2026-08-16T00:00:00Z",
  },
  {
    sourceOperationId: "22222222-2222-4222-8222-222222222222",
    generation: 2,
    image: "registry.example.test/api@sha256:" + "b".repeat(64),
    artifactAssurance: "managed-release-verified",
    managedReleaseVerified: true,
    createdAt: "2026-08-16T00:01:00Z",
  },
];

function renderPanel() {
  vi.spyOn(api, "deploymentConfig").mockResolvedValue({
    kind: "ConfigBundle",
    etag: `"sha256:${"c".repeat(64)}"`,
    targetHeadRevision: "a".repeat(40),
    indexedRevision: "a".repeat(40),
    configRevision: "b".repeat(64),
    freshness: "fresh",
    documents: [],
  } as ConfigBundle);
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <DeploymentRollbackPanel
        deployment={deployment}
        application={application}
        environment={environment}
        project={project}
        capabilities={[
          {
            scopeType: "project",
            scopeId: project.id,
            actions: ["deployments:update"],
          },
        ]}
        featureEnabled
        humanSession
      />
    </QueryClientProvider>,
  );
}

describe("DeploymentRollbackPanel", () => {
  it("keeps a newer rollback candidate selected after an older request completes", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "deploymentRollbackSources").mockResolvedValue({
      items: candidates,
    });
    let resolveRollback!: (operation: Operation) => void;
    vi.spyOn(api, "rollbackDeployment").mockImplementation(
      () => new Promise((resolve) => (resolveRollback = resolve)),
    );
    renderPanel();

    await screen.findByText("Generation 1");
    await user.click(
      screen.getAllByRole("button", { name: "Select rollback" })[0]!,
    );
    await user.click(
      screen.getByRole("checkbox", {
        name: /I understand this creates a new Git intent/i,
      }),
    );
    await user.click(screen.getByRole("button", { name: "Confirm rollback" }));
    await waitFor(() => expect(api.rollbackDeployment).toHaveBeenCalledOnce());

    await user.click(
      screen.getAllByRole("button", { name: "Select rollback" })[1]!,
    );
    resolveRollback({ id: "operation-old-rollback" } as Operation);

    await waitFor(() =>
      expect(screen.getByText("Confirm generation 2")).toBeVisible(),
    );
  });
});
