import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import type {
  Capabilities,
  ExternalDNSIntegration,
  Principal,
} from "../api/types";
import { ExternalDNSPage } from "./ExternalDNSPage";

const integration: ExternalDNSIntegration = {
  id: "integration-1",
  slug: "public-dns",
  name: "Public DNS",
  mode: "adopted",
  providerKind: "cloudflare",
  txtOwnerId: "kuberploy.production",
  allowedDomainSuffixes: ["example.com"],
  syncPolicy: "upsert-only",
  destructiveSyncConfirmed: false,
  operatorProfileRef: "operator-profile",
  environmentIds: ["environment-1"],
  createdBy: "user-admin",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

const environment = {
  id: "environment-1",
  projectId: "project-1",
  name: "Production",
  namespace: "project-production",
};

const session: Principal = {
  id: "user-admin",
  displayName: "Admin",
  role: "platform-admin",
  authentication: { kind: "session" },
};

beforeEach(() => {
  vi.spyOn(api, "me").mockResolvedValue(session);
  vi.spyOn(api, "externalDNSIntegrations").mockResolvedValue({
    items: [integration],
    truncated: false,
  });
  vi.spyOn(api, "externalDNSStatus").mockResolvedValue({
    configurationState: "configured",
    controllerReadiness: "unobserved",
    runtimeAvailable: false,
    detail: "No controller observation is available.",
  });
  vi.spyOn(api, "environments").mockResolvedValue({ items: [environment] });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const platformCapability = (action: string) => ({
  role: "platform-admin" as const,
  scopeType: "platform" as const,
  scopeId: "platform",
  actions: [action],
});

function renderPage(capabilities: Capabilities) {
  vi.spyOn(api, "capabilities").mockResolvedValue(capabilities);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ExternalDNSPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

describe("External DNS platform management", () => {
  it("fails closed when capability verification errors", async () => {
    vi.spyOn(api, "capabilities").mockRejectedValue(
      new Error("capability service unavailable"),
    );
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ExternalDNSPage />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByText("Could not verify External DNS access"),
    ).toBeVisible();
    expect(api.externalDNSIntegrations).not.toHaveBeenCalled();
    expect(
      screen.queryByText("External DNS configuration is not enabled"),
    ).toBeNull();
  });

  it("stays hidden when configuration is false", async () => {
    renderPage({
      features: { externalDNSConfiguration: false, externalDNS: false },
      capabilities: [platformCapability("external-dns-integrations:read")],
    });

    expect(
      await screen.findByText("External DNS configuration is not enabled"),
    ).toBeVisible();
    expect(api.externalDNSIntegrations).not.toHaveBeenCalled();
  });

  it("does not treat broad top-level actions or a project grant as platform access", async () => {
    renderPage({
      actions: [
        "external-dns-integrations:read",
        "external-dns-integrations:write",
      ],
      features: { externalDNSConfiguration: true },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-1",
          actions: ["external-dns-integrations:read"],
        },
      ],
    });

    expect(
      await screen.findByText("Platform External DNS access required"),
    ).toBeVisible();
    expect(api.externalDNSIntegrations).not.toHaveBeenCalled();
  });

  it("shows configuration while keeping controller availability unobserved", async () => {
    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [platformCapability("external-dns-integrations:read")],
    });

    expect(await screen.findByText("Public DNS")).toBeVisible();
    expect(screen.getByText("Runtime unavailable")).toBeVisible();
    expect(screen.getAllByText(/unobserved/i).length).toBeGreaterThan(0);
    expect(screen.getByText("operator-profile")).toBeVisible();
    expect(screen.queryByRole("button", { name: /delete/i })).toBeNull();
    expect(
      screen.getByText("External DNS metadata is read-only"),
    ).toBeVisible();
  });

  it("shows runtime availability only for a freshly observed controller", async () => {
    vi.mocked(api.externalDNSStatus).mockResolvedValue({
      configurationState: "configured",
      controllerReadiness: "ready",
      runtimeAvailable: true,
      detail: "The exact controller profile is freshly observed.",
    });
    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: true },
      capabilities: [platformCapability("external-dns-integrations:read")],
    });

    expect(await screen.findByText("Runtime available")).toBeVisible();
    expect(screen.getByText(/^ready$/i)).toBeVisible();
    expect(
      screen.getByText("The exact controller profile is freshly observed."),
    ).toBeVisible();
  });

  it("polls configured runtime readiness until controller observation completes", async () => {
    const status = vi.mocked(api.externalDNSStatus);
    status
      .mockResolvedValueOnce({
        configurationState: "configured",
        controllerReadiness: "unobserved",
        runtimeAvailable: false,
        detail: "Waiting for controller observation.",
      })
      .mockResolvedValueOnce({
        configurationState: "configured",
        controllerReadiness: "ready",
        runtimeAvailable: true,
        detail: "The exact controller profile is freshly observed.",
      });

    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: true },
      capabilities: [platformCapability("external-dns-integrations:read")],
    });

    expect(
      await screen.findByText("Waiting for controller observation."),
    ).toBeVisible();

    await waitFor(() => expect(status).toHaveBeenCalledTimes(2), {
      timeout: 3_000,
    });

    expect(
      await screen.findByText(
        "The exact controller profile is freshly observed.",
      ),
    ).toBeVisible();
  });

  it("reuses the same deactivation idempotency key after a network retry", async () => {
    const user = userEvent.setup();
    const deactivate = vi
      .spyOn(api, "deactivateExternalDNSIntegration")
      .mockRejectedValueOnce(new ApiError(0))
      .mockResolvedValueOnce({ ...integration, lifecycle: "deactivated" });
    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        platformCapability("external-dns-integrations:read"),
        platformCapability("external-dns-integrations:write"),
      ],
    });

    await user.click(await screen.findByRole("button", { name: "Deactivate" }));
    await user.click(
      screen.getByRole("button", { name: "Deactivate integration" }),
    );
    await waitFor(() => expect(deactivate).toHaveBeenCalledTimes(2), {
      timeout: 3_000,
    });
    expect(deactivate.mock.calls[0]?.[0]).toBe(integration.id);
    expect(deactivate.mock.calls[1]?.[0]).toBe(integration.id);
    expect(deactivate.mock.calls[0]?.[1]).toMatch(/^[0-9a-f-]{36}$/);
    expect(deactivate.mock.calls[1]?.[1]).toBe(deactivate.mock.calls[0]?.[1]);
  });

  it("keeps a newer profile editor open after an older deactivation completes", async () => {
    const user = userEvent.setup();
    const second = {
      ...integration,
      id: "integration-2",
      slug: "private-dns",
      name: "Private DNS",
    };
    vi.mocked(api.externalDNSIntegrations).mockResolvedValue({
      items: [integration, second],
      truncated: false,
    });
    let resolveDeactivate!: (
      value: ExternalDNSIntegration & { lifecycle: "deactivated" },
    ) => void;
    vi.spyOn(api, "deactivateExternalDNSIntegration").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveDeactivate = resolve;
        }),
    );
    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        platformCapability("external-dns-integrations:read"),
        platformCapability("external-dns-integrations:write"),
      ],
    });

    expect(await screen.findByText("Private DNS")).toBeVisible();
    await user.click(screen.getAllByRole("button", { name: "Deactivate" })[0]!);
    await user.click(
      screen.getByRole("button", { name: "Deactivate integration" }),
    );
    await user.click(
      screen.getAllByRole("button", { name: /Edit profile/ })[1]!,
    );

    resolveDeactivate({ ...integration, lifecycle: "deactivated" });

    await waitFor(() =>
      expect(api.externalDNSIntegrations).toHaveBeenCalledTimes(2),
    );
    expect(screen.getByRole("heading", { name: "Private DNS" })).toBeVisible();
  });

  it("keeps a reopened profile editor open after its older deactivation completes", async () => {
    const user = userEvent.setup();
    let resolveDeactivate!: (
      value: ExternalDNSIntegration & { lifecycle: "deactivated" },
    ) => void;
    vi.spyOn(api, "deactivateExternalDNSIntegration").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveDeactivate = resolve;
        }),
    );
    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        platformCapability("external-dns-integrations:read"),
        platformCapability("external-dns-integrations:write"),
      ],
    });

    await user.click(await screen.findByRole("button", { name: "Deactivate" }));
    await user.click(
      screen.getByRole("button", { name: "Deactivate integration" }),
    );
    await user.click(screen.getByRole("button", { name: /Edit profile/ }));

    resolveDeactivate({ ...integration, lifecycle: "deactivated" });

    await waitFor(() =>
      expect(api.externalDNSIntegrations).toHaveBeenCalledTimes(2),
    );
    expect(screen.getByRole("button", { name: "Save profile" })).toBeVisible();
  });

  it("keeps a reopened profile editor open after its older save completes", async () => {
    const user = userEvent.setup();
    let resolveUpdate!: (value: ExternalDNSIntegration) => void;
    vi.spyOn(api, "updateExternalDNSIntegration").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveUpdate = resolve;
        }),
    );
    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        platformCapability("external-dns-integrations:read"),
        platformCapability("external-dns-integrations:write"),
      ],
    });

    await user.click(
      await screen.findByRole("button", { name: /Edit profile/ }),
    );
    await user.click(screen.getByRole("button", { name: "Save profile" }));
    await waitFor(() =>
      expect(api.updateExternalDNSIntegration).toHaveBeenCalledOnce(),
    );
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: /Edit profile/ }));

    resolveUpdate(integration);

    await waitFor(() =>
      expect(api.externalDNSIntegrations).toHaveBeenCalledTimes(2),
    );
    expect(screen.getByRole("button", { name: "Save profile" })).toBeVisible();
  });

  it("submits a structured adopted profile with an exact environment", async () => {
    const user = userEvent.setup();
    const create = vi
      .spyOn(api, "createExternalDNSIntegration")
      .mockResolvedValue({ ...integration, id: "integration-created" });
    renderPage({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        platformCapability("external-dns-integrations:read"),
        platformCapability("external-dns-integrations:write"),
      ],
    });

    await screen.findByText("Authorized integration catalog");
    await user.type(screen.getByLabelText(/^DNS slug/), "new-dns");
    await user.type(screen.getByLabelText(/^Display name/), "New DNS");
    await user.type(
      screen.getByLabelText(/^TXT owner/),
      "kuberploy.new",
    );
    await user.type(
      screen.getByLabelText(/^Allowed domain suffixes/),
      "apps.example.com",
    );
    await user.type(
      screen.getByLabelText(/^Operator profile reference/),
      "operator-profile-new",
    );
    await user.click(screen.getByRole("checkbox", { name: /Production/ }));
    await user.click(screen.getByRole("button", { name: "Add profile" }));

    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0]?.[0]).toEqual({
      slug: "new-dns",
      name: "New DNS",
      mode: "adopted",
      providerKind: "cloudflare",
      txtOwnerId: "kuberploy.new",
      allowedDomainSuffixes: ["apps.example.com"],
      syncPolicy: "upsert-only",
      destructiveSyncConfirmed: false,
      operatorProfileRef: "operator-profile-new",
      environmentIds: ["environment-1"],
    });
    expect(create.mock.calls[0]?.[1]).toMatch(/^[0-9a-f-]{36}$/);
  });

  it("blocks saving an integration removed by a catalog refresh", async () => {
    const user = userEvent.setup();
    const queryClient = renderPage({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        platformCapability("external-dns-integrations:read"),
        platformCapability("external-dns-integrations:write"),
      ],
    });
    await user.click(
      await screen.findByRole("button", { name: /Edit profile/ }),
    );

    vi.mocked(api.externalDNSIntegrations).mockResolvedValueOnce({
      items: [],
      truncated: false,
    });
    await queryClient.invalidateQueries({
      queryKey: ["external-dns-integrations"],
    });

    expect(
      await screen.findByText(
        "This integration changed, was deactivated, or is no longer available. Reload the catalog before saving it.",
      ),
    ).toBeVisible();
    expect(screen.getByRole("button", { name: "Save profile" })).toBeDisabled();
  });

  it("keeps mutations human-only even with an exact write capability", async () => {
    vi.mocked(api.me).mockResolvedValue({
      ...session,
      authentication: {
        kind: "service-account",
        serviceAccountId: "service-account-1",
        tokenId: "token-1",
        scopes: ["app.read"],
        expiresAt: "2026-08-10T00:00:00Z",
      },
    });
    renderPage({
      features: { externalDNSConfiguration: true },
      capabilities: [
        platformCapability("external-dns-integrations:read"),
        platformCapability("external-dns-integrations:write"),
      ],
    });

    expect(
      await screen.findByText("External DNS metadata is read-only"),
    ).toBeVisible();
    expect(screen.queryByRole("button", { name: "Add profile" })).toBeNull();
  });
});
