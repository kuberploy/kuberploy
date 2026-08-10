import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import {
  RuntimeSecretReferencePicker,
  type RuntimeSecretReferenceDraft,
} from "./RuntimeSecretReferencePicker";

const binding = {
  id: "44444444-4444-7444-8444-444444444444",
  applicationId: "application-1",
  environmentId: "environment-1",
  name: "database",
  provider: "sealed-secrets" as const,
  state: "ready" as const,
  activeVersion: 3,
  createdBy: "11111111-1111-4111-8111-111111111111",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:00:00Z",
};

function wrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

function Harness() {
  const [value, setValue] = useState<RuntimeSecretReferenceDraft>({
    bindingId: "",
    bindingName: "",
    key: "",
    version: 0,
  });
  return (
    <RuntimeSecretReferencePicker
      index={0}
      applicationId="application-1"
      environmentId="environment-1"
      environmentName="DATABASE_PASSWORD"
      value={value}
      onChange={setValue}
      enabled
    />
  );
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("runtime-secret reference picker", () => {
  it("emits only server-selected immutable identity and authorized delivery keys", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({
      items: [
        binding,
        {
          ...binding,
          id: "55555555-5555-4555-8555-555555555555",
          name: "external",
          provider: "external-secrets",
        },
      ],
    });
    vi.spyOn(api, "runtimeSecretBinding").mockResolvedValue({
      ...binding,
      versions: [
        {
          id: "66666666-6666-4666-8666-666666666666",
          number: 3,
          state: "active",
          deliveries: [
            {
              sourceKey: "password",
              kind: "environment",
              environmentName: "DATABASE_PASSWORD",
            },
            {
              sourceKey: "admin",
              kind: "environment",
              environmentName: "OTHER_VARIABLE",
            },
          ],
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
      ],
      // Adversarial extra provider identity must never be rendered or copied.
      targetSecretName: "caller-must-not-see-this",
    } as Awaited<ReturnType<typeof api.runtimeSecretBinding>>);

    render(<Harness />, { wrapper: wrapper() });
    const bindingSelect = screen.getByRole("combobox", {
      name: "Secret variable 1 binding",
    });
    expect(
      await screen.findByRole("option", { name: "database · v3" }),
    ).toBeVisible();
    expect(screen.queryByRole("option", { name: /external/ })).toBeNull();
    await user.selectOptions(bindingSelect, binding.id);

    const key = await screen.findByRole("option", { name: "password" });
    expect(key).toBeVisible();
    expect(screen.queryByRole("option", { name: "admin" })).toBeNull();
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Secret variable 1 key" }),
      "password",
    );

    expect(
      screen.getByLabelText("Secret variable 1 version"),
    ).toHaveTextContent("v3");
    expect(bindingSelect).toHaveValue(binding.id);
    expect(document.body).not.toHaveTextContent("caller-must-not-see-this");
    expect(api.runtimeSecretBindings).toHaveBeenCalledWith(
      "application-1",
      "environment-1",
    );
    expect(api.runtimeSecretBinding).toHaveBeenCalledWith(binding.id);
  });

  it("does not query or offer caller-typed identity while unavailable", () => {
    const list = vi.spyOn(api, "runtimeSecretBindings");
    render(
      <RuntimeSecretReferencePicker
        index={0}
        environmentName="DATABASE_PASSWORD"
        value={{ bindingId: "", bindingName: "", key: "", version: 0 }}
        onChange={vi.fn()}
        enabled={false}
      />,
    );
    expect(
      screen.getByRole("combobox", { name: "Secret variable 1 binding" }),
    ).toBeDisabled();
    expect(list).not.toHaveBeenCalled();
  });
});
