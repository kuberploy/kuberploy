import { selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
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
  it("emits only server-selected identity and authorized delivery keys", async () => {
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
    await selectOption(bindingSelect, `${binding.id}@3`);

    await selectOption(
      screen.getByRole("combobox", { name: "Secret variable 1 key" }),
      "password",
    );

    expect(
      screen.getByLabelText("Secret variable 1 version"),
    ).toHaveTextContent("v3");
    expect(bindingSelect).toHaveValue(`${binding.id}@3`);
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

  it("refreshes delivery metadata when the binding active version changes", async () => {
    let listCall = 0;
    const list = vi
      .spyOn(api, "runtimeSecretBindings")
      .mockImplementation(async () => {
        listCall += 1;
        return {
          items: [{ ...binding, activeVersion: listCall === 1 ? 3 : 4 }],
        };
      });
    const detail = vi
      .spyOn(api, "runtimeSecretBinding")
      .mockImplementation(async () => {
        const version = listCall === 1 ? 3 : 4;
        return {
          ...binding,
          activeVersion: version,
          versions: [
            {
              id: `version-${version}`,
              number: version,
              state: "active",
              deliveries: [
                {
                  sourceKey: "password",
                  kind: "environment",
                  environmentName: "DATABASE_PASSWORD",
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
    render(<Harness />, {
      wrapper: ({ children }) => (
        <QueryClientProvider client={queryClient}>
          {children}
        </QueryClientProvider>
      ),
    });

    const bindingSelect = screen.getByRole("combobox", {
      name: "Secret variable 1 binding",
    });
    await selectOption(bindingSelect, `${binding.id}@3`);

    await queryClient.invalidateQueries({
      queryKey: [
        "runtime-secret-reference-bindings",
        "application-1",
        "environment-1",
      ],
    });
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(detail).toHaveBeenCalledTimes(2));
    expect(
      screen.getByLabelText("Secret variable 1 version"),
    ).toHaveTextContent("v3");
    expect(
      screen.getByRole("combobox", { name: "Secret variable 1 key" }),
    ).toBeDisabled();
    await selectOption(bindingSelect, `${binding.id}@4`);
    await waitFor(() =>
      expect(
        screen.getByLabelText("Secret variable 1 version"),
      ).toHaveTextContent("v4"),
    );
    expect(
      screen.getByRole("combobox", { name: "Secret variable 1 key" }),
    ).not.toBeDisabled();
  });
});
