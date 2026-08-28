import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { CertificateReferencePicker } from "./CertificateReferencePicker";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPicker(
  value: {
    bindingId: string;
    name: string;
    version: number;
  } | null,
  onChange = vi.fn(),
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const Wrapper = ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  render(
    <CertificateReferencePicker
      applicationId="application-1"
      environmentId="environment-1"
      value={value}
      enabled
      onChange={onChange}
    />,
    { wrapper: Wrapper },
  );
  return onChange;
}

describe("certificate reference picker", () => {
  it("offers only ready active immutable versions and emits the typed reference", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "certificateBindings").mockResolvedValue({
      items: [
        {
          id: "binding-ready",
          applicationId: "application-1",
          environmentId: "environment-1",
          name: "public-edge",
          state: "ready",
          activeVersion: 3,
          createdBy: "user-1",
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
        {
          id: "binding-pending",
          applicationId: "application-1",
          environmentId: "environment-1",
          name: "pending-edge",
          state: "provisioning",
          createdBy: "user-1",
          createdAt: "2026-08-09T00:00:00Z",
          updatedAt: "2026-08-09T00:00:00Z",
        },
      ],
    });
    const onChange = renderPicker(null);
    expect(screen.queryByText(/pending-edge/)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("textbox", { name: /certificate/i }),
    ).not.toBeInTheDocument();
    await selectOption(
      screen.getByRole("combobox", {
        name: "Certificate binding and immutable version",
      }),
      "binding-ready@3",
    );
    expect(onChange).toHaveBeenCalledWith({
      bindingId: "binding-ready",
      name: "public-edge",
      version: 3,
    });
  });

  it("preserves an unavailable exact YAML reference without coercing it", async () => {
    vi.spyOn(api, "certificateBindings").mockResolvedValue({ items: [] });
    renderPicker({
      bindingId: "binding-retained",
      name: "retained-edge",
      version: 2,
    });
    await openSelect(
      screen.getByRole("combobox", {
        name: "Certificate binding and immutable version",
      }),
    );
    expect(
      await screen.findByRole("option", {
        name: "Current YAML reference: retained-edge · v2",
      }),
    ).toHaveAttribute("data-disabled");
    expect(
      screen.getByRole("combobox", {
        name: "Certificate binding and immutable version",
      }),
    ).toHaveValue("binding-retained@2");
  });
});
