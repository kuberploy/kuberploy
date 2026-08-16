import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  ApplicationRegistryTarget,
  Capability,
  Project,
  RegistryCleanupPlan,
  RegistryTargetMode,
} from "../api/types";
import { RegistryPanel } from "./RegistryPanel";

const application: Application = {
  id: "application-a",
  projectId: "project-a",
  name: "Payments API",
};
const nextApplication: Application = {
  ...application,
  id: "application-b",
  name: "Orders API",
};

const project: Project = {
  id: "project-a",
  teamId: "team-a",
  name: "Payments",
};

function registryTarget(mode: RegistryTargetMode): ApplicationRegistryTarget {
  return {
    target: {
      id: `target-${mode}`,
      name: mode === "managed" ? "Managed registry" : "External registry",
      mode,
      endpoint: `${mode}.registry.test`,
      repositoryPrefix: "payments",
      pullCredentialRef: "credentials/pull",
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:05:00Z",
    },
    policy: {
      registryTargetId: `target-${mode}`,
      serviceId: application.id,
      repository: "payments/api",
      keepLastSuccessful: 10,
      minimumSafetyAgeSeconds: 86_400,
      cacheKeepGenerations: 2,
      cacheUnusedExpirySeconds: 604_800,
      cacheByteQuota: 10_737_418_240,
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:05:00Z",
    },
    catalogObservations: [],
    catalogTruncated: false,
    releases: [],
    releasesTruncated: false,
    cacheGenerations: [],
    cacheGenerationsTruncated: false,
    observedAt: "2026-08-09T00:05:00Z",
  };
}

const cleanupPlan = (state = "preview"): RegistryCleanupPlan => ({
  id: "plan-exact",
  registryTargetId: "target-managed",
  serviceId: application.id,
  planDigest: `sha256:${"a".repeat(64)}`,
  state,
  policy: registryTarget("managed").policy,
  summary: {
    protectedManifests: 4,
    deletedManifests: 2,
    garbageCollectBlobs: 3,
    estimatedBytes: 1024,
    cacheBytesBefore: 4096,
    cacheBytesAfter: 1024,
    cacheQuotaSatisfied: true,
  },
  items: [],
  itemsTruncated: false,
  createdAt: "2026-08-09T00:05:00Z",
});

const grant = (action: string): Capability => ({
  role: "project-admin",
  scopeType: "project",
  scopeId: project.id,
  actions: [action],
});

beforeEach(() => {
  vi.spyOn(api, "applicationRegistry").mockResolvedValue({
    items: [],
    truncated: false,
  });
  vi.spyOn(api, "registryTargets").mockResolvedValue({
    items: [],
    truncated: false,
  });
  vi.spyOn(api, "previewRegistryCleanup").mockResolvedValue(cleanupPlan());
  vi.spyOn(api, "registryCleanupPlan").mockResolvedValue(cleanupPlan());
  vi.spyOn(api, "executeRegistryCleanup").mockResolvedValue(
    cleanupPlan("succeeded"),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPanel({
  capabilities = [grant("registry:read")],
  featureEnabled = true,
  managedFeatureEnabled = false,
  humanSession = true,
}: {
  capabilities?: Capability[];
  featureEnabled?: boolean;
  managedFeatureEnabled?: boolean;
  humanSession?: boolean;
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RegistryPanel
        application={application}
        project={project}
        capabilities={capabilities}
        featureEnabled={featureEnabled}
        managedFeatureEnabled={managedFeatureEnabled}
        humanSession={humanSession}
      />
    </QueryClientProvider>,
  );
  return queryClient;
}

describe("application registry panel", () => {
  it("does not query or render when the registry feature is false", () => {
    renderPanel({ featureEnabled: false });

    expect(api.applicationRegistry).not.toHaveBeenCalled();
    expect(screen.queryByText(/registry/i)).not.toBeInTheDocument();
  });

  it("does not accept a broad action without an effective scoped grant", () => {
    renderPanel({ capabilities: [] });

    expect(screen.getByText("Registry access required")).toBeInTheDocument();
    expect(api.applicationRegistry).not.toHaveBeenCalled();
  });

  it("shows safe empty and unavailable states for an authorized scope", async () => {
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [registryTarget("managed")],
      truncated: false,
    });
    renderPanel();

    expect(
      await screen.findByText("Registry inventory unavailable"),
    ).toBeInTheDocument();
    expect(screen.getByText("No release inventory")).toBeInTheDocument();
    expect(screen.getByText("No cache generations")).toBeInTheDocument();
    expect(screen.getByText("Unavailable")).toBeInTheDocument();
  });

  it("preserves a newer retention draft when the earlier save completes", async () => {
    let resolveSave!: (
      value: Awaited<ReturnType<typeof api.putRegistryPolicy>>,
    ) => void;
    const save = vi.spyOn(api, "putRegistryPolicy").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveSave = resolve;
        }),
    );
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [registryTarget("managed")],
      truncated: false,
    });
    const user = userEvent.setup();
    renderPanel({
      capabilities: [grant("registry:read"), grant("registry-policies:write")],
    });

    await user.click(
      await screen.findByRole("button", { name: "Edit retention policy" }),
    );
    const repository = screen.getByPlaceholderText("payments/service");
    await user.clear(repository);
    await user.type(repository, "payments/first");
    await user.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(save).toHaveBeenCalledOnce());
    await user.clear(repository);
    await user.type(repository, "payments/newer");

    resolveSave({} as Awaited<ReturnType<typeof api.putRegistryPolicy>>);
    await waitFor(() => expect(repository).toHaveValue("payments/newer"));
  });

  it("never exposes cleanup for an external target", async () => {
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [registryTarget("external")],
      truncated: false,
    });
    const allActions = [
      "registry:read",
      "registry-cleanup:preview",
      "registry-cleanup:execute",
    ].map(grant);
    renderPanel({
      capabilities: allActions,
      managedFeatureEnabled: true,
    });

    expect(await screen.findByText("Registry operator")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Create preview" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Execute managed cleanup" }),
    ).not.toBeInTheDocument();
    expect(api.previewRegistryCleanup).not.toHaveBeenCalled();
  });

  it("keeps managed cleanup hidden until its separate feature is enabled", async () => {
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [registryTarget("managed")],
      truncated: false,
    });
    renderPanel({
      capabilities: [grant("registry:read"), grant("registry-cleanup:preview")],
      managedFeatureEnabled: false,
    });

    await screen.findByText("Managed registry");
    expect(
      screen.queryByRole("button", { name: "Create preview" }),
    ).not.toBeInTheDocument();
  });

  it("requires exact plan confirmation and keeps mutation keys caller-stable", async () => {
    const user = userEvent.setup();
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [registryTarget("managed")],
      truncated: false,
    });
    vi.mocked(api.previewRegistryCleanup).mockResolvedValue(cleanupPlan());
    vi.mocked(api.registryCleanupPlan).mockResolvedValue(cleanupPlan());
    vi.mocked(api.executeRegistryCleanup).mockResolvedValue(
      cleanupPlan("succeeded"),
    );
    renderPanel({
      capabilities: [
        grant("registry:read"),
        grant("registry-cleanup:preview"),
        grant("registry-cleanup:execute"),
      ],
      managedFeatureEnabled: true,
    });

    await user.click(
      await screen.findByRole("button", { name: "Create preview" }),
    );
    await waitFor(() =>
      expect(api.previewRegistryCleanup).toHaveBeenCalledWith(
        application.id,
        "target-managed",
        expect.any(String),
      ),
    );
    const previewKey = vi.mocked(api.previewRegistryCleanup).mock.calls[0]?.[2];
    expect(previewKey).toHaveLength(36);

    const confirmation = await screen.findByLabelText(/Confirm exact plan ID/);
    await user.type(confirmation, "plan-wrong");
    expect(
      screen.getByText("The confirmation does not match this plan."),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Execute managed cleanup" }),
    ).toBeDisabled();
    await user.clear(confirmation);
    await user.type(confirmation, "plan-exact");
    await user.click(
      screen.getByRole("button", { name: "Execute managed cleanup" }),
    );

    await waitFor(() =>
      expect(api.executeRegistryCleanup).toHaveBeenCalledWith(
        "plan-exact",
        "plan-exact",
        expect.any(String),
      ),
    );
    const executeKey = vi.mocked(api.executeRegistryCleanup).mock.calls[0]?.[2];
    expect(executeKey).toHaveLength(36);
  });

  it("ignores a cleanup preview that completes after the application changes", async () => {
    const user = userEvent.setup();
    let resolvePreview:
      | ((
          value: Awaited<ReturnType<typeof api.previewRegistryCleanup>>,
        ) => void)
      | undefined;
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [registryTarget("managed")],
      truncated: false,
    });
    vi.mocked(api.previewRegistryCleanup).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolvePreview = resolve;
        }),
    );
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const view = render(
      <QueryClientProvider client={client}>
        <RegistryPanel
          application={application}
          project={project}
          capabilities={[
            grant("registry:read"),
            grant("registry-cleanup:preview"),
            grant("registry-cleanup:execute"),
          ]}
          featureEnabled
          managedFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    await user.click(
      await screen.findByRole("button", { name: "Create preview" }),
    );
    view.rerender(
      <QueryClientProvider client={client}>
        <RegistryPanel
          application={nextApplication}
          project={project}
          capabilities={[
            grant("registry:read"),
            grant("registry-cleanup:preview"),
            grant("registry-cleanup:execute"),
          ]}
          featureEnabled
          managedFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    await screen.findByText("Managed registry");
    resolvePreview?.(cleanupPlan());
    await waitFor(() =>
      expect(
        screen.queryByLabelText(/Confirm exact plan ID/),
      ).not.toBeInTheDocument(),
    );
  });

  it("keeps every mutation hidden for a non-human principal", async () => {
    vi.mocked(api.applicationRegistry).mockResolvedValue({
      items: [registryTarget("managed")],
      truncated: false,
    });
    renderPanel({
      capabilities: [
        grant("registry:read"),
        grant("registry-policies:write"),
        grant("registry-cleanup:preview"),
        grant("registry-cleanup:execute"),
      ],
      managedFeatureEnabled: true,
      humanSession: false,
    });

    await screen.findByText("Managed registry");
    expect(
      screen.queryByRole("button", { name: "Edit retention policy" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Create preview" }),
    ).not.toBeInTheDocument();
  });

  it("shows a bounded load error and a safe no-policy state", async () => {
    vi.mocked(api.applicationRegistry).mockRejectedValueOnce(
      new Error("inventory unavailable"),
    );
    const first = renderPanel();
    expect(
      await screen.findByText("Could not load artifact inventory"),
    ).toBeInTheDocument();
    first.clear();
    cleanup();

    vi.mocked(api.applicationRegistry).mockResolvedValueOnce({
      items: [],
      truncated: false,
    });
    renderPanel();
    expect(
      await screen.findByText("No registry policy configured"),
    ).toBeInTheDocument();
  });
});
