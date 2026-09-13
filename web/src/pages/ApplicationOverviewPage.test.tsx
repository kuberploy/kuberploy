import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "../api/client";
import type { HelmReleaseStatus } from "../api/types";
import { ApplicationOverviewPage } from "./ApplicationOverviewPage";

const routeParams = vi.hoisted(
  (): {
    applicationId: string;
    projectId?: string;
    environmentId?: string;
  } => ({ applicationId: "application-1" }),
);
const routeSearch = vi.hoisted(
  (): { tab?: string; source?: string; environmentId?: string } => ({}),
);
const navigate = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    className,
  }: PropsWithChildren<{ to: string; className?: string }>) => (
    <a href={to} className={className}>
      {children}
    </a>
  ),
  useParams: () => routeParams,
  useSearch: () => routeSearch,
  useNavigate: () => navigate,
}));

function wrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return {
    client,
    Wrapper: ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  };
}

beforeEach(() => {
  vi.spyOn(api, "application").mockResolvedValue({
    id: "application-1",
    projectId: "project-1",
    name: "Payments API",
    sourceKind: "github",
  });
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [{ id: "project-1", name: "Payments" }],
  });
  vi.spyOn(api, "environments").mockResolvedValue({
    items: [
      {
        id: "environment-1",
        projectId: "project-1",
        name: "Test",
        namespace: "test",
      },
    ],
  });
  vi.spyOn(api, "environmentApps").mockResolvedValue({
    items: [
      {
        projectId: "project-1",
        environmentId: "environment-1",
        applicationId: "application-1",
        applicationName: "Payments API",
        applicationSlug: "payments-api",
        state: "draft",
        desiredState: "stopped",
        createdAt: "2026-08-23T00:00:00Z",
        updatedAt: "2026-08-23T00:00:00Z",
      },
    ],
  });
  vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
  vi.spyOn(api, "buildDefinitions").mockResolvedValue({
    items: [],
    nextCursor: null,
  });
  vi.spyOn(api, "disconnectBuildDefinition").mockResolvedValue(undefined);
  vi.spyOn(api, "capabilities").mockResolvedValue({
    features: { builds: false, builder: false, helmDeployments: false },
    capabilities: [],
  });
  vi.spyOn(api, "me").mockResolvedValue({
    id: "user-1",
    displayName: "User",
    role: "developer",
    authentication: { kind: "session" },
  });
});

afterEach(() => {
  cleanup();
  routeParams.applicationId = "application-1";
  delete routeParams.projectId;
  delete routeParams.environmentId;
  delete routeSearch.tab;
  delete routeSearch.source;
  delete routeSearch.environmentId;
  navigate.mockReset();
  vi.restoreAllMocks();
});

function setupHelmOverview(overrides: Partial<HelmReleaseStatus> = {}) {
  vi.mocked(api.application).mockResolvedValue({
    id: "application-1",
    projectId: "project-1",
    name: "Valkey",
    sourceKind: "helm",
  });
  vi.mocked(api.capabilities).mockResolvedValue({
    features: { helmDeployments: true },
    capabilities: [
      {
        scopeType: "environment",
        scopeId: "environment-1",
        actions: ["helm-releases:read"],
      },
    ],
  });
  const revision: HelmReleaseStatus = {
    id: "helm-revision-1",
    generation: 1,
    releaseName: "valkey",
    action: "deploy",
    desiredEnabled: true,
    source: {
      kind: "helm-repository",
      repositoryUrl: "https://valkey-io.github.io/valkey-helm",
      chart: "valkey",
      targetRevision: "0.11.0",
    },
    valuesYaml: "",
    valuesDigest: `sha256:${"a".repeat(64)}`,
    state: "applied",
    requestId: "request-1",
    createdAt: "2026-09-13T00:00:00Z",
    updatedAt: "2026-09-13T00:00:01Z",
    ...overrides,
  };
  return vi.spyOn(api, "helmRelease").mockResolvedValue(revision);
}

describe("application source overview", () => {
  it("shows the actual Helm release without a normal deployment", async () => {
    const read = setupHelmOverview();
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(
      await screen.findByRole("link", {
        name: /Test.*Applied.*valkey.*0\.11\.0.*Open App/,
      }),
    ).toBeVisible();
    expect(read).toHaveBeenCalledWith("application-1", "environment-1");
    expect(screen.queryByText("Stopped")).toBeNull();
    expect(screen.queryByText("Image pending")).toBeNull();
    expect(screen.queryByText("Configure App")).toBeNull();
  });

  it.each([
    [{ state: "pending", desiredEnabled: false }, "Pending"],
    [{ state: "applied", desiredEnabled: false }, "Disabled"],
    [{ state: "failed", desiredEnabled: true }, "Failed"],
  ] as const)(
    "distinguishes Helm release state %j",
    async (revision, label) => {
      setupHelmOverview(revision);
      render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

      const card = await screen.findByRole("link", {
        name: new RegExp(`Test.*${label}.*valkey.*Open App`),
      });
      expect(card).toBeVisible();
      expect(within(card).queryByText("Stopped")).toBeNull();
      expect(within(card).queryByText("Image pending")).toBeNull();
    },
  );

  it("does not mistake a loading Helm release for a stopped App", async () => {
    setupHelmOverview().mockReturnValue(new Promise(() => {}));
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(
      await screen.findByRole("link", {
        name: /Test.*Loading.*Loading Helm release.*Open App/,
      }),
    ).toBeVisible();
    expect(screen.queryByText("Stopped")).toBeNull();
  });

  it("offers Helm configuration only when the release is absent", async () => {
    setupHelmOverview().mockRejectedValue(
      new ApiError(404, { code: "HelmAppNotFound" }),
    );
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(
      await screen.findByRole("link", {
        name: /Test.*Not deployed.*Chart not configured.*Configure Helm/i,
      }),
    ).toBeVisible();
    expect(screen.queryByText("Image pending")).toBeNull();
  });

  it.each([403, 404, 503])(
    "shows Helm read failure %i instead of cached success",
    async (status) => {
      const read = setupHelmOverview();
      const { client, Wrapper } = wrapper();
      render(<ApplicationOverviewPage />, { wrapper: Wrapper });
      await screen.findByRole("link", { name: /Test.*Applied.*Open App/ });

      read.mockRejectedValue(
        new ApiError(status, {
          detail: "The Helm release could not be read.",
        }),
      );
      await client.invalidateQueries({
        queryKey: ["helm-release", "application-1", "environment-1"],
      });
      expect(
        await screen.findByRole("link", {
          name: /Test.*Unknown.*Helm release unavailable.*could not be read.*Open App/,
        }),
      ).toBeVisible();
      expect(screen.queryByText("Applied")).toBeNull();
      expect(screen.queryByText("Configure Helm")).toBeNull();
    },
  );

  it("refreshes the overview when the shared Helm release query changes", async () => {
    const read = setupHelmOverview();
    const { client, Wrapper } = wrapper();
    render(<ApplicationOverviewPage />, { wrapper: Wrapper });
    await screen.findByRole("link", { name: /Test.*Applied.*Open App/ });

    const previous = client.getQueryData<HelmReleaseStatus>([
      "helm-release",
      "application-1",
      "environment-1",
    ]);
    expect(previous).toBeDefined();
    read.mockResolvedValue({
      ...previous!,
      generation: 2,
      action: "disable",
      desiredEnabled: false,
    });
    await client.invalidateQueries({
      queryKey: ["helm-release", "application-1", "environment-1"],
    });
    expect(
      await screen.findByRole("link", {
        name: /Test.*Disabled.*valkey.*Open App/,
      }),
    ).toBeVisible();
  });

  it("hides cached Helm details when scoped access disappears", async () => {
    const read = setupHelmOverview();
    const { client, Wrapper } = wrapper();
    render(<ApplicationOverviewPage />, { wrapper: Wrapper });
    await screen.findByRole("link", { name: /Test.*Applied.*Open App/ });
    const readsBefore = read.mock.calls.length;

    vi.mocked(api.capabilities).mockResolvedValue({
      features: { helmDeployments: true },
      capabilities: [
        {
          scopeType: "environment",
          scopeId: "other-environment",
          actions: ["helm.read"],
        },
      ],
    });
    await client.invalidateQueries({ queryKey: ["capabilities"] });
    expect(
      await screen.findByRole("link", {
        name: /Test.*Unavailable.*Helm release access required.*Open App/,
      }),
    ).toBeVisible();
    expect(screen.queryByText("Applied")).toBeNull();
    expect(screen.queryByText(/valkey ·/)).toBeNull();
    expect(read).toHaveBeenCalledTimes(readsBefore);
  });

  it("does not read Helm releases when the feature is unavailable", async () => {
    const read = setupHelmOverview();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: { helmDeployments: false },
      capabilities: [
        { scopeType: "platform", scopeId: "platform", actions: ["helm.read"] },
      ],
    });
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });
    expect(
      await screen.findByRole("link", {
        name: /Test.*Unavailable.*Helm Apps unavailable/,
      }),
    ).toBeVisible();
    expect(read).not.toHaveBeenCalled();
  });

  it("reads only visible placed Helm instances in the App's project", async () => {
    const read = setupHelmOverview();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: { helmDeployments: true },
      capabilities: [
        { scopeType: "platform", scopeId: "platform", actions: ["helm.read"] },
      ],
    });
    vi.mocked(api.environments).mockResolvedValue({
      items: [
        {
          id: "environment-1",
          projectId: "project-1",
          name: "Test",
          namespace: "test",
        },
        {
          id: "unplaced",
          projectId: "project-1",
          name: "Unplaced",
          namespace: "unplaced",
        },
        {
          id: "other-project-env",
          projectId: "other-project",
          name: "Other",
          namespace: "other",
        },
      ],
    });
    const placement = await api.environmentApps("environment-1");
    vi.mocked(api.environmentApps).mockImplementation(async (id) =>
      id === "environment-1" ? placement : { items: [] },
    );
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });
    await screen.findByRole("link", { name: /Test.*Applied.*Open App/ });
    expect(read).toHaveBeenCalledTimes(1);
    expect(read).toHaveBeenCalledWith("application-1", "environment-1");
  });

  it("wraps every App overview section at narrow viewports", async () => {
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    const sections = await screen.findByRole("navigation", {
      name: "App sections",
    });
    expect(sections).toHaveClass("to-580:flex-wrap");
    expect(sections).not.toHaveClass("to-580:overflow-x-auto");
  });

  it("deletes an unused App through typed confirmation", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: { builds: false, builder: false, helmDeployments: false },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-1",
          actions: ["applications:delete"],
        },
      ],
    });
    const remove = vi.spyOn(api, "deleteApplication").mockResolvedValue();
    const user = userEvent.setup();
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    await user.click(await screen.findByRole("button", { name: /Delete App/ }));
    const confirm = screen.getByRole("button", { name: "Delete App" });
    expect(confirm).toBeDisabled();
    await user.type(
      screen.getByRole("textbox", { name: "Confirm deletion" }),
      "Payments API",
    );
    await user.click(confirm);

    await waitFor(() =>
      expect(remove).toHaveBeenCalledWith(
        "application-1",
        "Payments API",
        expect.any(String),
      ),
    );
    await waitFor(() =>
      expect(navigate).toHaveBeenCalledWith({
        to: "/projects/$projectId",
        params: { projectId: "project-1" },
      }),
    );
  });

  it("restores the durable OCI source after reload without URL state", async () => {
    vi.mocked(api.application).mockResolvedValue({
      id: "application-1",
      projectId: "project-1",
      name: "Payments API",
      sourceKind: "oci",
    });
    const user = userEvent.setup();
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(await screen.findByText("OCI image")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Source & build" }));
    expect(screen.getByText("Existing image").closest("li")).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(screen.getByText("Deploy an existing image")).toBeVisible();
  });

  it("shows the durable source and prevents accidental source-type drift", async () => {
    const user = userEvent.setup();
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(
      await screen.findByRole("heading", { name: "Payments API" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Overview" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(
      screen.getByRole("link", { name: /Test.*Stopped.*Configure App/ }),
    ).toBeVisible();
    expect(screen.queryByRole("link", { name: /Deploy App/i })).toBeNull();
    expect(screen.queryByRole("link", { name: /New deployment/i })).toBeNull();

    await user.click(screen.getByRole("button", { name: "Source & build" }));
    const sourceList = screen.getByRole("list", {
      name: "App source",
    });
    const kinds = within(sourceList).getAllByRole("listitem");
    expect(kinds.map((item) => item.textContent)).toEqual([
      "GitHub / Dockerfile",
      "Existing image",
      "Git SSH",
      "Helm chart",
    ]);
    // The source kind is fixed at creation, so nothing in this list may be
    // actionable — no button, no link, no tab role promising a switch.
    expect(within(sourceList).queryByRole("button")).toBeNull();
    expect(within(sourceList).queryByRole("link")).toBeNull();
    expect(within(sourceList).queryByRole("tab")).toBeNull();
    expect(kinds[0]).toHaveAttribute("aria-current", "true");
    expect(kinds[1]).not.toHaveAttribute("aria-current");
  });

  it("keeps GitHub source configuration editable while builder capacity is unavailable", async () => {
    routeSearch.tab = "source";
    routeSearch.source = "github";
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        builds: false,
        builder: false,
        githubAppSetup: true,
      },
      featureStates: { builds: "unavailable", builder: "unavailable" },
      capabilities: [
        {
          scopeType: "application",
          scopeId: "application-1",
          actions: ["app-sources:read", "app-sources:write"],
        },
      ],
    });

    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(
      await screen.findByText("Builder runtime unavailable"),
    ).toBeVisible();
    expect(
      screen.getByRole("combobox", { name: "GitHub installation" }),
    ).toBeVisible();
    expect(screen.queryByText("Source builds are disabled")).toBeNull();
    expect(api.buildDefinitions).toHaveBeenCalledWith("application-1");
  });

  it("clears Helm environment selection when environment access disappears", async () => {
    vi.mocked(api.application).mockResolvedValue({
      id: "application-1",
      projectId: "project-1",
      name: "Payments API",
      sourceKind: "helm",
    });
    const user = userEvent.setup();
    const { client, Wrapper } = wrapper();
    render(<ApplicationOverviewPage />, { wrapper: Wrapper });

    await screen.findByRole("heading", { name: "Payments API" });
    await user.click(screen.getByRole("button", { name: "Source & build" }));
    const environment = await screen.findByRole("combobox", {
      name: /Environment/,
    });
    await selectOption(environment, "environment-1");
    expect(environment).toHaveValue("environment-1");

    client.setQueryData(["environments"], { items: [] });

    await waitFor(() => expect(environment).toHaveValue(""));
  });

  it("keeps an environment-scoped Helm selection while placements load", async () => {
    vi.mocked(api.application).mockResolvedValue({
      id: "application-1",
      projectId: "project-1",
      name: "Payments API",
      sourceKind: "helm",
    });
    routeParams.projectId = "project-1";
    routeSearch.tab = "source";
    routeSearch.source = "helm";
    routeSearch.environmentId = "environment-1";
    let resolvePlacements!: (
      value: Awaited<ReturnType<typeof api.environmentApps>>,
    ) => void;
    vi.mocked(api.environmentApps).mockReturnValue(
      new Promise((resolve) => {
        resolvePlacements = resolve;
      }),
    );

    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    await waitFor(() =>
      expect(api.environmentApps).toHaveBeenCalledWith("environment-1"),
    );
    resolvePlacements({
      items: [
        {
          projectId: "project-1",
          environmentId: "environment-1",
          applicationId: "application-1",
          applicationName: "Payments API",
          applicationSlug: "payments-api",
          state: "draft",
          desiredState: "stopped",
          createdAt: "2026-08-23T00:00:00Z",
          updatedAt: "2026-08-23T00:00:00Z",
        },
      ],
    });

    const environment = await screen.findByRole("combobox", {
      name: /Environment/,
    });
    await waitFor(() => expect(environment).toHaveValue("environment-1"));
  });

  it("offers Helm only in Environments where the App is actually placed", async () => {
    vi.mocked(api.application).mockResolvedValue({
      id: "application-1",
      projectId: "project-1",
      name: "Payments API",
      sourceKind: "helm",
    });
    vi.mocked(api.environments).mockResolvedValue({
      items: [
        {
          id: "environment-1",
          projectId: "project-1",
          name: "Test",
          namespace: "test",
        },
        {
          id: "environment-2",
          projectId: "project-1",
          name: "Production",
          namespace: "production",
        },
      ],
    });
    vi.mocked(api.environmentApps).mockImplementation(async (environmentId) =>
      environmentId === "environment-1"
        ? {
            items: [
              {
                projectId: "project-1",
                environmentId,
                applicationId: "application-1",
                applicationName: "Payments API",
                applicationSlug: "payments-api",
                state: "draft",
                desiredState: "stopped",
                createdAt: "2026-08-23T00:00:00Z",
                updatedAt: "2026-08-23T00:00:00Z",
              },
            ],
          }
        : { items: [] },
    );
    const user = userEvent.setup();
    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    await screen.findByRole("heading", { name: "Payments API" });
    await user.click(screen.getByRole("button", { name: "Source & build" }));

    const environment = screen.getByRole("combobox", { name: /Environment/ });
    await openSelect(environment);
    expect(screen.getByRole("option", { name: "Test" })).toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: "Production" }),
    ).not.toBeInTheDocument();
  });

  it("rejects an environment-scoped URL when the App is not placed there", async () => {
    routeParams.projectId = "project-1";
    routeParams.environmentId = "environment-1";
    vi.mocked(api.environmentApps).mockResolvedValue({ items: [] });

    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(
      await screen.findByText("Environment App unavailable"),
    ).toBeVisible();
  });

  it("resets workspace state when navigating to another application", async () => {
    vi.mocked(api.application).mockResolvedValue({
      id: "application-1",
      projectId: "project-1",
      name: "Payments API",
      sourceKind: "helm",
    });
    const user = userEvent.setup();
    const { Wrapper } = wrapper();
    const view = render(<ApplicationOverviewPage />, { wrapper: Wrapper });

    await screen.findByRole("heading", { name: "Payments API" });
    await user.click(screen.getByRole("button", { name: "Source & build" }));
    await selectOption(
      await screen.findByRole("combobox", { name: /Environment/ }),
      "environment-1",
    );

    routeParams.applicationId = "application-2";
    view.rerender(<ApplicationOverviewPage />);

    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Overview" })).toHaveAttribute(
        "aria-current",
        "page",
      ),
    );
    expect(
      screen.queryByRole("combobox", { name: /Environment/ }),
    ).not.toBeInTheDocument();
  });

  it("summarizes the active GitHub build source", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: { builds: true, builder: true },
      capabilities: [
        {
          role: "developer",
          scopeType: "project",
          scopeId: "project-1",
          actions: ["app-sources:read"],
        },
      ],
    });
    vi.mocked(api.buildDefinitions).mockResolvedValue({
      items: [
        {
          id: "definition-1",
          projectId: "project-1",
          applicationId: "application-1",
          sourceKind: "github",
          installationId: "installation-1",
          repositoryId: "repository-1",
          triggerRef: "refs/heads/main",
          contextPath: ".",
          dockerfilePath: "Dockerfile",
          platforms: ["linux/amd64"],
          registry: {
            targetId: "target-1",
            mode: "managed",
            server: "registry.example.com",
            repositoryPrefix: "payments",
          },
          buildArgs: [],
          secretFiles: [],
          sshFiles: [],
          cacheTrustLane: "main",
          cacheImports: 1,
          profile: {
            resource: "standard",
            timeoutSeconds: 900,
            egress: "registry-and-source",
          },
          maxAttempts: 3,
          sourceDigest: `sha256:${"b".repeat(64)}`,
          sourceRevision: 1,
          enabled: true,
          createdAt: "2026-08-12T00:00:00Z",
          updatedAt: "2026-08-12T00:00:00Z",
        },
      ],
      nextCursor: null,
    });

    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });

    expect(await screen.findByText("GitHub / main")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Manage source/ }),
    ).toBeInTheDocument();
  });

  it("shows the current App source before editing it", async () => {
    routeParams.projectId = "project-1";
    routeParams.environmentId = "environment-1";
    const user = userEvent.setup();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: { builds: true, builder: true },
      capabilities: [
        {
          role: "developer",
          scopeType: "project",
          scopeId: "project-1",
          actions: [
            "app-sources:read",
            "app-sources:write",
            "builds:read",
            "builds:retry",
          ],
        },
      ],
    });
    vi.mocked(api.buildDefinitions).mockResolvedValue({
      items: [
        {
          id: "definition-1",
          projectId: "project-1",
          applicationId: "application-1",
          sourceKind: "github",
          installationId: "installation-1",
          repositoryId: "repository-1",
          triggerRef: "refs/tags/v1.2.3",
          contextPath: ".",
          dockerfilePath: "deploy/Dockerfile",
          platforms: ["linux/amd64", "linux/arm64"],
          registry: {
            targetId: "target-1",
            mode: "managed",
            server: "registry.example.com",
            repositoryPrefix: "payments",
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
          sourceDigest: `sha256:${"c".repeat(64)}`,
          sourceRevision: 2,
          enabled: true,
          createdAt: "2026-08-12T00:00:00Z",
          updatedAt: "2026-08-12T00:00:00Z",
        },
      ],
      nextCursor: null,
    });
    vi.spyOn(api, "buildAttempts").mockResolvedValue({
      items: [
        {
          id: "attempt-1",
          sourceId: "definition-1",
          projectId: "project-1",
          applicationId: "application-1",
          commitSha: "d".repeat(40),
          gitRef: "refs/tags/v1.2.3",
          generation: 2,
          state: "succeeded",
          executionAttempts: 1,
          maxAttempts: 3,
          createdAt: "2026-08-12T00:00:00Z",
          updatedAt: "2026-08-12T00:05:00Z",
        },
      ],
      nextCursor: null,
    });
    vi.mocked(api.deployments).mockResolvedValue({
      items: [
        {
          id: "deployment-1",
          environmentId: "environment-1",
          applicationId: "application-1",
          image: `registry.example.com/payments@sha256:${"f".repeat(64)}`,
          replicas: 1,
          port: 8080,
          runtime: {
            replicas: 1,
            ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
            resources: { requests: { cpu: "50m", memory: "100Mi" } },
          },
          state: "stopped",
          operationId: "operation-1",
          generation: 1,
          createdAt: "2026-08-12T00:00:00Z",
          updatedAt: "2026-08-12T00:00:00Z",
        },
      ],
    });
    const deploy = vi
      .spyOn(api, "deploySourceBuild")
      .mockResolvedValueOnce({
        build: {
          id: "attempt-deploy",
          sourceId: "definition-1",
          projectId: "project-1",
          applicationId: "application-1",
          commitSha: "e".repeat(40),
          gitRef: "refs/tags/v1.2.3",
          generation: 3,
          state: "queued",
          executionAttempts: 0,
          maxAttempts: 3,
          createdAt: "2026-08-12T00:06:00Z",
          updatedAt: "2026-08-12T00:06:00Z",
        },
        intentId: "intent-deploy",
        sequence: 1,
      })
      .mockResolvedValueOnce({
        build: {
          id: "attempt-rebuild",
          sourceId: "definition-1",
          projectId: "project-1",
          applicationId: "application-1",
          commitSha: "d".repeat(40),
          gitRef: "refs/tags/v1.2.3",
          generation: 4,
          state: "queued",
          executionAttempts: 0,
          maxAttempts: 3,
          createdAt: "2026-08-12T00:07:00Z",
          updatedAt: "2026-08-12T00:07:00Z",
        },
        intentId: "intent-rebuild",
        sequence: 2,
      });

    render(<ApplicationOverviewPage />, { wrapper: wrapper().Wrapper });
    await screen.findByRole("heading", { name: "Payments API" });
    await user.click(screen.getByRole("button", { name: "Source & build" }));

    expect(
      await screen.findByText("Connected GitHub source"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("GitHub / v1.2.3 · deploy/Dockerfile"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Edit and save the App source below/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Build history" }),
    ).toBeVisible();
    expect(screen.getByText("Generation 2")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Deploy" }));
    await waitFor(() =>
      expect(deploy).toHaveBeenCalledWith(
        "deployment-1",
        "deploy",
        undefined,
        expect.any(String),
      ),
    );
    await user.click(screen.getByRole("button", { name: "Rebuild" }));
    await waitFor(() =>
      expect(deploy).toHaveBeenCalledWith(
        "deployment-1",
        "rebuild",
        "attempt-1",
        expect.any(String),
      ),
    );
    await user.click(screen.getByRole("button", { name: "Disconnect source" }));
    await user.type(screen.getByLabelText("Confirm deletion"), "DISCONNECT");
    await user.click(
      screen.getAllByRole("button", { name: "Disconnect source" }).at(-1)!,
    );
    await waitFor(() =>
      expect(api.disconnectBuildDefinition).toHaveBeenCalledWith(
        "application-1",
        "definition-1",
        expect.any(String),
      ),
    );
  });
});
