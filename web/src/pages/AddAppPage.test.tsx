import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { AddAppPage } from "./AddAppPage";

const navigate = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params,
    className,
  }: PropsWithChildren<{
    to: string;
    params?: Record<string, string>;
    className?: string;
  }>) => (
    <a
      className={className}
      href={Object.entries(params ?? {}).reduce(
        (path, [key, value]) => path.replace(`$${key}`, value),
        to,
      )}
    >
      {children}
    </a>
  ),
  useNavigate: () => navigate,
  useParams: () => ({
    projectId: "project-payments",
    environmentId: "environment-production",
  }),
}));

function wrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

beforeEach(() => {
  navigate.mockReset();
  navigate.mockResolvedValue(undefined);
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [{ id: "project-payments", name: "Payments" }],
  });
  vi.spyOn(api, "environment").mockResolvedValue({
    id: "environment-production",
    projectId: "project-payments",
    name: "Production",
    namespace: "payments-production",
    protectionPolicy: "protected",
  });
  vi.spyOn(api, "capabilities").mockResolvedValue({
    features: {
      builds: true,
      builder: true,
      githubAppSetup: true,
      gitSSH: true,
      gitSSHBuilds: true,
      helmDeployments: true,
    },
    capabilities: [
      {
        role: "project-admin",
        scopeType: "project",
        scopeId: "project-payments",
        actions: [
          "applications:create",
          "deployments:create",
          "build-definitions:write",
          "helm-releases:deploy",
          "helm-releases:disable",
        ],
      },
    ],
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("Add App source flow", () => {
  it("offers exactly the four supported sources inside an Environment", async () => {
    render(<AddAppPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Add App" }),
    ).toBeInTheDocument();
    expect(
      screen.getAllByRole("radio").map((item) => item.textContent),
    ).toEqual([
      expect.stringContaining("OCI image"),
      expect.stringContaining("GitHub App"),
      expect.stringContaining("Git SSH"),
      expect.stringContaining("Helm chart"),
    ]);
    expect(
      screen.getByRole("radio", { name: /Git SSH/ }),
    ).toHaveAccessibleName(/No automatic provider webhooks/);
    expect(screen.queryByRole("textbox", { name: "App name" })).toBeNull();
  });

  it("creates a durable App identity and continues OCI setup in the same scope", async () => {
    const create = vi.spyOn(api, "createApplication").mockResolvedValue({
      id: "application-api",
      projectId: "project-payments",
      name: "Payments API",
    });
    const user = userEvent.setup();
    render(<AddAppPage />, { wrapper: wrapper() });

    await user.click(await screen.findByRole("radio", { name: /OCI image/ }));
    await user.type(
      screen.getByRole("textbox", { name: "App name" }),
      "Payments API",
    );
    await user.click(
      screen.getByRole("button", { name: "Continue with OCI image" }),
    );

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create.mock.calls[0]?.[0]).toEqual({
      projectId: "project-payments",
      environmentId: "environment-production",
      name: "Payments API",
      sourceKind: "oci",
    });
    await waitFor(() =>
      expect(navigate).toHaveBeenCalledWith({
        to: "/deploy",
        search: {
          projectId: "project-payments",
          environmentId: "environment-production",
          applicationId: "application-api",
        },
      }),
    );
  });

  it("opens GitHub source setup for the created App", async () => {
    const create = vi.spyOn(api, "createApplication").mockResolvedValue({
      id: "application-web",
      projectId: "project-payments",
      name: "Web",
    });
    const user = userEvent.setup();
    render(<AddAppPage />, { wrapper: wrapper() });

    await user.click(await screen.findByRole("radio", { name: /GitHub App/ }));
    await user.type(screen.getByRole("textbox", { name: "App name" }), "Web");
    await user.click(
      screen.getByRole("button", { name: "Continue with GitHub App" }),
    );

    await waitFor(() =>
      expect(create).toHaveBeenCalledWith(
        {
          projectId: "project-payments",
          environmentId: "environment-production",
          name: "Web",
          sourceKind: "github",
        },
        expect.any(String),
      ),
    );

    await waitFor(() =>
      expect(navigate).toHaveBeenCalledWith({
        to: "/projects/$projectId/environments/$environmentId/apps/$applicationId",
        params: {
          projectId: "project-payments",
          environmentId: "environment-production",
          applicationId: "application-web",
        },
        search: {
          tab: "source",
          source: "github",
          environmentId: "environment-production",
        },
      }),
    );
  });

  it("rejects environment-only App identity permission before mutation", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      capabilities: [
        {
          role: "developer",
          scopeType: "environment",
          scopeId: "environment-production",
          actions: ["applications:create", "deployments:create"],
        },
      ],
    });
    const create = vi.spyOn(api, "createApplication");

    render(<AddAppPage />, { wrapper: wrapper() });

    expect(await screen.findByText("Add App is unavailable")).toBeVisible();
    expect(screen.queryByRole("radio", { name: /OCI image/ })).toBeNull();
    expect(create).not.toHaveBeenCalled();
  });

  it("keeps build sources unavailable without build-definition authority", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        builds: true,
        builder: true,
        gitSSH: true,
        gitSSHBuilds: true,
        helmDeployments: true,
      },
      capabilities: [
        {
          role: "developer",
          scopeType: "project",
          scopeId: "project-payments",
          actions: [
            "applications:create",
            "deployments:create",
            "helm-releases:deploy",
            "helm-releases:disable",
          ],
        },
      ],
    });

    render(<AddAppPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("radio", { name: /OCI image/ }),
    ).toBeEnabled();
    expect(screen.getByRole("radio", { name: /GitHub App/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Git SSH/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Helm chart/ })).toBeEnabled();
  });

  it("allows stopped draft Git sources while build execution lacks capacity", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        builds: false,
        builder: false,
        githubAppSetup: true,
        gitSSH: true,
        gitSSHBuilds: false,
        helmDeployments: false,
      },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["applications:create", "build-definitions:write"],
        },
      ],
    });

    render(<AddAppPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("radio", { name: /GitHub App/ }),
    ).toBeEnabled();
    expect(screen.getByRole("radio", { name: /Git SSH/ })).toBeEnabled();
    expect(screen.getByRole("radio", { name: /OCI image/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Helm chart/ })).toBeDisabled();
  });

  it("allows Helm App creation without OCI deployment authority", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        builds: false,
        builder: false,
        gitSSH: false,
        gitSSHBuilds: false,
        helmDeployments: true,
      },
      capabilities: [
        {
          role: "developer",
          scopeType: "project",
          scopeId: "project-payments",
          actions: [
            "applications:create",
            "helm-releases:deploy",
            "helm-releases:disable",
          ],
        },
      ],
    });

    render(<AddAppPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("radio", { name: /OCI image/ }),
    ).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Helm chart/ })).toBeEnabled();
    expect(screen.queryByText("Add App is unavailable")).toBeNull();
  });

  it("does not create a draft App from an unavailable source", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        builds: false,
        builder: false,
        gitSSH: false,
        gitSSHBuilds: false,
        helmDeployments: false,
      },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["applications:create", "deployments:create"],
        },
      ],
    });
    const create = vi.spyOn(api, "createApplication");
    const user = userEvent.setup();

    render(<AddAppPage />, { wrapper: wrapper() });

    const github = await screen.findByRole("radio", { name: /GitHub App/ });
    expect(github).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Git SSH/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Helm chart/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /OCI image/ })).toBeEnabled();
    await user.click(github);
    expect(screen.queryByRole("textbox", { name: "App name" })).toBeNull();
    expect(create).not.toHaveBeenCalled();
  });
});
