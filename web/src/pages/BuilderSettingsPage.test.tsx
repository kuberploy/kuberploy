import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { BuilderPlatformSettings } from "../api/types";
import { BuilderSettingsPage } from "./BuilderSettingsPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const settings: BuilderPlatformSettings = {
  revision: 2,
  nodeIsolation: false,
  maxConcurrentBuilders: 1,
  checkoutResources: { cpuRequest: "100m", memoryRequest: "128Mi", ephemeralStorageRequest: "1Gi", cpuLimit: "1", memoryLimit: "512Mi", ephemeralStorageLimit: "2Gi" },
  dindResources: { cpuRequest: "500m", memoryRequest: "1Gi", ephemeralStorageRequest: "10Gi", cpuLimit: "4", memoryLimit: "8Gi", ephemeralStorageLimit: "50Gi" },
  agentResources: { cpuRequest: "250m", memoryRequest: "256Mi", ephemeralStorageRequest: "1Gi", cpuLimit: "4", memoryLimit: "4Gi", ephemeralStorageLimit: "10Gi" },
  updatedAt: "2026-08-24T12:00:00Z",
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><BuilderSettingsPage /></QueryClientProvider>);
}

describe("builder platform settings", () => {
  it("saves concurrency, isolation, and per-container resources from one dedicated page", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "builderPlatformSettings").mockResolvedValue(settings);
    const update = vi.spyOn(api, "updateBuilderPlatformSettings").mockResolvedValue({ ...settings, revision: 3, nodeIsolation: true, maxConcurrentBuilders: 3 });
    renderPage();

    await screen.findByRole("heading", { name: "Source builders" });
    const concurrency = screen.getByLabelText(/Maximum concurrent builders/);
    await user.clear(concurrency);
    await user.type(concurrency, "3");
    await user.click(screen.getByRole("checkbox", { name: /Enable node selector/ }));
    await user.click(screen.getByRole("button", { name: "Save builder settings" }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0]?.[0]).toBe(2);
    expect(update.mock.calls[0]?.[1]).toMatchObject({ nodeIsolation: true, maxConcurrentBuilders: 3 });
    expect(update.mock.calls[0]?.[1].dindResources).toEqual(settings.dindResources);
  });
});

