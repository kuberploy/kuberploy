import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { HelmApprovalsPage } from "./HelmApprovalsPage";

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
      <HelmApprovalsPage />
    </QueryClientProvider>,
  );
}

const exact = {
  features: { helmApprovals: true },
  capabilities: [
    {
      scopeType: "platform" as const,
      scopeId: "platform",
      actions: ["helm-approvals:manage"],
    },
  ],
};
const principal = {
  id: "admin",
  displayName: "Admin",
  role: "platform-admin",
  authentication: { kind: "session" as const },
};

describe("Helm approvals settings", () => {
  it("does not query the catalog without all exact gates", async () => {
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "capabilities").mockResolvedValue({
      ...exact,
      features: { helmApprovals: false },
    });
    const catalog = vi.spyOn(api, "platformHelmApprovals");
    renderPage();
    expect(
      await screen.findByText("Helm approval management unavailable"),
    ).toBeVisible();
    expect(catalog).not.toHaveBeenCalled();
  });

  it("submits only bounded contract fields and reuses the key after failure", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "capabilities").mockResolvedValue(exact);
    vi.spyOn(api, "platformHelmApprovals").mockResolvedValue({ items: [] });
    const created = vi
      .spyOn(api, "createPlatformHelmApproval")
      .mockRejectedValueOnce(new Error("temporary"))
      .mockResolvedValueOnce({
        id: "approval",
        revision: 1,
        repository: "oci://registry/charts/api",
        version: "1.0.0",
        manifestDigest: `sha256:${"a".repeat(64)}`,
        packageDigest: `sha256:${"b".repeat(64)}`,
        valuesSchemaDigest: `sha256:${"c".repeat(64)}`,
        rendererImage: "redacted",
        rendererVersion: "4.2.3",
        policyVersion: "external-helm-p0.v1",
        documentsDigest: `sha256:${"d".repeat(64)}`,
        valuesSchema: {},
        defaultValuesYaml: "{}\n",
        createdAt: "2026-08-09T00:00:00Z",
      });
    renderPage();
    await screen.findByText("Approve an immutable package");
    const input = (name: string) =>
      document.querySelector<HTMLInputElement>(`input[name="${name}"]`)!;
    await user.type(input("repository"), "oci://registry/charts/api");
    await user.type(input("version"), "1.0.0");
    await user.type(input("manifestDigest"), `sha256:${"a".repeat(64)}`);
    await user.type(input("packageDigest"), `sha256:${"b".repeat(64)}`);
    await user.type(input("valuesSchemaDigest"), `sha256:${"c".repeat(64)}`);
    const submit = screen.getByRole("button", {
      name: "Create immutable approval",
    });
    await user.click(submit);
    await screen.findByText("temporary");
    await user.click(submit);
    await waitFor(() => expect(created).toHaveBeenCalledTimes(2));
    expect(created.mock.calls[0][0]).toEqual({
      repository: "oci://registry/charts/api",
      version: "1.0.0",
      manifestDigest: `sha256:${"a".repeat(64)}`,
      packageDigest: `sha256:${"b".repeat(64)}`,
      valuesSchemaDigest: `sha256:${"c".repeat(64)}`,
    });
    expect(created.mock.calls[1][1]).toBe(created.mock.calls[0][1]);
    expect(screen.queryByLabelText(/credential|document|renderer/i)).toBeNull();
  });

  it("preserves a newer approval draft when the earlier create completes", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "capabilities").mockResolvedValue(exact);
    vi.spyOn(api, "platformHelmApprovals").mockResolvedValue({ items: [] });
    let resolveCreate!: (
      value: Awaited<ReturnType<typeof api.createPlatformHelmApproval>>,
    ) => void;
    const create = vi
      .spyOn(api, "createPlatformHelmApproval")
      .mockImplementation(
        () =>
          new Promise((resolve) => {
            resolveCreate = resolve;
          }),
      );
    renderPage();
    await screen.findByText("Approve an immutable package");
    const input = (name: string) =>
      document.querySelector<HTMLInputElement>(`input[name="${name}"]`)!;
    await user.type(input("repository"), "oci://registry/charts/api");
    await user.type(input("version"), "1.0.0");
    await user.type(input("manifestDigest"), `sha256:${"a".repeat(64)}`);
    await user.type(input("packageDigest"), `sha256:${"b".repeat(64)}`);
    await user.type(input("valuesSchemaDigest"), `sha256:${"c".repeat(64)}`);
    await user.click(
      screen.getByRole("button", { name: "Create immutable approval" }),
    );
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    await user.clear(input("version"));
    await user.type(input("version"), "2.0.0");

    resolveCreate(
      {} as Awaited<ReturnType<typeof api.createPlatformHelmApproval>>,
    );
    await waitFor(() => expect(input("version")).toHaveValue("2.0.0"));
  });
});
