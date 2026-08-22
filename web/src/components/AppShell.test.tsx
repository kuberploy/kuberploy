import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Capabilities } from "../api/types";
import { AppShell } from "./AppShell";

let currentPathname = "/";

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
  }: PropsWithChildren<{
    to: string;
  }>) => <a href={to}>{children}</a>,
  Outlet: () => <div>Page content</div>,
  useRouterState: ({
    select,
  }: {
    select: (state: { location: { pathname: string } }) => string;
  }) => select({ location: { pathname: currentPathname } }),
}));

beforeEach(() => {
  currentPathname = "/";
  const values = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    clear: () => values.clear(),
    getItem: (key: string) => values.get(key) ?? null,
    removeItem: (key: string) => values.delete(key),
    setItem: (key: string, value: string) => values.set(key, value),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  localStorage.clear();
  vi.unstubAllGlobals();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-theme-preference");
});

describe("appearance", () => {
  it("uses a deployment-neutral cluster label", () => {
    renderShell({});

    expect(screen.getByText("Kubernetes cluster")).toBeInTheDocument();
    expect(screen.queryByText("Local cluster")).not.toBeInTheDocument();
  });

  it("offers auto, light, and dark modes and persists an override", () => {
    renderShell({});

    const automatic = screen.getByRole("radio", {
      name: "Use automatic theme",
    });
    const dark = screen.getByRole("radio", { name: "Use dark theme" });
    expect(automatic).toHaveAttribute("aria-checked", "true");
    fireEvent.click(dark);

    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(localStorage.getItem("kuberploy-theme")).toBe("dark");
    expect(dark).toHaveAttribute("aria-checked", "true");
  });

  it("supports arrow focus and Space activation for theme radios", async () => {
    const user = userEvent.setup();
    renderShell({});

    const automatic = screen.getByRole("radio", {
      name: "Use automatic theme",
    });
    const light = screen.getByRole("radio", { name: "Use light theme" });

    automatic.focus();
    await user.keyboard("{ArrowRight}");
    expect(light).toHaveFocus();
    expect(light).toHaveAttribute("aria-checked", "false");

    await user.keyboard(" ");
    expect(light).toHaveAttribute("aria-checked", "true");
    expect(document.documentElement.dataset.theme).toBe("light");
  });
});

describe("session logout", () => {
  it("clears cached principal and tenant data after revocation", async () => {
    vi.spyOn(api, "logout").mockResolvedValue(undefined);
    const queryClient = renderShell({});
    queryClient.setQueryData(["me"], { id: "user-1" });
    queryClient.setQueryData(["projects"], [{ id: "project-1" }]);

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));

    await waitFor(() => expect(api.logout).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(queryClient.getQueryData(["me"])).toBeUndefined(),
    );
    expect(queryClient.getQueryData(["projects"])).toBeUndefined();
  });
});

function renderShell(
  capabilities: Capabilities,
  role: "viewer" | "platform-admin" = "viewer",
  authentication: "session" | "service-account" = "session",
  pathname = "/",
) {
  currentPathname = pathname;
  vi.spyOn(api, "capabilities").mockResolvedValue(capabilities);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <AppShell
        user={{
          id: "user-1",
          displayName: "Viewer",
          role,
          authentication:
            authentication === "session"
              ? { kind: "session" }
              : {
                  kind: "service-account",
                  serviceAccountId: "service-account-1",
                  tokenId: "token-1",
                  scopes: [],
                  expiresAt: "2026-08-10T00:00:00Z",
                },
        }}
      />
    </QueryClientProvider>,
  );
  return queryClient;
}

describe("platform Git authority navigation", () => {
  it("is visible only to platform administrators", async () => {
    renderShell({}, "viewer");
    expect(
      screen.queryByRole("link", { name: "Argo Git authority" }),
    ).toBeNull();

    cleanup();
    renderShell({}, "platform-admin");
    expect(
      await screen.findByRole("link", { name: "Argo Git authority" }),
    ).toHaveAttribute("href", "/settings/argo-git");
  });
});

describe("platform release navigation", () => {
  it("requires an exact platform release-read capability and human session", async () => {
    renderShell(
      {
        actions: ["platform-releases:read"],
        capabilities: [],
      },
      "platform-admin",
    );
    await waitFor(() => expect(api.capabilities).toHaveBeenCalled());
    expect(
      screen.queryByRole("link", { name: "Platform releases" }),
    ).toBeNull();

    cleanup();
    const exact = {
      capabilities: [
        {
          scopeType: "platform" as const,
          scopeId: "platform",
          actions: ["platform-releases:read"],
        },
      ],
    };
    renderShell(exact, "platform-admin", "session");
    expect(
      await screen.findByRole("link", { name: "Platform releases" }),
    ).toHaveAttribute("href", "/settings/releases");

    cleanup();
    renderShell(exact, "platform-admin", "service-account");
    await waitFor(() => expect(api.capabilities).toHaveBeenCalled());
    expect(
      screen.queryByRole("link", { name: "Platform releases" }),
    ).toBeNull();
  });
});

describe("application scheduling navigation", () => {
  it("keeps scheduling in each App configuration instead of platform navigation", async () => {
    renderShell({ features: {} }, "platform-admin", "session");

    await waitFor(() => expect(api.capabilities).toHaveBeenCalled());
    expect(
      screen.queryByRole("link", { name: "Scheduling profiles" }),
    ).toBeNull();
  });
});

describe("Helm approval navigation", () => {
  const exactGrant: Capabilities = {
    features: { helmApprovals: true },
    capabilities: [
      {
        scopeType: "platform",
        scopeId: "platform",
        actions: ["helm-approvals:manage"],
      },
    ],
  };

  it("requires the exact feature, platform grant, and human session", async () => {
    renderShell(exactGrant, "platform-admin", "session");
    expect(
      await screen.findByRole("link", { name: "Helm approvals" }),
    ).toHaveAttribute("href", "/settings/helm-approvals");

    cleanup();
    renderShell(exactGrant, "platform-admin", "service-account");
    await waitFor(() => expect(api.capabilities).toHaveBeenCalled());
    expect(screen.queryByRole("link", { name: "Helm approvals" })).toBeNull();

    cleanup();
    renderShell(
      { features: { helmApprovals: true }, actions: ["helm-approvals:manage"] },
      "platform-admin",
      "session",
    );
    await waitFor(() => expect(api.capabilities).toHaveBeenCalled());
    expect(screen.queryByRole("link", { name: "Helm approvals" })).toBeNull();
  });
});

describe("monitoring navigation", () => {
  it("stays hidden when only coarse actions and feature flags advertise metrics", async () => {
    const response: Capabilities = {
      actions: ["metrics:read"],
      features: { metrics: true, monitoring: true },
      capabilities: [],
    };
    const queryClient = renderShell(response);

    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toEqual(response),
    );
    expect(
      screen.queryByRole("link", { name: "Monitoring" }),
    ).not.toBeInTheDocument();
  });

  it("appears for an effective grant with metrics access", async () => {
    renderShell({
      capabilities: [
        {
          role: "viewer",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["metrics:read"],
        },
      ],
    });

    expect(
      await screen.findByRole("link", { name: "Monitoring" }),
    ).toHaveAttribute("href", "/monitoring");
  });
});

describe("registry navigation", () => {
  it("ignores top-level actions and non-platform grants", async () => {
    const response: Capabilities = {
      actions: ["registry-targets:read"],
      features: { registry: true },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["registry-targets:read"],
        },
      ],
    };
    const queryClient = renderShell(response);

    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toEqual(response),
    );
    expect(
      screen.queryByRole("link", { name: "Registries" }),
    ).not.toBeInTheDocument();
  });

  it("requires both the feature and exact platform target-read grant", async () => {
    renderShell({
      features: { registry: true },
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["registry-targets:read"],
        },
      ],
    });

    expect(
      await screen.findByRole("link", { name: "Registries" }),
    ).toHaveAttribute("href", "/registry");
  });
});

describe("External DNS navigation", () => {
  it("requires configuration plus an exact platform-admin read capability", async () => {
    const response: Capabilities = {
      actions: ["external-dns-integrations:read"],
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        {
          role: "project-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["external-dns-integrations:read"],
        },
      ],
    };
    const queryClient = renderShell(response);
    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toEqual(response),
    );
    expect(screen.queryByRole("link", { name: "External DNS" })).toBeNull();

    cleanup();
    renderShell({
      features: { externalDNSConfiguration: true, externalDNS: false },
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["external-dns-integrations:read"],
        },
      ],
    });
    expect(
      await screen.findByRole("link", { name: "External DNS" }),
    ).toHaveAttribute("href", "/external-dns");
  });
});

describe("Git provider navigation", () => {
  it("ignores coarse actions and environment-only build grants", async () => {
    const response: Capabilities = {
      actions: ["build-definitions:read", "builds:read"],
      features: { githubAppSetup: false, builds: true, builder: true },
      capabilities: [
        {
          scopeType: "environment",
          scopeId: "environment-payments",
          actions: ["build-definitions:read", "builds:read"],
        },
      ],
    };
    const queryClient = renderShell(response);
    await waitFor(() =>
      expect(queryClient.getQueryData(["capabilities"])).toEqual(response),
    );
    expect(
      screen.queryByRole("link", { name: "Git Providers" }),
    ).not.toBeInTheDocument();
  });

  it("appears for exact build-read access or separately enabled setup", async () => {
    renderShell({
      features: { githubAppSetup: false, builds: true, builder: true },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-payments",
          actions: ["build-definitions:read", "builds:read"],
        },
      ],
    });
    expect(
      await screen.findByRole("link", { name: "Git Providers" }),
    ).toHaveAttribute("href", "/builds");

    cleanup();
    renderShell({
      features: { githubAppSetup: true, builds: false, builder: false },
      capabilities: [],
    });
    expect(
      await screen.findByRole("link", { name: "Git Providers" }),
    ).toHaveAttribute("href", "/builds");
  });
});

describe("navigation hierarchy", () => {
  it("uses the exact product-level labels and keeps operational pages secondary", async () => {
    renderShell({
      features: {
        registry: true,
        githubAppSetup: true,
        externalDNSConfiguration: true,
      },
      capabilities: [
        {
          role: "platform-admin",
          scopeType: "platform",
          scopeId: "platform",
          actions: ["registry-targets:read", "external-dns-integrations:read"],
        },
      ],
    });

    await screen.findByRole("link", { name: "Registries" });
    await screen.findByRole("link", { name: "Git Providers" });
    const primary = within(
      screen.getByRole("group", { name: "Primary navigation" }),
    );
    expect(
      primary.getAllByRole("link").map((link) => link.textContent),
    ).toEqual(["Projects", "Registries", "Git Providers", "Settings"]);

    const settings = within(
      screen.getByRole("group", { name: "Settings navigation" }),
    );
    expect(settings.getByRole("link", { name: "Dashboard" })).toHaveAttribute(
      "href",
      "/",
    );
    expect(settings.getByRole("link", { name: "Teams" })).toHaveAttribute(
      "href",
      "/teams",
    );
    expect(
      settings.getByRole("link", { name: "Audit timeline" }),
    ).toHaveAttribute("href", "/audit");
    expect(
      settings.getByRole("link", { name: "External DNS" }),
    ).toHaveAttribute("href", "/external-dns");
    expect(screen.queryByRole("link", { name: "Deploy" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Source builds" })).toBeNull();
  });

  it("uses App language for creation and application routes", () => {
    renderShell({}, "viewer", "session", "/deploy");
    expect(screen.getByText("Add App")).toBeInTheDocument();

    cleanup();
    renderShell(
      {},
      "viewer",
      "session",
      "/applications/app-1/deployments/deployment-1",
    );
    expect(screen.getByText("App")).toBeInTheDocument();
    expect(screen.queryByText("Deployment")).toBeNull();
    expect(screen.queryByText("Service")).toBeNull();
  });
});
