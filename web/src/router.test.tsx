import {
  focusManager,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "./api/client";
import {
  postAuthenticationDestination,
  requireScopedDeploySearch,
  RootComponent,
  router,
} from "./router";

vi.mock("./components/AppShell", () => ({
  AppShell: ({ user }: { user: { authentication: { kind: string } } }) => (
    <div>Authenticated application {user.authentication.kind}</div>
  ),
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.replaceState({}, "", "/");
});

describe("root invitation boundary", () => {
  it("preserves and prefills an invitation from the initial URL", async () => {
    window.history.replaceState({}, "", "/#invite=kp_initial_mount_invite");
    vi.spyOn(api, "me").mockRejectedValue(new ApiError(401));
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<RootComponent />, { wrapper: Wrapper });

    expect(
      await screen.findByRole("heading", { name: "Join your Kuberploy team" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(/invitation token/i)).toHaveValue(
      "kp_initial_mount_invite",
    );
    expect(window.location.hash).toBe("");
  });

  it("enters invitation mode when a link changes the hash after root mount", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_existing",
      displayName: "Existing administrator",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<RootComponent />, { wrapper: Wrapper });
    expect(
      await screen.findByText("Authenticated application session"),
    ).toBeInTheDocument();

    window.location.hash = "invite=kp_same_page_invite";

    expect(
      await screen.findByRole("heading", { name: "Join your Kuberploy team" }),
    ).toBeInTheDocument();
    expect(window.location.hash).toBe("");
  });

  it("keeps the incoming invitation when routing clears the fragment first", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_existing",
      displayName: "Existing administrator",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<RootComponent />, { wrapper: Wrapper });
    expect(
      await screen.findByText("Authenticated application session"),
    ).toBeInTheDocument();

    const inviteURL = new URL(window.location.href);
    inviteURL.hash = "invite=kp_router_cleared_fragment_invite";
    window.history.replaceState(window.history.state, "", "/");
    window.dispatchEvent(
      new HashChangeEvent("hashchange", {
        newURL: inviteURL.toString(),
        oldURL: window.location.href,
      }),
    );

    expect(
      await screen.findByRole("heading", { name: "Join your Kuberploy team" }),
    ).toBeInTheDocument();
  });

  it("renders the application after a successful login", async () => {
    vi.spyOn(api, "me").mockRejectedValue(new ApiError(401));
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    vi.spyOn(api, "login").mockResolvedValue({
      id: "user_admin",
      displayName: "Administrator",
      role: "platform-admin",
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();

    render(<RootComponent />, { wrapper: Wrapper });
    await screen.findByText("Sign in to continue");
    await user.type(screen.getByLabelText(/email/i), "admin@example.com");
    await user.type(
      screen.getByLabelText(/^password/i),
      "correct horse battery staple",
    );
    await user.click(screen.getByRole("button", { name: /^sign in/i }));

    expect(
      await screen.findByText("Authenticated application session"),
    ).toBeInTheDocument();
  });

  it("keeps unauthenticated form data when the window regains focus", async () => {
    const me = vi.spyOn(api, "me").mockRejectedValue(new ApiError(401));
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();

    render(<RootComponent />, { wrapper: Wrapper });
    const email = await screen.findByLabelText(/^email/i);
    await user.type(email, "admin@example.com");

    focusManager.setFocused(false);
    focusManager.setFocused(true);

    expect(email).toHaveValue("admin@example.com");
    expect(me).toHaveBeenCalledTimes(1);
    focusManager.setFocused(undefined);
  });

  it("shows and clears an invitation even when an existing session is valid", async () => {
    window.history.replaceState(
      { existing: true },
      "",
      "/#invite=kp_existing_session_invite",
    );
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_existing",
      displayName: "Existing administrator",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<RootComponent />, { wrapper: Wrapper });

    expect(
      await screen.findByRole("heading", { name: "Join your Kuberploy team" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(/invitation token/i)).toHaveValue(
      "kp_existing_session_invite",
    );
    expect(window.location.hash).toBe("");
    expect(window.history.state).toEqual({ existing: true });
    expect(
      screen.queryByText("Authenticated application session"),
    ).not.toBeInTheDocument();

    await userEvent.setup().click(
      screen.getByRole("button", {
        name: /Use installation bootstrap|Back to sign in/,
      }),
    );
    expect(
      await screen.findByText("Authenticated application session"),
    ).toBeInTheDocument();
  });

  it("accepts an invitation and switches away from an existing session", async () => {
    window.history.replaceState({}, "", "/#invite=kp_switch_account_invite");
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_existing",
      displayName: "Existing administrator",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const acceptInvitation = vi
      .spyOn(api, "acceptInvitation")
      .mockResolvedValue({
        id: "user_invited",
        displayName: "Invited developer",
        role: "developer",
      });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();

    render(<RootComponent />, { wrapper: Wrapper });

    await user.type(
      screen.getByLabelText(/display name/i),
      "Invited developer",
    );
    await user.type(
      screen.getByLabelText(/^password/i),
      "developer password 123",
    );
    await user.click(
      screen.getByRole("button", { name: /accept invitation/i }),
    );

    expect(
      await screen.findByText("Authenticated application session"),
    ).toBeInTheDocument();
    expect(acceptInvitation).toHaveBeenCalledWith(
      {
        displayName: "Invited developer",
        password: "developer password 123",
        token: "kp_switch_account_invite",
      },
      expect.anything(),
    );
  });

  it("clears an invalid invitation fragment for an existing session", async () => {
    window.history.replaceState({}, "", "/#invite=not%20a%20token");
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_existing",
      displayName: "Existing User",
      role: "platform-admin",
      authentication: { kind: "session" },
    });

    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<RootComponent />, { wrapper: Wrapper });

    expect(
      await screen.findByText("Authenticated application session"),
    ).toBeInTheDocument();
    expect(window.location.hash).toBe("");
  });
});

describe("app-centric routes", () => {
  const projectId = "project-payments";
  const environmentId = "environment-production";
  const applicationId = "application-api";

  it("matches the canonical environment workspace with exact ancestry params", () => {
    const match = router
      .matchRoutes(`/projects/${projectId}/environments/${environmentId}`, {})
      .at(-1);

    expect(match?.routeId).toBe(
      "/projects/$projectId/environments/$environmentId",
    );
    expect(match?.params).toMatchObject({ projectId, environmentId });
  });

  it("matches canonical Add App without leaving the Environment", () => {
    const match = router
      .matchRoutes(
        `/projects/${projectId}/environments/${environmentId}/apps/new`,
        {},
      )
      .at(-1);

    expect(match?.routeId).toBe(
      "/projects/$projectId/environments/$environmentId/apps/new",
    );
    expect(match?.params).toMatchObject({ projectId, environmentId });
  });

  it("keeps the environment App route canonical without redirecting to a top-level workspace", () => {
    const path = `/projects/${projectId}/environments/${environmentId}/apps/${applicationId}`;
    const match = router.matchRoutes(path, {}).at(-1);

    expect(match?.routeId).toBe(
      "/projects/$projectId/environments/$environmentId/apps/$applicationId",
    );
    expect(match?.params).toMatchObject({
      projectId,
      environmentId,
      applicationId,
    });
    expect(
      router.routesByPath[
        "/projects/$projectId/environments/$environmentId/apps/$applicationId"
      ].options.beforeLoad,
    ).toBeUndefined();
  });

  it("keeps scoped creation, application, and deep settings routes compatible", () => {
    const routes = [
      ["/deploy", "/deploy"],
      [`/applications/${applicationId}`, "/applications/$applicationId"],
      [
        `/applications/${applicationId}/deployments/deployment-current`,
        "/applications/$applicationId/deployments/$deploymentId",
      ],
      ["/settings/releases", "/settings/releases"],
      ["/settings/argo-git", "/settings/argo-git"],
      ["/settings/middleware-profiles", "/settings/middleware-profiles"],
      ["/settings/certificate-issuers", "/settings/certificate-issuers"],
      ["/settings", "/settings"],
      ["/settings/integrations", "/settings/integrations"],
    ];

    for (const [path, routeId] of routes) {
      expect(router.matchRoutes(path, {}).at(-1)?.routeId).toBe(routeId);
    }
  });

  it("loads page modules on route demand", () => {
    const lazyPaths = [
      "/",
      "/projects",
      "/projects/$projectId",
      "/projects/$projectId/environments/$environmentId",
      "/projects/$projectId/environments/$environmentId/apps/new",
      "/projects/$projectId/environments/$environmentId/apps/$applicationId",
      "/applications/$applicationId",
      "/applications/$applicationId/deployments/$deploymentId",
      "/builds/$buildId",
      "/monitoring",
      "/audit",
      "/registry",
      "/git",
      "/setup",
      "/settings/releases",
    ] as const;

    for (const path of lazyPaths) {
      const component = router.routesByPath[path].options.component as {
        preload?: () => Promise<unknown>;
      };
      expect(component.preload, path).toEqual(expect.any(Function));
    }
  });

  it("redirects the OCI compatibility route unless Add App supplied exact scope", () => {
    expect(() => requireScopedDeploySearch({})).toThrow();
    expect(() =>
      requireScopedDeploySearch({ projectId, environmentId, applicationId }),
    ).not.toThrow();
  });

  it("preserves scoped App search on the OCI compatibility route", () => {
    const match = router
      .matchRoutes("/deploy", { projectId, environmentId, applicationId })
      .at(-1);

    expect(match?.routeId).toBe("/deploy");
    expect(match?.search).toEqual({ projectId, environmentId, applicationId });
  });

  it("preserves an exact Environment Git deep link on the Project route", () => {
    const match = router
      .matchRoutes(`/projects/${projectId}`, {
        gitEnvironmentId: environmentId,
      })
      .at(-1);

    expect(match?.routeId).toBe("/projects/$projectId");
    expect(match?.search).toEqual({ gitEnvironmentId: environmentId });
  });

  it("ignores malformed or empty Environment Git search values", () => {
    for (const gitEnvironmentId of [42, [environmentId], {}, " "]) {
      const match = router
        .matchRoutes(`/projects/${projectId}`, { gitEnvironmentId })
        .at(-1);

      expect(match?.search).toMatchObject({ gitEnvironmentId: undefined });
    }
  });
});

describe("post-authentication routing", () => {
  it("preserves valid deep links and provider callbacks", () => {
    expect(
      postAuthenticationDestination(
        "/projects/project-payments/environments/environment-production/apps/application-api",
      ),
    ).toBeNull();
    expect(postAuthenticationDestination("/github/setup/complete")).toBeNull();
  });

  it("lands stale builds and unknown routes safely", () => {
    expect(postAuthenticationDestination("/builds")).toBe("/git");
    expect(postAuthenticationDestination("/route-that-does-not-exist")).toBe(
      "/",
    );
  });
});
