import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { BasicAuthBindingPicker } from "./BasicAuthBindingPicker";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("BasicAuth binding picker", () => {
  it("offers only the exact-scope active users file delivery", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({
      items: [
        {
          id: "11111111-1111-4111-8111-111111111111",
          applicationId: "app-a",
          environmentId: "env-a",
          name: "auth-users",
          provider: "sealed-secrets",
          state: "ready",
          activeVersion: 3,
          createdBy: "user-a",
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
        {
          id: "22222222-2222-4222-8222-222222222222",
          applicationId: "app-a",
          environmentId: "env-b",
          name: "other-environment",
          provider: "sealed-secrets",
          state: "ready",
          activeVersion: 1,
          createdBy: "user-a",
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
      ],
    });
    vi.spyOn(api, "runtimeSecretBinding").mockResolvedValue({
      id: "11111111-1111-4111-8111-111111111111",
      applicationId: "app-a",
      environmentId: "env-a",
      name: "auth-users",
      provider: "sealed-secrets",
      state: "ready",
      activeVersion: 3,
      createdBy: "user-a",
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:00:00Z",
      versions: [
        {
          id: "version-a",
          number: 3,
          state: "active",
          deliveries: [
            {
              sourceKey: "users",
              kind: "file",
              filePath: "/var/run/secrets/kuberploy/traefik-basic-auth/users",
              fileMode: 256,
            },
          ],
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
      ],
    });
    const onChange = vi.fn();
    render(
      <QueryClientProvider client={new QueryClient()}>
        <BasicAuthBindingPicker
          applicationId="app-a"
          environmentId="env-a"
          value={{
            bindingId: "00000000-0000-4000-8000-000000000000",
            name: "select-runtime-secret",
            key: "users",
            version: 1,
          }}
          onChange={onChange}
        />
      </QueryClientProvider>,
    );
    expect(screen.queryByText(/other-environment/)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("textbox", { name: /binding/i }),
    ).not.toBeInTheDocument();
    await selectOption(
      screen.getByRole("combobox", { name: "BasicAuth users binding" }),
      "11111111-1111-4111-8111-111111111111",
    );
    expect(onChange).toHaveBeenCalledWith({
      bindingId: "11111111-1111-4111-8111-111111111111",
      name: "auth-users",
      key: "users",
      version: 3,
    });
  });

  it("refreshes detail metadata when the active version changes", async () => {
    let listCall = 0;
    const list = vi
      .spyOn(api, "runtimeSecretBindings")
      .mockImplementation(async () => {
        listCall += 1;
        const activeVersion = listCall === 1 ? 3 : 4;
        return {
          items: [
            {
              id: "11111111-1111-4111-8111-111111111111",
              applicationId: "app-a",
              environmentId: "env-a",
              name: "auth-users",
              provider: "sealed-secrets",
              state: "ready",
              activeVersion,
              createdBy: "user-a",
              createdAt: "2026-08-09T00:00:00Z",
              updatedAt: "2026-08-09T00:00:00Z",
            },
          ],
        };
      });
    const detail = vi
      .spyOn(api, "runtimeSecretBinding")
      .mockImplementation(async () => {
        const activeVersion = listCall === 1 ? 3 : 4;
        return {
          id: "11111111-1111-4111-8111-111111111111",
          applicationId: "app-a",
          environmentId: "env-a",
          name: "auth-users",
          provider: "sealed-secrets",
          state: "ready",
          activeVersion,
          createdBy: "user-a",
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
          versions: [
            {
              id: `version-${activeVersion}`,
              number: activeVersion,
              state: "active",
              deliveries: [
                {
                  sourceKey: "users",
                  kind: "file",
                  filePath:
                    "/var/run/secrets/kuberploy/traefik-basic-auth/users",
                  fileMode: 256,
                },
              ],
              createdAt: "2026-08-09T00:00:00Z",
              updatedAt: "2026-08-09T00:00:00Z",
            },
          ],
        };
      });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <BasicAuthBindingPicker
          applicationId="app-a"
          environmentId="env-a"
          value={{
            bindingId: "11111111-1111-4111-8111-111111111111",
            name: "auth-users",
            key: "users",
            version: 3,
          }}
          onChange={vi.fn()}
        />
      </QueryClientProvider>,
    );

    const select = screen.getByRole("combobox", {
      name: "BasicAuth users binding",
    });
    await openSelect(select);
    expect(
      await screen.findByRole("option", { name: "auth-users · v3" }),
    ).toBeInTheDocument();
    await queryClient.invalidateQueries({
      queryKey: ["basic-auth-bindings", "app-a", "env-a"],
    });

    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(detail).toHaveBeenCalledTimes(2));
    expect(
      await screen.findByRole("option", { name: "auth-users · v4" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: "auth-users · v3" }),
    ).not.toBeInTheDocument();
  });
});
