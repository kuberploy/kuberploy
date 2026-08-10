import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  Capability,
  CertificateBindingDetail,
  Environment,
  Project,
} from "../api/types";
import { CertificateBindingsPanel } from "./CertificateBindingsPanel";

function memoryStorage(): Storage {
  const values = new Map<string, string>();
  return {
    get length() {
      return values.size;
    },
    clear: () => values.clear(),
    getItem: (key) => values.get(key) ?? null,
    key: (index) => Array.from(values.keys())[index] ?? null,
    removeItem: (key) => values.delete(key),
    setItem: (key, value) => values.set(key, value),
  };
}

beforeEach(() => {
  vi.stubGlobal("localStorage", memoryStorage());
  vi.stubGlobal("sessionStorage", memoryStorage());
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const application: Application = {
  id: "application-payments",
  projectId: "project-payments",
  name: "Payments API",
};
const project: Project = {
  id: "project-payments",
  name: "Payments",
  teamId: "team-commerce",
};
const production: Environment = {
  id: "environment-production",
  projectId: project.id,
  name: "Production",
  namespace: "payments-production",
};
const detail: CertificateBindingDetail = {
  id: "certificate-public-edge",
  applicationId: application.id,
  environmentId: production.id,
  name: "public-edge",
  state: "ready",
  activeVersion: 2,
  createdBy: "user-admin",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
  versions: [
    {
      number: 2,
      leafFingerprint: `sha256:${"a".repeat(64)}`,
      publicKeyFingerprint: `sha256:${"b".repeat(64)}`,
      dnsNames: ["api.example.test"],
      ipAddresses: [],
      notBefore: "2026-08-09T00:00:00Z",
      notAfter: "2026-11-09T00:00:00Z",
      createdBy: "user-admin",
      createdAt: "2026-08-09T00:00:00Z",
    },
  ],
};

function capability(action: string): Capability {
  return {
    role: "project-admin",
    scopeType: "environment",
    scopeId: production.id,
    actions: [action],
  };
}

function renderPanel({
  featureEnabled = true,
  humanSession = true,
  capabilities = [
    capability("certificate-bindings:read"),
    capability("certificate-bindings:create"),
    capability("certificate-bindings:rotate"),
    capability("certificate-bindings:delete"),
  ],
}: {
  featureEnabled?: boolean;
  humanSession?: boolean;
  capabilities?: Capability[];
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const Wrapper = ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  render(
    <CertificateBindingsPanel
      application={application}
      environments={[production]}
      project={project}
      capabilities={capabilities}
      featureEnabled={featureEnabled}
      humanSession={humanSession}
    />,
    { wrapper: Wrapper },
  );
  return queryClient;
}

describe("certificate management panel", () => {
  it("stays hidden when exact production readiness is not advertised", async () => {
    const list = vi.spyOn(api, "certificateBindings");
    renderPanel({ featureEnabled: false });
    expect(
      screen.queryByText("Custom TLS certificates"),
    ).not.toBeInTheDocument();
    await Promise.resolve();
    expect(list).not.toHaveBeenCalled();
  });

  it("requires an interactive session even when scoped actions are present", () => {
    const list = vi.spyOn(api, "certificateBindings");
    renderPanel({ humanSession: false });
    expect(
      screen.getByText("Interactive session required"),
    ).toBeInTheDocument();
    expect(list).not.toHaveBeenCalled();
  });

  it("clears and uncaches uncontrolled PEM immediately after success", async () => {
    const user = userEvent.setup();
    const certificatePem =
      "-----BEGIN CERTIFICATE-----\ncertificate-request-only\n-----END CERTIFICATE-----";
    const privateKeyPem =
      "-----BEGIN PRIVATE KEY-----\nprivate-request-only\n-----END PRIVATE KEY-----";
    vi.spyOn(api, "certificateBindings").mockResolvedValue({ items: [] });
    let receivedExactPayload = false;
    const create = vi
      .spyOn(api, "createCertificateBinding")
      .mockImplementation(async (_applicationID, input) => {
        receivedExactPayload =
          input.environmentId === production.id &&
          input.name === detail.name &&
          input.certificatePem === certificatePem &&
          input.privateKeyPem === privateKeyPem;
        return detail;
      });
    const queryClient = renderPanel();

    await user.click(
      await screen.findByRole("button", { name: "New certificate" }),
    );
    await user.type(
      screen.getByRole("textbox", { name: "Certificate binding name" }),
      detail.name,
    );
    await user.type(
      screen.getByRole("textbox", {
        name: "Create certificate certificate chain PEM",
      }),
      certificatePem,
    );
    await user.type(
      screen.getByRole("textbox", {
        name: "Create certificate private key PEM",
      }),
      privateKeyPem,
    );
    await user.click(
      screen.getByRole("button", { name: "Validate and create" }),
    );

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(receivedExactPayload).toBe(true);
    expect(screen.queryByDisplayValue(certificatePem)).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue(privateKeyPem)).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent("private-request-only");
    expect(JSON.stringify(create.mock.calls)).not.toContain("request-only");
    expect(JSON.stringify(queryClient.getQueryCache().getAll())).not.toContain(
      "request-only",
    );
    expect(queryClient.getMutationCache().getAll()).toEqual([]);
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(window.location.href).not.toContain("request-only");
  });

  it("does not retain a hostile echoed PEM error and reuses only the stable key", async () => {
    const user = userEvent.setup();
    const privateKeyPem = "private-error-material";
    const certificatePem = "certificate-error-material";
    vi.spyOn(api, "certificateBindings").mockResolvedValue({ items: [] });
    const keys: string[] = [];
    const create = vi
      .spyOn(api, "createCertificateBinding")
      .mockImplementation(async (_applicationID, _input, key) => {
        keys.push(key);
        throw new Error(`hostile transport echoed ${privateKeyPem}`);
      });
    const queryClient = renderPanel();
    await user.click(
      await screen.findByRole("button", { name: "New certificate" }),
    );
    await user.type(
      screen.getByRole("textbox", { name: "Certificate binding name" }),
      detail.name,
    );

    for (let attempt = 0; attempt < 2; attempt += 1) {
      await user.type(
        screen.getByRole("textbox", {
          name: "Create certificate certificate chain PEM",
        }),
        certificatePem,
      );
      await user.type(
        screen.getByRole("textbox", {
          name: "Create certificate private key PEM",
        }),
        privateKeyPem,
      );
      await user.click(
        screen.getByRole("button", { name: "Validate and create" }),
      );
      await waitFor(() => expect(create).toHaveBeenCalledTimes(attempt + 1));
    }

    expect(keys[0]).toBeTruthy();
    expect(keys[1]).toBe(keys[0]);
    expect(document.body).not.toHaveTextContent(privateKeyPem);
    expect(JSON.stringify(create.mock.calls)).not.toContain(privateKeyPem);
    expect(JSON.stringify(queryClient.getQueryCache().getAll())).not.toContain(
      privateKeyPem,
    );
    expect(
      screen.getByText(/write-only certificate request failed/i),
    ).toBeInTheDocument();
  });

  it("uses observed active version for rotation and exact delete confirmation", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "certificateBindings").mockResolvedValue({ items: [detail] });
    vi.spyOn(api, "certificateBinding").mockResolvedValue(detail);
    let expectedVersion = 0;
    const rotate = vi
      .spyOn(api, "rotateCertificateBinding")
      .mockImplementation(async (_bindingID, input) => {
        expectedVersion = input.expectedActiveVersion;
        return detail;
      });
    const remove = vi
      .spyOn(api, "deleteCertificateBinding")
      .mockResolvedValue(undefined);
    renderPanel();

    await user.click(
      await screen.findByRole("button", { name: /public-edge/i }),
    );
    await screen.findByText("Immutable public attestations");
    await user.type(
      screen.getByRole("textbox", {
        name: "Rotate certificate certificate chain PEM",
      }),
      "new-certificate",
    );
    await user.type(
      screen.getByRole("textbox", {
        name: "Rotate certificate private key PEM",
      }),
      "new-private-key",
    );
    await user.click(
      screen.getByRole("button", { name: "Validate and rotate" }),
    );
    await waitFor(() => expect(rotate).toHaveBeenCalledOnce());
    expect(expectedVersion).toBe(2);
    expect(JSON.stringify(rotate.mock.calls)).not.toContain("new-private-key");

    const confirmation = screen.getByRole("textbox", {
      name: "Exact certificate binding name confirmation",
    });
    await user.type(confirmation, "wrong-name");
    await user.click(
      screen.getByRole("button", { name: "Delete certificate" }),
    );
    expect(remove).not.toHaveBeenCalled();
    await user.clear(confirmation);
    await user.type(confirmation, detail.name);
    await user.click(
      screen.getByRole("button", { name: "Delete certificate" }),
    );
    await waitFor(() => expect(remove).toHaveBeenCalledOnce());
    expect(remove.mock.calls[0]?.[0]).toBe(detail.id);
    expect(remove.mock.calls[0]?.[1]).toMatch(/^[A-Za-z0-9._:-]{16,128}$/);
  });
});
