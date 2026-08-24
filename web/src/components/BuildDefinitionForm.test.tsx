import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
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
  sourceKind: "github",
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
  vi.spyOn(api, "buildSecretProfiles").mockResolvedValue({
    build: [],
    ssh: [],
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderForm(
  humanSession = true,
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  }),
  defaultBuildPlatform: "linux/amd64" | "linux/arm64" = "linux/amd64",
) {
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
        defaultBuildPlatform={defaultBuildPlatform}
        humanSession={humanSession}
        registryTargets={[target]}
      />
    </QueryClientProvider>,
  );
  return queryClient;
}

describe("build definition form", () => {
  it("defaults to the installation CPU and keeps multi-platform as opt-in", async () => {
    const user = userEvent.setup();
    renderForm(true, undefined, "linux/arm64");

    const amd64 = screen.getByRole("checkbox", { name: "linux/amd64" });
    const arm64 = screen.getByRole("checkbox", { name: "linux/arm64" });
    expect(amd64).not.toBeChecked();
    expect(arm64).toBeChecked();

    await user.click(amd64);
    expect(amd64).toBeChecked();
    expect(arm64).toBeChecked();
    expect(
      screen.getByText(/Defaults to this Kuberploy installation's CPU/),
    ).toBeInTheDocument();
  });

  it("submits no build-secret or SSH fields when no operator profiles exist", async () => {
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
    expect(input.triggerRef).toBe("refs/heads/main");
    expect(input).not.toHaveProperty("secretFiles");
    expect(input).not.toHaveProperty("sshFiles");
    expect(
      screen.getByText(
        "No managed build-secret or SSH profiles are configured.",
      ),
    ).toBeInTheDocument();
  });

  it("submits only approved profile IDs and never secret file paths", async () => {
    const user = userEvent.setup();
    vi.mocked(api.buildSecretProfiles).mockResolvedValue({
      build: [{ id: "npmrc", label: "Private npm registry" }],
      ssh: [{ id: "github", label: "GitHub deploy key" }],
    });
    const create = vi
      .spyOn(api, "createBuildDefinition")
      .mockResolvedValue(definition);
    renderForm();

    await screen.findByRole("checkbox", { name: "Private npm registry" });
    await user.click(
      screen.getByRole("checkbox", { name: "Private npm registry" }),
    );
    await user.click(
      screen.getByRole("checkbox", { name: "GitHub deploy key" }),
    );
    await user.selectOptions(
      await screen.findByLabelText(/^GitHub installation/),
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

    await screen.findByText("Immutable build definition created");
    const input = create.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(input).toMatchObject({
      secretProfileIds: ["npmrc"],
      sshProfileIds: ["github"],
    });
    expect(input).not.toHaveProperty("secretFiles");
    expect(input).not.toHaveProperty("sshFiles");
  });

  it("clears selected profiles removed by an authorization refresh", async () => {
    const user = userEvent.setup();
    vi.mocked(api.buildSecretProfiles)
      .mockResolvedValueOnce({
        build: [{ id: "npmrc", label: "Private npm registry" }],
        ssh: [],
      })
      .mockResolvedValueOnce({ build: [], ssh: [] });
    const queryClient = renderForm();

    const checkbox = await screen.findByRole("checkbox", {
      name: "Private npm registry",
    });
    await user.click(checkbox);
    await queryClient.invalidateQueries({
      queryKey: ["build-secret-profiles", application.id],
    });

    await waitFor(() =>
      expect(
        screen.queryByRole("checkbox", { name: "Private npm registry" }),
      ).toBeNull(),
    );
    expect(checkbox).not.toBeChecked();
  });

  it("uses a readable tag name while submitting the canonical tag ref", async () => {
    const user = userEvent.setup();
    const create = vi
      .spyOn(api, "createBuildDefinition")
      .mockResolvedValue({ ...definition, triggerRef: "refs/tags/v1.2.3" });
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
    await user.selectOptions(screen.getByLabelText(/^Source type/), "tag");
    const tag = screen.getByLabelText(/^Tag/);
    await user.clear(tag);
    await user.type(tag, "v1.2.3");
    expect(screen.queryByDisplayValue("refs/tags/v1.2.3")).toBeNull();
    await user.click(
      screen.getByRole("button", { name: "Create immutable definition" }),
    );

    expect(create.mock.calls[0]?.[1].triggerRef).toBe("refs/tags/v1.2.3");
  });

  it("accepts valid Docker build arguments without enforcing a naming policy", async () => {
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
    await user.type(
      screen.getByLabelText(/^Docker build arguments/),
      "API_TOKEN=team-selected-value",
    );
    await user.click(
      screen.getByRole("button", { name: "Create immutable definition" }),
    );

    expect(
      await screen.findByText("Immutable build definition created"),
    ).toBeInTheDocument();
    expect(create).toHaveBeenCalledTimes(1);
    expect(create.mock.calls[0]?.[1]).toMatchObject({
      buildArgs: [{ name: "API_TOKEN", value: "team-selected-value" }],
    });
    expect(
      screen.getByText("Docker build arguments are not secret storage"),
    ).toBeInTheDocument();
  });

  it("does not show a late create success after the definition draft changes", async () => {
    const user = userEvent.setup();
    let resolveCreate!: (value: BuildDefinition) => void;
    const create = vi.spyOn(api, "createBuildDefinition").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveCreate = resolve;
        }),
    );
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
    await waitFor(() => expect(create).toHaveBeenCalledOnce());

    const branch = screen.getByLabelText(/^Branch/);
    await user.clear(branch);
    await user.type(branch, "release");
    resolveCreate(definition);

    await waitFor(() =>
      expect(
        screen.queryByText("Immutable build definition created"),
      ).not.toBeInTheDocument(),
    );
    expect(branch).toHaveValue("release");
  });

  it("labels Docker build arguments as build-time-only input", () => {
    renderForm();
    expect(
      screen.getByLabelText(/^Docker build arguments/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /runtime environment values are never passed to the builder/i,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /values may be retained in image history or build cache/i,
      ),
    ).toBeInTheDocument();
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
