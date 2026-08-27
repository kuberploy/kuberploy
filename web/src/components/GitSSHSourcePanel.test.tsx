import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { GitSSHSourcePanel } from "./GitSSHSourcePanel";

const project = { id: "project-1", name: "Payments" };
const application = {
  id: "application-1",
  projectId: project.id,
  name: "API",
};

function wrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

beforeEach(() => {
  vi.spyOn(api, "projectGitSSHKeys").mockResolvedValue({ items: [] });
  vi.spyOn(api, "applicationGitSSHKeys").mockResolvedValue({ items: [] });
  vi.spyOn(api, "buildDefinitions").mockResolvedValue({
    items: [],
    nextCursor: null,
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Git SSH source key scope", () => {
  it("defaults to an isolated App key and can generate it", async () => {
    const user = userEvent.setup();
    const create = vi.spyOn(api, "createGitSSHKey").mockResolvedValue({
      scope: "app",
      ownerId: application.id,
      revision: 1,
      status: "active",
      publicKey: "ssh-ed25519 AAAATEST",
      fingerprint: "SHA256:test",
    });
    render(
      <GitSSHSourcePanel application={application} project={project} enabled />,
      { wrapper: wrapper() },
    );

    expect(
      await screen.findByRole("radio", { name: /App key/ }),
    ).toHaveAttribute("aria-checked", "true");
    await user.click(
      await screen.findByRole("button", { name: "Generate deploy key" }),
    );
    expect(create).toHaveBeenCalledWith(
      "app",
      application.id,
      expect.any(String),
    );
  });

  it("selects a reusable Project key and uses shadcn confirmation for rotation", async () => {
    const user = userEvent.setup();
    vi.mocked(api.projectGitSSHKeys).mockResolvedValue({
      items: [
        {
          scope: "project",
          ownerId: project.id,
          revision: 1,
          status: "active",
          publicKey: "ssh-ed25519 AAAATEST",
          fingerprint: "SHA256:test",
        },
      ],
    });
    const rotate = vi.spyOn(api, "rotateGitSSHKey").mockResolvedValue({
      scope: "project",
      ownerId: project.id,
      revision: 2,
      status: "active",
      publicKey: "ssh-ed25519 AAAANEW",
      fingerprint: "SHA256:new",
    });
    render(
      <GitSSHSourcePanel application={application} project={project} enabled />,
      { wrapper: wrapper() },
    );

    await user.click(await screen.findByRole("radio", { name: /Project key/ }));
    expect(await screen.findByLabelText("SSH public key")).toHaveValue(
      "ssh-ed25519 AAAATEST",
    );
    await user.click(screen.getByRole("button", { name: "Rotate" }));
    expect(screen.getByRole("alertdialog")).toHaveTextContent(
      "Rotate deploy key?",
    );
    await user.click(screen.getByRole("button", { name: "Rotate key" }));
    await waitFor(() =>
      expect(rotate).toHaveBeenCalledWith(
        "project",
        project.id,
        expect.any(String),
      ),
    );
  });

  it("fails closed when installation storage is disabled", () => {
    render(
      <GitSSHSourcePanel
        application={application}
        project={project}
        enabled={false}
      />,
      { wrapper: wrapper() },
    );
    expect(screen.getByText("Git SSH is unavailable")).toBeInTheDocument();
    expect(api.projectGitSSHKeys).not.toHaveBeenCalled();
  });

  it("creates a pinned Git SSH definition and manually builds an exact commit", async () => {
    const user = userEvent.setup();
    vi.mocked(api.applicationGitSSHKeys).mockResolvedValue({
      items: [
        {
          scope: "app",
          ownerId: application.id,
          revision: 2,
          status: "active",
          publicKey: "ssh-ed25519 AAAATEST",
          fingerprint: "SHA256:test",
        },
      ],
    });
    const activeDefinition = {
      id: "definition-1",
      projectId: project.id,
      applicationId: application.id,
      sourceKind: "git_ssh" as const,
      repositoryUrl: "ssh://git@git.example.test/team/repository.git",
      gitSSHKeyScope: "app" as const,
      gitSSHKeyRevision: 2,
      triggerRef: "refs/heads/main",
      contextPath: ".",
      dockerfilePath: "Dockerfile",
      platforms: ["linux/amd64" as const],
      registry: {
        targetId: "registry-1",
        mode: "managed" as const,
        server: "registry.example.test",
        repositoryPrefix: "apps",
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
      createdAt: "2026-08-23T00:00:00Z",
      updatedAt: "2026-08-23T00:00:00Z",
    };
    vi.mocked(api.buildDefinitions).mockResolvedValue({
      items: [activeDefinition],
      nextCursor: null,
    });
    const createDefinition = vi
      .spyOn(api, "createBuildDefinition")
      .mockResolvedValue(activeDefinition);
    const createBuild = vi
      .spyOn(api, "createManualBuildAttempt")
      .mockResolvedValue({
        id: "attempt-1",
        definitionId: activeDefinition.id,
        projectId: project.id,
        applicationId: application.id,
        commitSha: "b".repeat(40),
        gitRef: "refs/heads/main",
        generation: 1,
        state: "queued",
        executionAttempts: 0,
        maxAttempts: 3,
        createdAt: "2026-08-23T00:00:00Z",
        updatedAt: "2026-08-23T00:00:00Z",
      });
    render(
      <GitSSHSourcePanel
        application={application}
        project={project}
        enabled
        buildConfigured
        buildReady
        canManageBuilds
        defaultBuildPlatform="linux/arm64"
        registryTargets={[
          {
            id: "registry-1",
            name: "Managed registry",
            mode: "managed",
            endpoint: "registry.example.test",
            repositoryPrefix: "apps",
            createdAt: "2026-08-23T00:00:00Z",
            updatedAt: "2026-08-23T00:00:00Z",
          },
        ]}
      />,
      { wrapper: wrapper() },
    );

    await user.type(
      await screen.findByLabelText(/^Repository URL/),
      "ssh://git@git.example.test/team/repository.git",
    );
    await user.selectOptions(
      screen.getByLabelText(/^Registry target/),
      "registry-1",
    );
    await user.type(
      screen.getByLabelText(/^SSH host public key/),
      "ssh-ed25519 AAAAHOST",
    );
    expect(
      screen.getByRole("checkbox", { name: "linux/amd64" }),
    ).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: "linux/arm64" })).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: "linux/amd64" }));
    await user.click(screen.getByRole("button", { name: /Replace binding/ }));
    await waitFor(() => expect(createDefinition).toHaveBeenCalled());
    expect(createDefinition.mock.calls[0]?.[1]).toMatchObject({
      sourceKind: "git_ssh",
      repositoryUrl: "ssh://git@git.example.test/team/repository.git",
      gitSSHKeyScope: "app",
      gitSSHKeyRevision: 2,
      hostKeyPins: [
        { endpoint: "git.example.test:22", publicKey: "ssh-ed25519 AAAAHOST" },
      ],
      platforms: ["linux/amd64", "linux/arm64"],
    });

    const commit = "b".repeat(40);
    await user.type(screen.getByLabelText(/^Commit SHA/), commit);
    await user.click(screen.getByRole("button", { name: "Build commit" }));
    await waitFor(() =>
      expect(createBuild).toHaveBeenCalledWith(
        activeDefinition.id,
        commit,
        expect.any(String),
      ),
    );
  });

  it("keeps repository binding editable while build execution is unavailable", async () => {
    vi.mocked(api.applicationGitSSHKeys).mockResolvedValue({
      items: [
        {
          scope: "app",
          ownerId: application.id,
          revision: 1,
          status: "active",
          publicKey: "ssh-ed25519 AAAATEST",
          fingerprint: "SHA256:test",
        },
      ],
    });

    render(
      <GitSSHSourcePanel
        application={application}
        project={project}
        enabled
        buildConfigured
        buildReady={false}
        canManageBuilds
      />,
      { wrapper: wrapper() },
    );

    expect(await screen.findByLabelText(/^Repository URL/)).toBeVisible();
    expect(screen.getByText("Builder runtime unavailable")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Build commit" })).toBeNull();
  });
});
