import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  BuildDefinition,
  Project,
  RegistryTarget,
} from "../api/types";
import { BuildDefinitionForm } from "./BuildDefinitionForm";

const project: Project = { id: "project-safe", name: "Payments" };
const application: Application = {
  id: "application-safe",
  projectId: project.id,
  name: "API",
};
const target: RegistryTarget = {
  id: "target-safe",
  name: "Primary",
  mode: "managed",
  endpoint: "registry.example.test",
  repositoryPrefix: "tenant",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};
const definition: BuildDefinition = {
  id: "definition-safe",
  projectId: project.id,
  applicationId: application.id,
  installationId: "installation-safe",
  repositoryId: "repository-safe",
  triggerRef: "refs/heads/main",
  contextPath: ".",
  dockerfilePath: "Dockerfile",
  platforms: ["linux/amd64"],
  registry: {
    targetId: target.id,
    mode: target.mode,
    server: target.endpoint,
    repositoryPrefix: target.repositoryPrefix,
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

beforeEach(() => {
  vi.spyOn(api, "githubInstallations").mockResolvedValue({
    nextCursor: undefined,
    items: [
      {
        id: "installation-safe",
        githubInstallationId: 42,
        accountLogin: "example",
        accountType: "Organization",
        ownerUserId: "user-safe",
        visibility: "private",
        repositorySelection: "selected",
        repositoryCount: 1,
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      },
    ],
  });
  vi.spyOn(api, "githubInstallationRepositories").mockResolvedValue({
    nextCursor: undefined,
    items: [
      {
        id: "repository-safe",
        githubRepositoryId: 84,
        installationId: "installation-safe",
        ownerId: 21,
        ownerLogin: "example",
        name: "api",
        lifecycle: "active",
      },
    ],
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderForm(humanSession = true) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <BuildDefinitionForm
        application={application}
        project={project}
        capabilities={[
          {
            scopeType: "project",
            scopeId: project.id,
            actions: ["build-definitions:write"],
          },
        ]}
        humanSession={humanSession}
        registryTargets={[target]}
      />
    </QueryClientProvider>,
  );
}

describe("build definition form", () => {
  it("submits no build-secret or SSH fields while operator profile resolution is unavailable", async () => {
    const user = userEvent.setup();
    const create = vi
      .spyOn(api, "createBuildDefinition")
      .mockResolvedValue(definition);
    renderForm();

    await screen.findByRole("option", { name: "example" });
    await user.selectOptions(
      screen.getByLabelText(/^GitHub installation/),
      "installation-safe",
    );
    await screen.findByRole("option", { name: "example/api" });
    await user.selectOptions(
      screen.getByLabelText(/^Repository/),
      "repository-safe",
    );
    await user.selectOptions(
      screen.getByLabelText(/^Registry target/),
      "target-safe",
    );
    await user.click(
      screen.getByRole("button", { name: "Create immutable definition" }),
    );

    expect(
      await screen.findByText("Immutable build definition created"),
    ).toBeInTheDocument();
    expect(create).toHaveBeenCalledTimes(1);
    const input = create.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(input).not.toHaveProperty("secretFiles");
    expect(input).not.toHaveProperty("sshFiles");
    expect(
      screen.getByText("Build-secret and SSH profiles are not available yet"),
    ).toBeInTheDocument();
  });

  it("rejects secret-like build arguments before they reach a mutation cache or API", async () => {
    const user = userEvent.setup();
    const create = vi.spyOn(api, "createBuildDefinition");
    renderForm();

    await screen.findByRole("option", { name: "example" });
    await user.selectOptions(
      screen.getByLabelText(/^GitHub installation/),
      "installation-safe",
    );
    await screen.findByRole("option", { name: "example/api" });
    await user.selectOptions(
      screen.getByLabelText(/^Repository/),
      "repository-safe",
    );
    await user.selectOptions(
      screen.getByLabelText(/^Registry target/),
      "target-safe",
    );
    await user.type(
      screen.getByLabelText(/^Non-secret build arguments/),
      "API_TOKEN=do-not-send",
    );
    await user.click(
      screen.getByRole("button", { name: "Create immutable definition" }),
    );

    expect(
      await screen.findByText(/API_TOKEN is invalid or secret-like/),
    ).toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
  });

  it("hides every mutation control for a non-human principal", () => {
    renderForm(false);
    expect(
      screen.getByText("Build-definition changes require a human session"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Create immutable definition" }),
    ).not.toBeInTheDocument();
    expect(api.githubInstallations).not.toHaveBeenCalled();
  });
});
