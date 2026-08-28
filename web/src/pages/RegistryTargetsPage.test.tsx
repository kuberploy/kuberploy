import { selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Capabilities, RegistryTarget } from "../api/types";
import { RegistryTargetsPage } from "./RegistryTargetsPage";

const target: RegistryTarget = {
  id: "target-primary",
  name: "Primary",
  mode: "external",
  endpoint: "registry.example.test",
  repositoryPrefix: "tenant",
  pullCredentialRef: "credentials/pull",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
};

beforeEach(() => {
  vi.spyOn(api, "me").mockResolvedValue({
    id: "user-admin",
    displayName: "Admin",
    role: "platform-admin",
    authentication: { kind: "session" },
  });
  vi.spyOn(api, "registryTargets").mockResolvedValue({
    items: [target],
    truncated: false,
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPage(capabilities: Capabilities) {
  vi.spyOn(api, "capabilities").mockResolvedValue(capabilities);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RegistryTargetsPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

const targetCapability = (action: string) => ({
  role: "platform-admin" as const,
  scopeType: "platform" as const,
  scopeId: "platform",
  actions: [action],
});

describe("registry target management", () => {
  it("stays disabled when the feature is false", async () => {
    renderPage({
      features: { registry: false },
      capabilities: [targetCapability("registry-targets:read")],
    });

    expect(
      await screen.findByText("Registry management is not enabled"),
    ).toBeInTheDocument();
    expect(api.registryTargets).not.toHaveBeenCalled();
  });

  it("does not treat broad top-level actions as platform scope", async () => {
    renderPage({
      actions: ["registry-targets:read", "registry-targets:write"],
      features: { registry: true },
      capabilities: [],
    });

    expect(
      await screen.findByText("Platform registry access required"),
    ).toBeInTheDocument();
    expect(api.registryTargets).not.toHaveBeenCalled();
  });

  it("shows metadata without external lifecycle controls", async () => {
    renderPage({
      features: { registry: true },
      capabilities: [targetCapability("registry-targets:read")],
    });

    expect(
      await screen.findByText("registry.example.test"),
    ).toBeInTheDocument();
    expect(screen.getByText("Registry operator")).toBeInTheDocument();
    expect(screen.getByText("credentials/pull")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /delete|garbage|cleanup/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText("Registry target metadata is read-only"),
    ).toBeInTheDocument();
  });

  it("submits reference-only target metadata with one caller-stable key", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "createRegistryTarget").mockResolvedValue({
      ...target,
      id: "target-created",
      name: "New target",
      mode: "managed",
      endpoint: "managed.registry.test",
      repositoryPrefix: "team",
      pullCredentialRef: "credentials/managed-pull",
    });
    renderPage({
      features: { registry: true },
      capabilities: [
        targetCapability("registry-targets:read"),
        targetCapability("registry-targets:write"),
      ],
    });

    await screen.findByText("Configured OCI endpoints");
    await user.type(screen.getByLabelText(/^Name/), "New target");
    await selectOption(screen.getByLabelText(/^Mode/), "managed");
    await user.type(
      screen.getByLabelText(/^Endpoint/),
      "managed.registry.test",
    );
    await user.type(screen.getByLabelText(/^Repository prefix/), "team");
    await user.type(
      screen.getByLabelText(/^Pull credential reference/),
      "credentials/managed-pull",
    );
    await user.click(screen.getByRole("button", { name: "Add target" }));

    await waitFor(() =>
      expect(api.createRegistryTarget).toHaveBeenCalledWith(
        {
          name: "New target",
          mode: "managed",
          endpoint: "managed.registry.test",
          repositoryPrefix: "team",
          pullCredentialRef: "credentials/managed-pull",
        },
        expect.any(String),
      ),
    );
    const [input, key] =
      vi.mocked(api.createRegistryTarget).mock.calls[0] ?? [];
    expect(JSON.stringify(input)).not.toMatch(/password|token|secretValue/);
    expect(key).toHaveLength(36);
  });

  it("blocks saving a target removed by a catalog refresh", async () => {
    const user = userEvent.setup();
    const queryClient = renderPage({
      features: { registry: true },
      capabilities: [
        targetCapability("registry-targets:read"),
        targetCapability("registry-targets:write"),
      ],
    });
    await user.click(
      await screen.findByRole("button", { name: /Edit metadata/ }),
    );

    vi.mocked(api.registryTargets).mockResolvedValueOnce({
      items: [],
      truncated: false,
    });
    await queryClient.invalidateQueries({ queryKey: ["registry-targets"] });

    expect(
      await screen.findByText(
        "This registry target changed or is no longer available. Reload the catalog before saving it.",
      ),
    ).toBeVisible();
    expect(screen.getByRole("button", { name: "Save target" })).toBeDisabled();
  });

  it("keeps a reopened target editor open after its older save completes", async () => {
    const user = userEvent.setup();
    let resolveUpdate!: (value: RegistryTarget) => void;
    vi.spyOn(api, "updateRegistryTarget").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveUpdate = resolve;
        }),
    );
    renderPage({
      features: { registry: true },
      capabilities: [
        targetCapability("registry-targets:read"),
        targetCapability("registry-targets:write"),
      ],
    });

    await user.click(
      await screen.findByRole("button", { name: /Edit metadata/ }),
    );
    await user.click(screen.getByRole("button", { name: "Save target" }));
    await waitFor(() =>
      expect(api.updateRegistryTarget).toHaveBeenCalledOnce(),
    );
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: /Edit metadata/ }));

    resolveUpdate(target);

    await waitFor(() => expect(api.registryTargets).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("button", { name: "Save target" })).toBeVisible();
  });

  it("keeps mutations hidden for a non-human principal", async () => {
    vi.mocked(api.me).mockResolvedValue({
      id: "service-account",
      displayName: "Registry bot",
      role: "project-admin",
      authentication: {
        kind: "service-account",
        serviceAccountId: "service-account",
        tokenId: "token",
        scopes: ["app.read", "app.edit"],
        expiresAt: "2026-08-09T01:00:00Z",
      },
    });
    renderPage({
      features: { registry: true },
      capabilities: [
        targetCapability("registry-targets:read"),
        targetCapability("registry-targets:write"),
      ],
    });

    expect(
      await screen.findByText("Registry target metadata is read-only"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Add target" }),
    ).not.toBeInTheDocument();
  });
});
