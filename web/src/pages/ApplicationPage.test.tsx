import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Capabilities } from "../api/types";
import { ApplicationPage } from "./ApplicationPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: PropsWithChildren<{ to: string }>) => (
    <a href={to}>{children}</a>
  ),
  useParams: () => ({
    applicationId: "application-payments",
    deploymentId: "deployment-production",
  }),
}));

beforeEach(() => {
  vi.spyOn(api, "application").mockResolvedValue({
    id: "application-payments",
    projectId: "project-payments",
    name: "Payments API",
  });
  vi.spyOn(api, "deployment").mockResolvedValue({
    id: "deployment-production",
    applicationId: "application-payments",
    environmentId: "environment-production",
    image: `ghcr.io/acme/payments@sha256:${"a".repeat(64)}`,
    runtime: {
      replicas: 1,
      ports: [{ name: "http", containerPort: 3000 }],
      resources: { requests: { cpu: "50m", memory: "100Mi" } },
    },
    state: "healthy",
  });
  vi.spyOn(api, "deploymentStatus").mockResolvedValue({
    state: "git-committed",
    operationStatus: "succeeded",
    argoSyncStatus: "synced",
    rolloutHealth: "healthy",
    desiredRevision: "a".repeat(40),
    argoObservedRevision: "a".repeat(40),
    argoObservedAt: "2026-08-09T00:00:00Z",
  });
  vi.spyOn(api, "operations").mockResolvedValue({ items: [] });
  vi.spyOn(api, "environments").mockResolvedValue({
    items: [
      {
        id: "environment-production",
        projectId: "project-payments",
        name: "Production",
        namespace: "payments-production",
      },
    ],
  });
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [
      {
        id: "project-payments",
        name: "Payments",
        teamId: "team-commerce",
      },
    ],
  });
  vi.spyOn(api, "applicationRegistry").mockResolvedValue({
    items: [],
    truncated: false,
  });
  vi.spyOn(api, "me").mockResolvedValue({
    id: "user-admin",
    displayName: "Admin",
    role: "developer",
    authentication: { kind: "session" },
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderApplication(capabilities: Capabilities) {
  vi.spyOn(api, "capabilities").mockResolvedValue(capabilities);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ApplicationPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

describe("application rollout truth", () => {
  it("shows only the authoritative Argo observed revision", async () => {
    const authoritativeRevision = "d".repeat(40);
    const staleStatusRevision = "b".repeat(40);
    const staleDeploymentRevision = "c".repeat(40);
    vi.mocked(api.deployment).mockResolvedValue({
      ...(await api.deployment("deployment-production")),
      observedRevision: staleDeploymentRevision,
    });
    vi.mocked(api.deploymentStatus).mockResolvedValue({
      state: "git-committed",
      operationStatus: "succeeded",
      desiredRevision: "a".repeat(40),
      observedRevision: staleStatusRevision,
      argoObservedRevision: authoritativeRevision,
      argoSyncStatus: "synced",
      rolloutHealth: "healthy",
    });

    renderApplication({ features: {}, capabilities: [] });

    const observed = (await screen.findByText("Observed revision"))
      .parentElement;
    expect(observed).toHaveTextContent(authoritativeRevision);
    expect(observed).not.toHaveTextContent(staleStatusRevision);
    expect(observed).not.toHaveTextContent(staleDeploymentRevision);
  });

  it("fails closed when no authoritative Argo revision is observed", async () => {
    const staleStatusRevision = "b".repeat(40);
    const staleDeploymentRevision = "c".repeat(40);
    vi.mocked(api.deployment).mockResolvedValue({
      ...(await api.deployment("deployment-production")),
      observedRevision: staleDeploymentRevision,
    });
    vi.mocked(api.deploymentStatus).mockResolvedValue({
      state: "git-committed",
      operationStatus: "succeeded",
      desiredRevision: "a".repeat(40),
      observedRevision: staleStatusRevision,
      argoSyncStatus: "unknown",
      rolloutHealth: "unknown",
    });

    renderApplication({ features: {}, capabilities: [] });

    const observed = (await screen.findByText("Observed revision"))
      .parentElement;
    expect(observed).toHaveTextContent("Not reported");
    expect(observed).not.toHaveTextContent(staleStatusRevision);
    expect(observed).not.toHaveTextContent(staleDeploymentRevision);
  });

  it("shows protected pull-request review without treating its candidate as desired", async () => {
    vi.mocked(api.deploymentStatus).mockResolvedValue({
      state: "review-pending",
      operationStatus: "succeeded",
      argoSyncStatus: "unknown",
      rolloutHealth: "unknown",
    });
    vi.mocked(api.operations).mockResolvedValue({
      items: [
        {
          id: "operation-protected",
          kind: "deployment.git-write",
          status: "succeeded",
          state: "succeeded",
          targetType: "deployment",
          targetId: "deployment-production",
          target: { id: "deployment-production", type: "deployment" },
          requestId: "request-protected",
          generation: 1,
          progress: [],
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:01:00Z",
          pullRequest: {
            number: 42,
            url: "https://github.com/acme/platform/pull/42",
            state: "open",
            candidateRevision: "b".repeat(40),
          },
        },
      ],
    });
    renderApplication({ features: {}, capabilities: [] });
    expect(
      await screen.findByRole("link", { name: /Pull request #42 · open/i }),
    ).toHaveAttribute("href", "https://github.com/acme/platform/pull/42");
    expect(
      screen.getByText(/candidate is not desired state/i),
    ).toBeInTheDocument();
    expect(screen.getAllByText("Review open").length).toBeGreaterThan(0);
  });

  it("does not turn Git operation success into rollout success", async () => {
    vi.mocked(api.deploymentStatus).mockResolvedValue({
      state: "git-committed",
      operationStatus: "succeeded",
      desiredRevision: "a".repeat(40),
      argoSyncStatus: "unknown",
      rolloutHealth: "unknown",
    });
    renderApplication({ features: {}, capabilities: [] });
    expect(await screen.findByText("Argo sync")).toBeInTheDocument();
    expect(screen.getByText("Rollout health")).toBeInTheDocument();
    expect(screen.getAllByText("Unknown").length).toBeGreaterThanOrEqual(2);
  });

  it("shows exact OutOfSync and Degraded observation independently", async () => {
    vi.mocked(api.deploymentStatus).mockResolvedValue({
      state: "git-committed",
      operationStatus: "succeeded",
      desiredRevision: "a".repeat(40),
      argoSyncStatus: "out-of-sync",
      rolloutHealth: "degraded",
      argoObservedRevision: "b".repeat(40),
      argoObservedAt: "2026-08-09T00:00:00Z",
    });
    renderApplication({ features: {}, capabilities: [] });
    expect(await screen.findAllByText("Out of sync")).not.toHaveLength(0);
    expect(screen.getAllByText("Degraded")).not.toHaveLength(0);
    expect(screen.getAllByText("Behind")).not.toHaveLength(0);
  });

  it("shows exact Kubernetes replica readiness and rollout condition", async () => {
    vi.mocked(api.deploymentStatus).mockResolvedValue({
      state: "git-committed",
      operationStatus: "succeeded",
      argoSyncStatus: "synced",
      rolloutHealth: "progressing",
      desiredReplicas: 3,
      readyReplicas: 2,
      rolloutConditions: [
        {
          type: "Progressing",
          status: "True",
          reason: "ReplicaSetUpdated",
        },
      ],
      rolloutObservedAt: "2026-08-09T00:00:00Z",
    });
    renderApplication({ features: {}, capabilities: [] });
    expect(await screen.findByText("Ready replicas")).toBeVisible();
    expect(screen.getAllByText("2/3").length).toBeGreaterThan(0);
    expect(screen.getByText("Rollout condition")).toBeVisible();
    expect(screen.getAllByText("Progressing").length).toBeGreaterThan(0);
  });
});

describe("application runtime-secret navigation", () => {
  it("hides the variables and secrets tab while the feature is false", async () => {
    const queryClient = renderApplication({
      features: { secretBindings: false },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["secret-bindings:read"],
        },
      ],
    });

    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toBeDefined(),
    );
    expect(
      screen.queryByRole("button", { name: "Variables & secrets" }),
    ).not.toBeInTheDocument();
    expect(api.environments).not.toHaveBeenCalled();
    expect(api.projects).not.toHaveBeenCalled();
  });

  it("does not use broad top-level actions as a scoped grant", async () => {
    renderApplication({
      actions: ["secret-bindings:read"],
      features: { secretBindings: true },
      capabilities: [],
    });

    await waitFor(() => expect(api.environments).toHaveBeenCalledOnce());
    expect(
      screen.queryByRole("button", { name: "Variables & secrets" }),
    ).not.toBeInTheDocument();
  });

  it("opens the panel only after an exact effective read capability", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({ items: [] });
    renderApplication({
      actions: ["secret-bindings:read"],
      features: { secretBindings: true },
      capabilities: [
        {
          role: "viewer",
          scopeType: "environment",
          scopeId: "environment-production",
          actions: ["secret-bindings:read"],
        },
      ],
    });

    await user.click(
      await screen.findByRole("button", { name: "Variables & secrets" }),
    );
    expect(
      await screen.findByText("No runtime-secret bindings"),
    ).toBeInTheDocument();
    expect(api.runtimeSecretBindings).toHaveBeenCalledWith(
      "application-payments",
      "environment-production",
    );
  });

  it("returns to overview when a selected feature tab loses access", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({ items: [] });
    const queryClient = renderApplication({
      features: { secretBindings: true },
      capabilities: [
        {
          scopeType: "environment",
          scopeId: "environment-production",
          actions: ["secret-bindings:read"],
        },
      ],
    });

    await user.click(
      await screen.findByRole("button", { name: "Variables & secrets" }),
    );
    expect(await screen.findByText("No runtime-secret bindings")).toBeVisible();

    queryClient.setQueryData(["capabilities"], {
      features: { secretBindings: true },
      capabilities: [],
    });

    await waitFor(() =>
      expect(screen.getByText("Immutable artifact")).toBeVisible(),
    );
    expect(screen.queryByText("Secret metadata access not granted")).toBeNull();
  });
});

describe("application custom-certificate navigation", () => {
  it("keeps certificate management hidden without exact runtime readiness", async () => {
    const queryClient = renderApplication({
      features: { customCertificates: false },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["certificate-bindings:read"],
        },
      ],
    });
    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toBeDefined(),
    );
    expect(
      screen.queryByRole("button", { name: "TLS certificates" }),
    ).not.toBeInTheDocument();
    expect(api.environments).not.toHaveBeenCalled();
  });

  it("opens human-only metadata management after exact scoped readiness", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "certificateBindings").mockResolvedValue({ items: [] });
    renderApplication({
      features: { customCertificates: true },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "environment",
          scopeId: "environment-production",
          actions: ["certificate-bindings:read", "certificate-bindings:create"],
        },
      ],
    });
    await user.click(
      await screen.findByRole("button", { name: "TLS certificates" }),
    );
    expect(await screen.findByText("No custom certificates")).toBeVisible();
    expect(api.certificateBindings).toHaveBeenCalledWith(
      "application-payments",
      "environment-production",
    );
  });
});

describe("application approved-Helm navigation", () => {
  it("hides Helm when readiness is false or only broad actions exist", async () => {
    const helmApprovals = vi.spyOn(api, "helmApprovals");
    const queryClient = renderApplication({
      actions: ["helm.read"],
      features: { helmDeployments: false },
      capabilities: [],
    });
    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toBeDefined(),
    );
    expect(
      screen.queryByRole("button", { name: "Helm" }),
    ).not.toBeInTheDocument();
    expect(helmApprovals).not.toHaveBeenCalled();
  });

  it("opens the exact deployment environment panel after a scoped read grant", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "helmApprovals").mockResolvedValue({ items: [] });
    vi.spyOn(api, "helmRelease").mockRejectedValue(
      Object.assign(new Error("missing"), { status: 404 }),
    );
    vi.spyOn(api, "helmReleaseHistory").mockResolvedValue({ items: [] });
    renderApplication({
      features: { helmDeployments: true, helmRollbacks: true },
      capabilities: [
        {
          scopeType: "environment",
          scopeId: "environment-production",
          actions: ["helm.read"],
        },
      ],
    });
    await user.click(await screen.findByRole("button", { name: "Helm" }));
    expect(await screen.findByText("No approved Helm charts")).toBeVisible();
    expect(api.helmApprovals).toHaveBeenCalledWith(
      "application-payments",
      "environment-production",
      50,
    );
  });
});

describe("application registry navigation", () => {
  it("hides artifacts while the registry feature is false", async () => {
    const queryClient = renderApplication({
      features: { registry: false },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["registry:read"],
        },
      ],
    });

    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toBeDefined(),
    );
    expect(
      screen.queryByRole("button", { name: "Artifacts" }),
    ).not.toBeInTheDocument();
    expect(api.applicationRegistry).not.toHaveBeenCalled();
  });

  it("does not use broad top-level registry actions as scoped access", async () => {
    renderApplication({
      actions: ["registry:read"],
      features: { registry: true },
      capabilities: [],
    });

    await waitFor(() => expect(api.projects).toHaveBeenCalledOnce());
    expect(
      screen.queryByRole("button", { name: "Artifacts" }),
    ).not.toBeInTheDocument();
    expect(api.applicationRegistry).not.toHaveBeenCalled();
  });

  it("loads the application aggregate only after an exact effective grant", async () => {
    const user = userEvent.setup();
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [],
      truncated: false,
    });
    renderApplication({
      features: { registry: true },
      capabilities: [
        {
          role: "viewer",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["registry:read"],
        },
      ],
    });

    await user.click(await screen.findByRole("button", { name: "Artifacts" }));
    expect(
      await screen.findByText("No registry policy configured"),
    ).toBeInTheDocument();
    expect(api.applicationRegistry).toHaveBeenCalledWith(
      "application-payments",
      50,
    );
  });
});

describe("application deployment rollback", () => {
  it("shows exact eligible history and submits only the selected operation identity", async () => {
    const user = userEvent.setup();
    const sourceOperationId = "11111111-1111-4111-8111-111111111111";
    vi.spyOn(api, "deploymentRollbackSources").mockResolvedValue({
      items: [
        {
          sourceOperationId,
          generation: 2,
          image: `ghcr.io/acme/payments@sha256:${"b".repeat(64)}`,
          artifactAssurance: "managed-release-verified",
          managedReleaseVerified: true,
          createdAt: "2026-08-08T00:00:00Z",
        },
      ],
    });
    vi.spyOn(api, "rollbackDeployment").mockResolvedValue({
      id: "22222222-2222-4222-8222-222222222222",
      kind: "deployment.git-write",
      status: "queued",
      state: "queued",
      targetType: "deployment",
      targetId: "deployment-production",
      requestId: "rollback-request",
      generation: 4,
      progress: [],
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:00:00Z",
    });
    renderApplication({
      features: { deploymentRollbacks: true },
      capabilities: [
        {
          scopeType: "environment",
          scopeId: "environment-production",
          actions: ["deployments:update"],
        },
      ],
    });

    await user.click(await screen.findByRole("button", { name: "Releases" }));
    expect(await screen.findByText("Managed release verified")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Select rollback" }));
    await user.click(
      screen.getByRole("checkbox", {
        name: "I understand this creates a new Git intent governed by this environment's publication policy.",
      }),
    );
    await user.click(screen.getByRole("button", { name: "Confirm rollback" }));
    await waitFor(() => expect(api.rollbackDeployment).toHaveBeenCalledOnce());
    expect(api.rollbackDeployment).toHaveBeenCalledWith(
      "deployment-production",
      sourceOperationId,
      expect.any(String),
    );
    expect(
      await screen.findByText("Rollback Git intent accepted"),
    ).toBeVisible();
  });

  it("does not load rollback history from a top-level action without exact capability", async () => {
    const history = vi.spyOn(api, "deploymentRollbackSources");
    const user = userEvent.setup();
    renderApplication({
      actions: ["deployments:update"],
      features: { deploymentRollbacks: true },
      capabilities: [],
    });
    await user.click(await screen.findByRole("button", { name: "Releases" }));
    expect(
      screen.queryByText("Prior successful versions"),
    ).not.toBeInTheDocument();
    expect(history).not.toHaveBeenCalled();
  });
});
