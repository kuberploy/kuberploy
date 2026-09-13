import {
  focusManager,
  onlineManager,
  QueryClient,
  QueryClientProvider,
  useQuery,
} from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "./api/client";
import type { Principal, Project } from "./api/types";
import { RootComponent } from "./router";

vi.mock("@tanstack/react-router", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-router")>()),
  Link: ({ children, to }: PropsWithChildren<{ to: string }>) => (
    <a href={to}>{children}</a>
  ),
  Outlet: () => <TenantPage />,
  useRouterState: ({
    select,
  }: {
    select: (state: { location: { pathname: string } }) => string;
  }) => select({ location: { pathname: "/teams" } }),
}));

const principal: Principal = {
  id: "invited-user",
  displayName: "Invited developer",
  role: "developer",
  authentication: { kind: "session" },
};

function TenantPage() {
  useQuery({ queryKey: ["me"], queryFn: api.me });
  useQuery({ queryKey: ["projects"], queryFn: api.projects });
  return <div>Private tenant page</div>;
}

function mountRoot(client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <RootComponent />
    </QueryClientProvider>,
  );
}

function sessionClient() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  client.setQueryData(["me"], principal);
  client.setQueryData(["projects"], { items: [{ id: "private-project" }] });
  return client;
}

beforeEach(() => {
  window.history.replaceState({}, "", "/teams");
  vi.spyOn(api, "session").mockResolvedValue(null);
  vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
  vi.spyOn(api, "capabilities").mockResolvedValue({});
  vi.spyOn(api, "projects").mockResolvedValue({ items: [] });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  focusManager.setFocused(undefined);
  onlineManager.setOnline(true);
  window.history.replaceState({}, "", "/");
});

describe("confirmed browser sign-out", () => {
  it("shows sign-in without a revoked-session request, including stale focus and remount", async () => {
    const me = vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "logout").mockResolvedValue(undefined);
    vi.spyOn(api, "login").mockResolvedValue({
      id: "another-user",
      displayName: "Another developer",
      role: "developer",
    });
    const client = sessionClient();
    const user = userEvent.setup();
    const mounted = mountRoot(client);
    await screen.findByText("Private tenant page");
    await waitFor(() => expect(client.isFetching()).toBe(0));
    const previousReads = me.mock.calls.length;
    const previousProjectReads = vi.mocked(api.projects).mock.calls.length;
    me.mockRejectedValue(new ApiError(401));

    await user.click(screen.getByRole("button", { name: "Sign out" }));

    await screen.findByText("Sign in to continue");
    expect(me).toHaveBeenCalledTimes(previousReads);
    expect(client.getQueryData(["me"])).toBeNull();
    expect(client.getQueryData(["projects"])).toBeUndefined();
    expect(client.getQueryData(["capabilities"])).toBeUndefined();
    expect(api.projects).toHaveBeenCalledTimes(previousProjectReads);

    act(() => {
      client.setQueryData(["me"], null, { updatedAt: 1 });
      focusManager.setFocused(false);
      focusManager.setFocused(true);
      onlineManager.setOnline(false);
      onlineManager.setOnline(true);
    });
    mounted.unmount();
    mountRoot(client);
    await screen.findByText("Sign in to continue");
    expect(me).toHaveBeenCalledTimes(previousReads);

    await user.click(screen.getByRole("button", { name: "Retry session" }));
    await waitFor(() => expect(api.session).toHaveBeenCalledTimes(1));
    expect(me).toHaveBeenCalledTimes(previousReads);
    await waitFor(() => expect(client.isFetching()).toBe(0));
    await screen.findByText("Sign in to continue");
    me.mockResolvedValue({ ...principal, id: "another-user" });
    await user.type(screen.getByLabelText(/^email/i), "another@example.test");
    await user.type(screen.getByLabelText(/^password/i), "a-test-password");
    expect(screen.getByLabelText(/^email/i)).toHaveValue(
      "another@example.test",
    );
    await user.click(screen.getByRole("button", { name: /^sign in/i }));
    await waitFor(() => expect(api.login).toHaveBeenCalledTimes(1));
    await screen.findByText("Private tenant page");
    expect(client.getQueryData<Principal>(["me"])?.id).toBe("another-user");
  });

  it("cancels active principal and tenant reads before clearing their cached results", async () => {
    let finishMe!: (value: Principal) => void;
    let finishProjects!: (value: { items: Project[] }) => void;
    const me = vi.spyOn(api, "me").mockImplementation(
      () =>
        new Promise((resolve) => {
          finishMe = resolve;
        }),
    );
    vi.mocked(api.projects).mockImplementation(
      () =>
        new Promise((resolve) => {
          finishProjects = resolve;
        }),
    );
    vi.spyOn(api, "logout").mockResolvedValue(undefined);
    const client = sessionClient();
    const user = userEvent.setup();
    mountRoot(client);
    await screen.findByText("Private tenant page");
    await waitFor(() => expect(me).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(api.projects).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    await screen.findByText("Sign in to continue");
    await act(async () => {
      finishMe(principal);
      finishProjects({ items: [] });
    });

    expect(client.getQueryData(["me"])).toBeNull();
    expect(client.getQueryData(["projects"])).toBeUndefined();
    expect(screen.queryByText("Private tenant page")).not.toBeInTheDocument();
    expect(me).toHaveBeenCalledTimes(1);
    expect(api.projects).toHaveBeenCalledTimes(1);
  });

  it("keeps the authenticated session and tenant cache when revocation fails", async () => {
    vi.spyOn(api, "me").mockResolvedValue(principal);
    vi.spyOn(api, "logout").mockRejectedValue(new ApiError(503));
    const client = sessionClient();
    const user = userEvent.setup();
    mountRoot(client);
    await screen.findByText("Private tenant page");
    await waitFor(() => expect(client.isFetching()).toBe(0));
    const projects = client.getQueryData(["projects"]);

    await user.click(screen.getByRole("button", { name: "Sign out" }));

    await waitFor(() => expect(api.logout).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Sign out" })).toBeEnabled(),
    );
    expect(client.getQueryData(["me"])).toEqual(principal);
    expect(client.getQueryData(["projects"])).toEqual(projects);
    expect(screen.getByText("Private tenant page")).toBeInTheDocument();
    expect(screen.queryByText("Sign in to continue")).not.toBeInTheDocument();
  });
});
