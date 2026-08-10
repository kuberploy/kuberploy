import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
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
    const option = await screen.findByRole("option", {
      name: "auth-users · v3",
    });
    expect(screen.queryByText(/other-environment/)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("textbox", { name: /binding/i }),
    ).not.toBeInTheDocument();
    await user.selectOptions(
      screen.getByRole("combobox", { name: "BasicAuth users binding" }),
      option,
    );
    expect(onChange).toHaveBeenCalledWith({
      bindingId: "11111111-1111-4111-8111-111111111111",
      name: "auth-users",
      key: "users",
      version: 3,
    });
  });
});
