import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { MiddlewareProfilesPage } from "./MiddlewareProfilesPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPage() {
  render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <MiddlewareProfilesPage />
    </QueryClientProvider>,
  );
}

describe("middleware profile management", () => {
  it("does not query profile metadata for a service account", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user-a",
      displayName: "Automation",
      role: "developer",
      authentication: {
        kind: "service-account",
        serviceAccountId: "service-a",
        tokenId: "token-a",
        scopes: [],
        expiresAt: "2026-08-10T00:00:00Z",
      },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { middlewareProfiles: true },
    });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [] });
    vi.spyOn(api, "environments").mockResolvedValue({ items: [] });
    vi.spyOn(api, "applications").mockResolvedValue({ items: [] });
    const catalog = vi.spyOn(api, "middlewareProfileCatalog");
    renderPage();
    expect(
      await screen.findByText("Middleware profile management unavailable"),
    ).toBeVisible();
    expect(catalog).not.toHaveBeenCalled();
  });
});
