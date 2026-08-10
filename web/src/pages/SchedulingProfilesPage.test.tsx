import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { SchedulingProfilesPage } from "./SchedulingProfilesPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPage() {
  render(
    <QueryClientProvider
      client={
        new QueryClient({
          defaultOptions: {
            queries: { retry: false },
            mutations: { retry: false },
          },
        })
      }
    >
      <SchedulingProfilesPage />
    </QueryClientProvider>,
  );
}

const principal = {
  id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  displayName: "Admin",
  role: "platform-admin",
  authentication: { kind: "session" as const },
};

describe("scheduling profile settings", () => {
  it("never queries the admin catalog outside an enabled human admin session", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      ...principal,
      authentication: {
        kind: "service-account",
        serviceAccountId: "service",
        tokenId: "token",
        scopes: [],
        expiresAt: "2026-08-10T00:00:00Z",
      },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { schedulingProfiles: true },
    });
    const catalog = vi.spyOn(api, "platformSchedulingProfiles");
    renderPage();
    expect(
      await screen.findByText("Scheduling profile management unavailable"),
    ).toBeVisible();
    expect(catalog).not.toHaveBeenCalled();
  });

  it("creates only an assigned immutable profile contract", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { schedulingProfiles: true },
    });
    vi.spyOn(api, "platformSchedulingProfiles").mockResolvedValue({
      items: [],
    });
    const create = vi
      .spyOn(api, "createPlatformSchedulingProfile")
      .mockResolvedValue({
        profile: {
          id: "11111111-1111-4111-8111-111111111111",
          name: "stable-amd64",
          lifecycle: "active",
          currentRevision: 1,
          createdBy: principal.id,
          createdAt: "2026-08-09T00:00:00Z",
        },
        revision: {
          profileId: "11111111-1111-4111-8111-111111111111",
          revision: 1,
          spec: { pod: { nodeSelector: { arch: "amd64" } } },
          specDigest: `sha256:${"a".repeat(64)}`,
          assignmentsDigest: `sha256:${"b".repeat(64)}`,
          createdBy: principal.id,
          assignments: [
            {
              scope: "environment",
              id: "22222222-2222-4222-8222-222222222222",
            },
          ],
          createdAt: "2026-08-09T00:00:00Z",
        },
      });
    renderPage();
    await screen.findByText("Create a profile");
    await user.type(screen.getByLabelText(/^Profile name/), "stable-amd64");
    await user.type(
      screen.getByLabelText(/^Exact assignments/),
      "environment:22222222-2222-4222-8222-222222222222",
    );
    await user.type(screen.getByLabelText(/^Node selectors/), "arch=amd64");
    await user.click(
      screen.getByRole("button", { name: "Add preferred term" }),
    );
    await user.clear(screen.getByLabelText(/^Preferred term 1 weight/));
    await user.type(screen.getByLabelText(/^Preferred term 1 weight/), "75");
    await user.type(
      screen.getByLabelText(/^Term 1 expression 1 key/),
      "topology.kubernetes.io/zone",
    );
    await user.type(
      screen.getByLabelText(/^Term 1 expression 1 values/),
      "zone-a, zone-b",
    );
    await user.click(
      screen.getByRole("button", { name: "Add anti-affinity preset" }),
    );
    await user.selectOptions(
      screen.getByLabelText(/^Preset 1 enforcement/),
      "preferred",
    );
    await user.clear(screen.getByLabelText(/^Preset 1 weight/));
    await user.type(screen.getByLabelText(/^Preset 1 weight/), "40");
    await user.click(
      screen.getByRole("button", { name: "Create immutable profile" }),
    );
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0]?.[0]).toEqual({
      name: "stable-amd64",
      assignments: [
        {
          scope: "environment",
          id: "22222222-2222-4222-8222-222222222222",
        },
      ],
      spec: {
        pod: {
          nodeSelector: { arch: "amd64" },
          preferredNodeAffinity: [
            {
              weight: 75,
              requirements: [
                {
                  key: "topology.kubernetes.io/zone",
                  operator: "In",
                  values: ["zone-a", "zone-b"],
                },
              ],
            },
          ],
          sameApplicationPodAntiAffinity: [
            {
              enforcement: "preferred",
              topologyKey: "kubernetes.io/hostname",
              weight: 40,
            },
          ],
        },
      },
    });
  });
});
