import { openSelect } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { BuildAttempt, BuildDefinition, Capabilities } from "../api/types";
import { SourceBuildsPage } from "./SourceBuildsPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: PropsWithChildren<{ to: string }>) => (
    <a href={to}>{children}</a>
  ),
}));

const definition: BuildDefinition = {
  sourceKind: "github",
  id: "definition-safe",
  projectId: "project-safe",
  applicationId: "application-safe",
  installationId: "installation-safe",
  repositoryId: "repository-safe",
  triggerRef: "refs/heads/main",
  contextPath: ".",
  dockerfilePath: "Dockerfile",
  platforms: ["linux/amd64"],
  registry: {
    targetId: "target-safe",
    mode: "managed",
    server: "registry.example.test",
    repositoryPrefix: "tenant",
  },
  buildArgs: [],
  secretFiles: [],
  sshFiles: [],
  cacheTrustLane: "protected",
  cacheImports: 2,
  profile: {
    resource: "standard",
    timeoutSeconds: 900,
    egress: "registry-and-source",
  },
  maxAttempts: 3,
  definitionDigest: `sha256:${"a".repeat(64)}`,
  definitionGeneration: 1,
  enabled: true,
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};
const attempt: BuildAttempt = {
  id: "attempt-safe",
  definitionId: definition.id,
  projectId: definition.projectId,
  applicationId: definition.applicationId,
  commitSha: "b".repeat(40),
  gitRef: "refs/heads/main",
  generation: 1,
  state: "running",
  cacheReuse: "hit",
  executionAttempts: 1,
  maxAttempts: 3,
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

beforeEach(() => {
  vi.spyOn(api, "me").mockResolvedValue({
    id: "user-safe",
    displayName: "User",
    role: "viewer",
    authentication: { kind: "session" },
  });
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [
      { id: "project-safe", name: "Payments", teamId: "team-safe" },
      { id: "project-restricted", name: "Restricted", teamId: "team-other" },
    ],
  });
  vi.spyOn(api, "applications").mockResolvedValue({
    items: [
      {
        id: "application-safe",
        projectId: "project-safe",
        name: "API",
      },
      {
        id: "application-restricted",
        projectId: "project-restricted",
        name: "Restricted app",
      },
    ],
  });
  vi.spyOn(api, "buildDefinitions").mockResolvedValue({
    items: [definition],
    nextCursor: undefined,
  });
  vi.spyOn(api, "buildAttempts").mockResolvedValue({
    items: [attempt],
    nextCursor: undefined,
  });
  vi.spyOn(api, "disconnectBuildDefinition").mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPage(response: Capabilities) {
  vi.spyOn(api, "capabilities").mockResolvedValue(response);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <SourceBuildsPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

describe("source-build workspace", () => {
  it("disconnects a source through the styled confirmation dialog", async () => {
    const user = userEvent.setup();
    renderPage({
      features: {
        githubAppSetup: false,
        builds: true,
        builder: true,
        registry: false,
      },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: [
            "build-definitions:read",
            "build-definitions:write",
            "builds:read",
          ],
        },
      ],
    });

    await screen.findByText("registry.example.test");
    await user.click(
      await screen.findByRole("button", { name: "Disconnect source" }),
    );
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Confirm deletion"), "DISCONNECT");
    await user.click(
      screen.getAllByRole("button", { name: "Disconnect source" }).at(-1)!,
    );
    await waitFor(() =>
      expect(api.disconnectBuildDefinition).toHaveBeenCalledWith(
        "application-safe",
        "definition-safe",
        expect.any(String),
      ),
    );
    await waitFor(() =>
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument(),
    );
  });

  it("does not query build catalogs when the feature is disabled", async () => {
    renderPage({
      features: { githubAppSetup: false, builds: false, builder: false },
      featureStates: { builds: "disabled", builder: "disabled" },
      capabilities: [
        {
          scopeType: "platform",
          scopeId: "platform",
          actions: ["build-definitions:read", "builds:read"],
        },
      ],
    });

    expect(
      await screen.findByText("Source builds are not ready"),
    ).toBeInTheDocument();
    expect(api.projects).not.toHaveBeenCalled();
    expect(api.applications).not.toHaveBeenCalled();
    expect(api.buildDefinitions).not.toHaveBeenCalled();
    expect(api.buildAttempts).not.toHaveBeenCalled();
  });

  it("keeps immutable history readable when builder capacity is unavailable", async () => {
    renderPage({
      features: { githubAppSetup: false, builds: false, builder: false },
      featureStates: { builds: "unavailable", builder: "unavailable" },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: [
            "build-definitions:read",
            "build-definitions:write",
            "builds:read",
          ],
        },
      ],
    });

    expect(
      await screen.findByText("Builder runtime unavailable"),
    ).toBeInTheDocument();
    expect(screen.getByText(/eligible Ready builder node/)).toBeInTheDocument();
    expect(await screen.findByText("Source connections")).toBeInTheDocument();
    expect(screen.getByText("Attempt history")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Create immutable definition" }),
    ).toBeInTheDocument();
    expect(api.buildDefinitions).toHaveBeenCalledWith("application-safe");
    expect(api.buildAttempts).toHaveBeenCalledWith("application-safe", 50);
  });

  it("does not treat coarse unions or environment grants as build access", async () => {
    renderPage({
      actions: [
        "build-definitions:read",
        "build-definitions:write",
        "builds:read",
        "builds:cancel",
      ],
      features: { githubAppSetup: false, builds: true, builder: true },
      capabilities: [
        {
          scopeType: "environment",
          scopeId: "environment-safe",
          actions: [
            "build-definitions:read",
            "build-definitions:write",
            "builds:read",
            "builds:cancel",
          ],
        },
      ],
    });

    expect(
      await screen.findByText("No build-readable application"),
    ).toBeInTheDocument();
    expect(api.buildDefinitions).not.toHaveBeenCalled();
    expect(api.buildAttempts).not.toHaveBeenCalled();
  });

  it("filters the application catalog and hides all mutations for automation", async () => {
    vi.mocked(api.me).mockResolvedValue({
      id: "service-account-safe",
      displayName: "Build reader",
      role: "project-admin",
      authentication: {
        kind: "service-account",
        serviceAccountId: "service-account-safe",
        tokenId: "token-safe",
        scopes: ["app.read", "build.create"],
        expiresAt: "2026-08-09T01:00:00Z",
      },
    });
    renderPage({
      features: {
        githubAppSetup: false,
        builds: true,
        builder: true,
        registry: false,
      },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: [
            "build-definitions:read",
            "build-definitions:write",
            "builds:read",
            "builds:cancel",
            "builds:retry",
          ],
        },
      ],
    });

    expect(await screen.findByText("Attempt history")).toBeInTheDocument();
    await openSelect(screen.getByRole("combobox", { name: /application/i }));
    expect(
      screen.getByRole("option", { name: "Payments / API" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: /Restricted/ }),
    ).not.toBeInTheDocument();
    expect(await screen.findAllByText("main")).not.toHaveLength(0);
    expect(screen.getByText("Registry cache: hit")).toBeInTheDocument();
    expect(screen.queryByText("refs/heads/main")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Create immutable definition" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Cancel build" }),
    ).not.toBeInTheDocument();
    await waitFor(() =>
      expect(api.buildDefinitions).toHaveBeenCalledWith("application-safe"),
    );
    expect(api.buildAttempts).toHaveBeenCalledWith("application-safe", 50);
  });

  it("does not offer a platform target without an application policy", async () => {
    vi.spyOn(api, "applicationRegistry").mockResolvedValue({
      items: [],
      truncated: false,
    });
    const platformTargets = vi.spyOn(api, "registryTargets").mockResolvedValue({
      items: [
        {
          id: "target-platform-only",
          name: "Platform registry",
          mode: "managed",
          endpoint: "https://registry.example.test",
          repositoryPrefix: "tenant",
          pullCredentialRef: "pull",
          pushCredentialRef: "push",
          cacheCredentialRef: "cache",
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
      ],
      truncated: false,
    });
    vi.spyOn(api, "githubInstallations").mockResolvedValue({
      items: [],
      nextCursor: null,
    });

    renderPage({
      features: {
        githubAppSetup: true,
        builds: true,
        builder: true,
        registry: true,
      },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: [
            "build-definitions:read",
            "build-definitions:write",
            "builds:read",
            "registry:read",
          ],
        },
        {
          scopeType: "platform",
          scopeId: "platform",
          actions: ["registry-targets:read"],
        },
      ],
    });

    expect(
      await screen.findByText("No accessible registry target"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: /Platform registry/ }),
    ).not.toBeInTheDocument();
    expect(platformTargets).not.toHaveBeenCalled();
  });

  it("refreshes application registry access with build history", async () => {
    const user = userEvent.setup();
    const applicationRegistry = vi
      .spyOn(api, "applicationRegistry")
      .mockResolvedValue({ items: [], truncated: false });

    renderPage({
      features: {
        githubAppSetup: false,
        builds: true,
        builder: true,
        registry: true,
      },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: [
            "build-definitions:read",
            "build-definitions:write",
            "builds:read",
            "registry:read",
          ],
        },
      ],
    });

    await waitFor(() => expect(applicationRegistry).toHaveBeenCalledOnce());
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(applicationRegistry).toHaveBeenCalledTimes(2));
  });

  it("clears revoked application scope before build queries can refetch", async () => {
    const queryClient = renderPage({
      features: {
        githubAppSetup: false,
        builds: true,
        builder: true,
        registry: false,
      },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: ["build-definitions:read", "builds:read"],
        },
      ],
    });

    expect(await screen.findByText("Attempt history")).toBeInTheDocument();
    expect(api.buildDefinitions).toHaveBeenCalledTimes(1);
    expect(api.buildAttempts).toHaveBeenCalledTimes(1);

    queryClient.setQueryData(["applications"], { items: [] });
    expect(
      await screen.findByText("No build-readable application"),
    ).toBeInTheDocument();

    await queryClient.invalidateQueries({
      queryKey: ["build-definitions", "application-safe"],
    });
    await queryClient.invalidateQueries({
      queryKey: ["build-attempts", "application-safe"],
    });

    expect(api.buildDefinitions).toHaveBeenCalledTimes(1);
    expect(api.buildAttempts).toHaveBeenCalledTimes(1);
  });
});
