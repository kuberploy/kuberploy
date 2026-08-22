import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { CertificateIssuerAdminEntry } from "../api/types";
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

const issuer: CertificateIssuerAdminEntry = {
  id: "issuer-1",
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
    state: "ready",
    updatedAt: "2026-08-09T00:00:00Z",
  },
  createdAt: "2026-08-09T00:00:00Z",
};

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <CertificateIssuersPage />
    </QueryClientProvider>,
  );
  return queryClient;
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

  it("creates a Cloudflare DNS-01 profile from zones and an existing Secret reference", async () => {
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
        ...issuer,
        name: "tenant-dns",
        revision: {
          ...issuer.revision,
          solver: "dns01-cloudflare",
          dnsZones: ["example.com", "services.example.net"],
          apiTokenSecretName: "cloudflare-dns01",
          apiTokenSecretKey: "api-token",
        },
      });
    renderPage();

    await screen.findByText("Create an issuer");
    await user.type(screen.getByLabelText(/^Issuer name/), "tenant-dns");
    await user.type(
      screen.getByLabelText(/^ACME account email/),
      "admin@example.com",
    );
    await user.type(
      screen.getByLabelText(/^ACME account Secret name/),
      "tenant-dns-account",
    );
    await user.selectOptions(
      screen.getByLabelText(/^Solver/),
      "dns01-cloudflare",
    );
    fireEvent.change(screen.getByLabelText(/^Authorized DNS zones/), {
      target: { value: "example.com\nservices.example.net" },
    });
    await user.type(
      screen.getByLabelText(/^Cloudflare API-token Secret name/),
      "cloudflare-dns01",
    );
    await user.click(screen.getByRole("button", { name: "Create issuer" }));

    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0]?.[1]).toEqual({
      environment: "production",
      email: "admin@example.com",
      accountPrivateKeySecretName: "tenant-dns-account",
      solver: {
        type: "dns01-cloudflare",
        dnsZones: ["example.com", "services.example.net"],
        apiTokenSecretName: "cloudflare-dns01",
        apiTokenSecretKey: "api-token",
      },
    });
  });

  it("blocks publishing a revision after the catalog advances", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { certificateIssuerManagement: true },
    });
    const catalog = vi
      .spyOn(api, "platformCertificateIssuers")
      .mockResolvedValueOnce({ items: [issuer] })
      .mockResolvedValueOnce({
        items: [
          {
            ...issuer,
            currentRevision: 2,
            revision: { ...issuer.revision, number: 2 },
          },
        ],
      });
    const queryClient = renderPage();

    await user.click(await screen.findByRole("button", { name: "Revise" }));
    await queryClient.invalidateQueries({
      queryKey: ["platform-certificate-issuers"],
    });

    expect(
      await screen.findByText(
        "This issuer changed, was deactivated, or is no longer available. Reload the catalog before publishing a revision.",
      ),
    ).toBeVisible();
    expect(
      screen.getByRole("button", { name: "Publish revision" }),
    ).toBeDisabled();
    expect(catalog).toHaveBeenCalledTimes(2);
  });

  it("keeps a reopened issuer editor open after its older revision completes", async () => {
    const user = userEvent.setup();
    let resolveRevision!: (value: CertificateIssuerAdminEntry) => void;
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { certificateIssuerManagement: true },
    });
    vi.spyOn(api, "platformCertificateIssuers").mockResolvedValue({
      items: [issuer],
    });
    vi.spyOn(api, "revisePlatformCertificateIssuer").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveRevision = resolve;
        }),
    );
    renderPage();

    await user.click(await screen.findByRole("button", { name: "Revise" }));
    await user.click(screen.getByRole("button", { name: "Publish revision" }));
    await waitFor(() =>
      expect(api.revisePlatformCertificateIssuer).toHaveBeenCalledOnce(),
    );
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Revise" }));

    resolveRevision(issuer);

    await waitFor(() =>
      expect(api.platformCertificateIssuers).toHaveBeenCalledTimes(2),
    );
    expect(
      screen.getByRole("button", { name: "Publish revision" }),
    ).toBeVisible();
  });
});
