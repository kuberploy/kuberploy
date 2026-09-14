import { selectOption } from "../test/selectOption";
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
    expect(screen.getByRole("radio", { name: /App key/ })).toHaveClass(
      "aria-checked:border-mint",
    );
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
    expect(screen.getByRole("radio", { name: /Project key/ })).toHaveClass(
      "aria-checked:border-mint",
    );
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

  it("creates a pinned Git SSH source with an editable branch or tag", async () => {
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
      gitSSHKnownHosts:
        "@cert-authority git.example.test ssh-ed25519 AAAAHOST fixture-comment\n",
      triggerRef: "refs/tags/v1.2.3",
      contextPath: ".",
      dockerfilePath: "Dockerfile",
      platforms: ["linux/arm64" as const],
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
      cacheImports: 1,
      profile: {
        resource: "standard",
        timeoutSeconds: 900,
        egress: "registry-and-source",
      },
      maxAttempts: 3,
      sourceDigest: `sha256:${"a".repeat(64)}`,
      sourceRevision: 1,
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

    expect(await screen.findByLabelText(/^Repository URL/)).toHaveValue(
      "ssh://git@git.example.test/team/repository.git",
    );
    expect(screen.getByLabelText(/^Branch or tag/)).toHaveValue(
      "refs/tags/v1.2.3",
    );
    expect(screen.getByLabelText(/^Registry target/)).toHaveValue("registry-1");
    expect(screen.getByLabelText(/^SSH host public key/)).toHaveValue(
      "ssh-ed25519 AAAAHOST",
    );
    expect(
      screen.getByRole("checkbox", { name: "linux/amd64" }),
    ).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: "linux/arm64" })).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: "linux/amd64" }));
    await user.click(screen.getByRole("button", { name: /Save App source/ }));
    await waitFor(() => expect(createDefinition).toHaveBeenCalled());
    expect(createDefinition.mock.calls[0]?.[1]).toMatchObject({
      sourceKind: "git_ssh",
      repositoryUrl: "ssh://git@git.example.test/team/repository.git",
      gitSSHKeyScope: "app",
      gitSSHKeyRevision: 2,
      hostKeyPins: [
        { endpoint: "git.example.test:22", publicKey: "ssh-ed25519 AAAAHOST" },
      ],
      triggerRef: "refs/tags/v1.2.3",
      platforms: ["linux/amd64", "linux/arm64"],
    });

    expect(screen.queryByLabelText(/^Commit SHA/)).toBeNull();
    expect(
      screen.getByText(/Deploy resolves the configured branch or tag head/),
    ).toBeVisible();
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
  });

  it("explains why the submit button is disabled with no registry target", async () => {
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
        buildReady
        canManageBuilds
      />,
      { wrapper: wrapper() },
    );

    // Regression: the submit button was the only reachable signal that a
    // registry target was required -- no hint told the user why it was
    // disabled.
    expect(
      await screen.findByText(/No registry target is attached/),
    ).toBeVisible();
    expect(
      screen.getByRole("button", { name: /Connect App source/ }),
    ).toBeDisabled();
  });
});
