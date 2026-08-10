import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { CertificateIssuersPage } from "./CertificateIssuersPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const principal = {
  id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  displayName: "Admin",
  role: "platform-admin",
  authentication: { kind: "session" as const },
};

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
      <CertificateIssuersPage />
    </QueryClientProvider>,
  );
}

describe("certificate issuer administration", () => {
  it("does not query admin metadata for service accounts", async () => {
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
      features: { certificateIssuerManagement: true },
    });
    const catalog = vi.spyOn(api, "platformCertificateIssuers");
    renderPage();
    expect(
      await screen.findByText("Certificate issuer management unavailable"),
    ).toBeVisible();
    expect(catalog).not.toHaveBeenCalled();
  });

  it("creates a closed HTTP-01 profile without accepting an ACME server or credentials", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { certificateIssuerManagement: true },
    });
    vi.spyOn(api, "platformCertificateIssuers").mockResolvedValue({
      items: [],
    });
    const create = vi
      .spyOn(api, "createPlatformCertificateIssuer")
      .mockResolvedValue({
        id: "11111111-1111-4111-8111-111111111111",
        name: "tenant-production",
        lifecycle: "active",
        currentRevision: 1,
        revision: {
          number: 1,
          environment: "production",
          email: "admin@example.com",
          accountPrivateKeySecretName: "tenant-production-account",
          solver: "http01",
          specDigest: `sha256:${"a".repeat(64)}`,
          createdAt: "2026-08-09T00:00:00Z",
        },
        observation: {
          state: "pending",
          updatedAt: "2026-08-09T00:00:00Z",
        },
        createdAt: "2026-08-09T00:00:00Z",
      });
    renderPage();
    await screen.findByText("Create an issuer");
    await user.type(screen.getByLabelText(/^Issuer name/), "tenant-production");
    await user.type(
      screen.getByLabelText(/^ACME account email/),
      "admin@example.com",
    );
    await user.type(
      screen.getByLabelText(/^ACME account Secret name/),
      "tenant-production-account",
    );
    await user.click(screen.getByRole("button", { name: "Create issuer" }));
    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0]?.[0]).toBe("tenant-production");
    expect(create.mock.calls[0]?.[1]).toEqual({
      environment: "production",
      email: "admin@example.com",
      accountPrivateKeySecretName: "tenant-production-account",
      solver: { type: "http01" },
    });
    expect(create.mock.calls[0]?.[1]).not.toHaveProperty("server");
  });
});
