import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { CertificateIssuerPicker } from "./CertificateIssuerPicker";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPicker(value = "", onChange = vi.fn()) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const Wrapper = ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  render(
    <CertificateIssuerPicker
      applicationId="aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
      environmentId="bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
      hostname="api.example.com"
      value={value}
      enabled
      onChange={onChange}
    />,
    { wrapper: Wrapper },
  );
  return onChange;
}

describe("certificate issuer picker", () => {
  it("offers only server-projected ready issuer identities", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "applicationCertificateIssuers").mockResolvedValue({
      items: [
        {
          name: "kuberploy-letsencrypt-production",
          environment: "production",
          solverTypes: ["dns01", "http01"],
          source: "bootstrap",
        },
      ],
    });
    const onChange = renderPicker();
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    await selectOption(
      screen.getByRole("combobox", { name: "Approved certificate issuer" }),
      "kuberploy-letsencrypt-production",
    );
    expect(onChange).toHaveBeenCalledWith("kuberploy-letsencrypt-production");
    expect(api.applicationCertificateIssuers).toHaveBeenCalledWith(
      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
      "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
      "api.example.com",
    );
  });

  it("preserves an unavailable YAML issuer without allowing free text", async () => {
    vi.spyOn(api, "applicationCertificateIssuers").mockResolvedValue({
      items: [],
    });
    renderPicker("retained-issuer");
    await openSelect(
      screen.getByRole("combobox", { name: "Approved certificate issuer" }),
    );
    expect(
      await screen.findByRole("option", {
        name: "Current YAML issuer: retained-issuer",
      }),
    ).toHaveAttribute("data-disabled");
    expect(
      screen.getByRole("combobox", { name: "Approved certificate issuer" }),
    ).toHaveValue("retained-issuer");
  });
});
