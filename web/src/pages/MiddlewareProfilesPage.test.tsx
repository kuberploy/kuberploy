import { selectOption } from "../test/selectOption";
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
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <MiddlewareProfilesPage />
    </QueryClientProvider>,
  );
  return queryClient;
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
    const unavailable = await screen.findByText(
      "Middleware profile management unavailable",
    );
    expect(unavailable).toBeVisible();
    expect(unavailable.closest('[data-slot="page"]')).toHaveAttribute(
      "data-narrow",
      "true",
    );
    expect(catalog).not.toHaveBeenCalled();
  });

  it("requires explicit confirmation before deletion", async () => {
    const user = userEvent.setup();
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
      .mockRejectedValueOnce(new Error("profile is still referenced"))
      .mockResolvedValue({} as never);
    renderPage();
    await selectOption(
      await screen.findByLabelText("Environment"),
      "environment-a",
    );
    await selectOption(screen.getByLabelText("Application"), "application-a");
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    expect(
      screen.getByRole("alertdialog", {
        name: /Delete middleware profile/,
      }),
    ).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(deactivate).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await user.click(
      screen.getByRole("button", { name: "Delete profile" }),
    );
    await waitFor(() => expect(deactivate).toHaveBeenCalledTimes(1));
    expect(
      await screen.findByText("Profile was not deleted"),
    ).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await user.click(
      screen.getByRole("button", { name: "Delete profile" }),
    );
    await waitFor(() => expect(deactivate).toHaveBeenCalledTimes(2));
    expect(deactivate.mock.calls[1]?.[2]).toBe(deactivate.mock.calls[0]?.[2]);
  });

  it("blocks revising a profile removed by a catalog refresh", async () => {
    const user = userEvent.setup();
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
    vi.spyOn(api, "middlewareProfileCatalog")
      .mockResolvedValueOnce({
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
      })
      .mockResolvedValueOnce({ items: [] });
    const queryClient = renderPage();

    await selectOption(
      await screen.findByLabelText("Environment"),
      "environment-a",
    );
    await selectOption(screen.getByLabelText("Application"), "application-a");
    await user.click(await screen.findByRole("button", { name: "Revise" }));
    await queryClient.invalidateQueries({
      queryKey: ["middleware-profile-catalog"],
    });

    expect(
      await screen.findByText(
        "This profile changed or is no longer active. Reload the current catalog before revising it.",
      ),
    ).toBeVisible();
    expect(
      screen.getByRole("button", { name: "Append revision" }),
    ).toBeDisabled();
  });

  it("clears selected scope when the environment leaves access scope", async () => {
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
    const queryClient = renderPage();
    const user = userEvent.setup();

    const environment = await screen.findByLabelText("Environment");
    await selectOption(environment, "environment-a");
    const application = screen.getByLabelText("Application");
    await selectOption(application, "application-a");
    expect(environment).toHaveValue("environment-a");
    expect(application).toHaveValue("application-a");

    queryClient.setQueryData(["environments"], { items: [] });

    await waitFor(() => {
      expect(environment).toHaveValue("");
      expect(application).toHaveValue("");
    });
  });
});
