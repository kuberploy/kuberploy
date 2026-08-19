import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { AuthScreen } from "./AuthScreen";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.replaceState({}, "", "/");
});

describe("invitation acceptance", () => {
  it("prefills a token supplied on the initial auth render", async () => {
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<AuthScreen invitationToken="kp_initial_invitation_token" />, {
      wrapper: Wrapper,
    });

    expect(
      await screen.findByRole("heading", { name: "Join your Kuberploy team" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(/invitation token/i)).toHaveValue(
      "kp_initial_invitation_token",
    );
  });

  it("hydrates a token when the router supplies an invitation after auth mounts", async () => {
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    const { rerender } = render(<AuthScreen />, { wrapper: Wrapper });
    await screen.findByText("Sign in to continue");

    rerender(<AuthScreen invitationToken="kp_late_invitation_token" />);

    expect(
      await screen.findByRole("heading", { name: "Join your Kuberploy team" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(/invitation token/i)).toHaveValue(
      "kp_late_invitation_token",
    );
  });

  it("opens invitation mode from a fragment, prefills the token, and removes it from browser history", async () => {
    window.history.replaceState(
      { preserved: true },
      "",
      "/teams?from=admin#invite=kp_invite_link_secret",
    );
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: true });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    queryClient.setQueryData(["projects"], {
      items: [{ id: "old-private-project", name: "Old private project" }],
    });
    queryClient.setQueryData(["capabilities"], {
      capabilities: [{ scopeType: "project", scopeId: "old-private-project" }],
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<AuthScreen />, { wrapper: Wrapper });

    expect(
      screen.getByRole("heading", { name: "Join your Kuberploy team" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(/invitation token/i)).toHaveValue(
      "kp_invite_link_secret",
    );
    expect(window.location.pathname).toBe("/teams");
    expect(window.location.search).toBe("?from=admin");
    expect(window.location.hash).toBe("");
    expect(window.history.state).toEqual({ preserved: true });
  });

  it("rejects and removes an invalid invitation fragment", async () => {
    window.history.replaceState({}, "", "/#invite=not%20a%20token");
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<AuthScreen />, { wrapper: Wrapper });

    await screen.findByText("Sign in to continue");
    expect(window.location.hash).toBe("");
    expect(
      screen.queryByRole("heading", { name: "Join your Kuberploy team" }),
    ).not.toBeInTheDocument();
  });

  it("submits the one-time token and creates the signed-in session", async () => {
    const acceptedUser = {
      id: "user_invited",
      displayName: "Ada Lovelace",
      role: "developer",
    };
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const acceptInvitation = vi
      .spyOn(api, "acceptInvitation")
      .mockResolvedValue(acceptedUser);
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();
    render(<AuthScreen />, { wrapper: Wrapper });

    await screen.findByText("Sign in to continue");
    await user.click(
      screen.getByRole("button", { name: /use a team invitation/i }),
    );
    expect(
      screen.getByText(
        "Shown in the workspace; the invitation email is the sign-in identity.",
      ),
    ).toBeInTheDocument();
    await user.type(screen.getByLabelText(/display name/i), "Ada Lovelace");
    await user.type(
      screen.getByLabelText(/invitation token/i),
      "kp_invite_one_time",
    );
    await user.type(
      screen.getByLabelText(/^password/i),
      "developer password 123",
    );
    await user.click(
      screen.getByRole("button", { name: /accept invitation/i }),
    );

    await waitFor(() => expect(acceptInvitation).toHaveBeenCalledOnce());
    expect(acceptInvitation.mock.calls[0]?.[0]).toEqual({
      displayName: "Ada Lovelace",
      token: "kp_invite_one_time",
      password: "developer password 123",
    });
    expect(queryClient.getQueryData(["me"])).toEqual({
      ...acceptedUser,
      authentication: { kind: "session" },
    });
  });
});

describe("local password login", () => {
  it("uses email as the only login identity field", async () => {
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<AuthScreen />, { wrapper: Wrapper });

    await screen.findByRole("heading", { name: "Sign in to continue" });
    expect(screen.getByRole("textbox", { name: "Email" })).toHaveAttribute(
      "type",
      "email",
    );
    expect(screen.getByLabelText(/^Password/i)).toHaveAttribute(
      "type",
      "password",
    );
    expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/^display name/i)).not.toBeInTheDocument();
  });

  it("does not show bootstrap before installation metadata resolves", async () => {
    let resolveMeta!: (value: Awaited<ReturnType<typeof api.meta>>) => void;
    vi.spyOn(api, "meta").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveMeta = resolve;
        }),
    );
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );

    render(<AuthScreen />, { wrapper: Wrapper });

    expect(
      screen.getByRole("heading", { name: "Checking installation" }),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText(/admin email/i)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /sign in/i }),
    ).not.toBeInTheDocument();

    resolveMeta({ bootstrapRequired: false });
    expect(
      await screen.findByRole("heading", { name: "Sign in to continue" }),
    ).toBeInTheDocument();
  });

  it("restores a recurring session without retaining the password", async () => {
    const signedInUser = {
      id: "user_admin",
      displayName: "Local Admin",
      role: "platform-admin",
    };
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: false });
    const login = vi.spyOn(api, "login").mockResolvedValue(signedInUser);
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();
    render(<AuthScreen />, { wrapper: Wrapper });

    await screen.findByText("Sign in to continue");
    await user.type(screen.getByLabelText(/email/i), "admin@example.com");
    await user.type(
      screen.getByLabelText(/^password/i),
      "correct horse battery staple",
    );
    await user.click(screen.getByRole("button", { name: /^sign in/i }));

    await waitFor(() =>
      expect(login).toHaveBeenCalledWith(
        {
          email: "admin@example.com",
          password: "correct horse battery staple",
        },
        expect.anything(),
      ),
    );
    expect(queryClient.getQueryData(["me"])).toEqual({
      ...signedInUser,
      authentication: { kind: "session" },
    });
    expect(queryClient.getQueryData(["projects"])).toBeUndefined();
    expect(queryClient.getQueryData(["capabilities"])).toBeUndefined();
  });
});

describe("installation bootstrap", () => {
  it("stores the created administrator as a session principal", async () => {
    const administrator = {
      id: "user_admin",
      displayName: "Platform Admin",
      role: "platform-admin",
    };
    vi.spyOn(api, "meta").mockResolvedValue({ bootstrapRequired: true });
    vi.spyOn(api, "bootstrap").mockResolvedValue(administrator);
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const Wrapper = ({ children }: PropsWithChildren) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const user = userEvent.setup();
    render(<AuthScreen />, { wrapper: Wrapper });

    await screen.findByRole("heading", { name: "Claim this installation" });
    await user.type(
      screen.getByRole("textbox", { name: /^Admin email/ }),
      "admin@example.com",
    );
    const displayName = screen.getByRole("textbox", { name: /^Display name/ });
    await user.clear(displayName);
    await user.type(displayName, "Platform Admin");
    await user.type(
      screen.getByLabelText(/bootstrap token/i),
      "kp_bootstrap_token",
    );
    await user.type(
      screen.getByLabelText(/^password/i),
      "administrator password 123",
    );
    await user.click(
      screen.getByRole("button", { name: /create administrator/i }),
    );

    await waitFor(() =>
      expect(queryClient.getQueryData(["me"])).toEqual({
        ...administrator,
        authentication: { kind: "session" },
      }),
    );
  });
});
