import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./api/client";
import { RootComponent } from "./router";

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
