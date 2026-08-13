import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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

  it("requires explicit confirmation before deactivation", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user-a",
      displayName: "Admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { middlewareProfiles: true },
      capabilities: [
        {
          scopeType: "platform",
          scopeId: "platform",
          actions: ["deployment-config:write"],
        },
      ],
    });
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ id: "project-a", name: "Payments" }],
    });
    vi.spyOn(api, "environments").mockResolvedValue({
      items: [
        {
          id: "environment-a",
          projectId: "project-a",
          name: "Production",
          namespace: "kp-production",
        },
      ],
    });
    vi.spyOn(api, "applications").mockResolvedValue({
      items: [{ id: "application-a", projectId: "project-a", name: "API" }],
    });
    vi.spyOn(api, "middlewareProfileCatalog").mockResolvedValue({
      items: [
        {
          profile: {
            id: "profile-a",
            name: "secure-headers",
            lifecycle: "active",
            currentRevision: 1,
            createdBy: "user-a",
            createdAt: "2026-08-10T00:00:00Z",
          },
          revision: {
            profileId: "profile-a",
            revision: 1,
            spec: { headers: {} },
            specDigest: `sha256:${"a".repeat(64)}`,
            assignmentsDigest: `sha256:${"b".repeat(64)}`,
            createdBy: "user-a",
            assignments: [{ scope: "application", id: "application-a" }],
            createdAt: "2026-08-10T00:00:00Z",
          },
        },
      ],
    });
    const deactivate = vi
      .spyOn(api, "deactivateMiddlewareProfile")
      .mockResolvedValue({} as never);
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderPage();
    await userEvent.selectOptions(
      await screen.findByLabelText("Environment"),
      "environment-a",
    );
    await userEvent.selectOptions(
      screen.getByLabelText("Application"),
      "application-a",
    );
    await userEvent.click(
      await screen.findByRole("button", { name: "Deactivate" }),
    );
    expect(confirm).toHaveBeenCalledWith(
      expect.stringContaining("secure-headers"),
    );
    expect(deactivate).not.toHaveBeenCalled();
    confirm.mockReturnValue(true);
    await userEvent.click(screen.getByRole("button", { name: "Deactivate" }));
    await waitFor(() =>
      expect(deactivate).toHaveBeenCalledWith(
        "profile-a",
        1,
        expect.any(String),
      ),
    );
  });
});
