import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Capability,
  ServiceAccount,
  ServiceAccountToken,
} from "../api/types";
import { ProjectAutomationPanel } from "./ProjectAutomationPanel";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: undefined,
  });
});

const account: ServiceAccount = {
  id: "service-account-1",
  projectId: "project-1",
  name: "release-bot",
  role: "developer",
  createdBy: "user-admin",
  createdAt: "2026-08-09T00:00:00Z",
};

const tokenRecord: ServiceAccountToken = {
  id: "token-1",
  serviceAccountId: account.id,
  name: "production deploy",
  prefix: "kp_sa_abcdefgh",
  scopes: ["app.read", "app.edit"],
  expiresAt: "2026-09-01T00:00:00Z",
  createdBy: "user-admin",
  createdAt: "2026-08-09T00:01:00Z",
};

const projectAdmin: Capability = {
  role: "project-admin",
  scopeType: "project",
  scopeId: "project-1",
  actions: [
    "access-grants:read",
    "access-grants:create",
    "access-grants:delete",
  ],
};

function wrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

function panel() {
  return (
    <ProjectAutomationPanel
      project={{ id: "project-1", name: "Payments" }}
      capabilities={[projectAdmin]}
      onClose={() => undefined}
    />
  );
}

describe("project service account management", () => {
  it("creates a project-bound account and reuses the key for an unchanged retry", async () => {
    vi.spyOn(api, "serviceAccounts").mockResolvedValue({ items: [] });
    const create = vi
      .spyOn(api, "createServiceAccount")
      .mockRejectedValueOnce(new Error("connection interrupted"))
      .mockResolvedValue(account);
    const user = userEvent.setup();
    render(panel(), { wrapper: wrapper() });

    await screen.findByText("No service accounts");
    await user.type(
      screen.getByRole("textbox", { name: /^Service account name/ }),
      " release-bot ",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Project role/ }),
      "developer",
    );
    await user.click(screen.getByRole("button", { name: "Create account" }));
    await screen.findByText("connection interrupted");
    await user.click(screen.getByRole("button", { name: "Create account" }));

    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
    expect(create.mock.calls[0]?.slice(0, 2)).toEqual([
      "project-1",
      { name: "release-bot", role: "developer" },
    ]);
    expect(create.mock.calls[0]?.[2]).toBe(create.mock.calls[1]?.[2]);
  });

  it("shows and copies a newly issued raw token exactly until dismissal", async () => {
    vi.spyOn(api, "serviceAccounts").mockResolvedValue({ items: [account] });
    vi.spyOn(api, "serviceAccountTokens")
      .mockResolvedValueOnce({ items: [] })
      .mockResolvedValue({ items: [tokenRecord] });
    const rawToken = `kp_sa_${"x".repeat(43)}`;
    const issue = vi.spyOn(api, "createServiceAccountToken").mockResolvedValue({
      tokenRecord,
      token: rawToken,
    });
    const user = userEvent.setup();
    const writeText = vi
      .spyOn(navigator.clipboard, "writeText")
      .mockResolvedValue(undefined);
    render(panel(), { wrapper: wrapper() });

    await user.click(
      await screen.findByRole("button", { name: "Credentials" }),
    );
    await screen.findByText("No token records for this account.");
    await user.type(
      screen.getByRole("textbox", { name: "Token name" }),
      "production deploy",
    );
    await user.click(screen.getByRole("checkbox", { name: /app\.edit/i }));
    await user.click(
      screen.getByRole("button", { name: "Issue one-time token" }),
    );

    const dialog = await screen.findByRole("alertdialog", {
      name: "Copy this token now",
    });
    expect(
      within(dialog).getByLabelText("New service account token"),
    ).toHaveTextContent(rawToken);
    expect(dialog).toHaveTextContent("cannot display this credential again");
    await user.click(
      within(dialog).getByRole("button", { name: "Copy token" }),
    );
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(rawToken));
    expect(
      within(dialog).getByRole("button", { name: "Copied" }),
    ).toBeEnabled();

    expect(issue).toHaveBeenCalledOnce();
    expect(issue.mock.calls[0]?.[0]).toBe(account.id);
    expect(issue.mock.calls[0]?.[1]).toMatchObject({
      name: "production deploy",
      scopes: ["app.read", "app.edit"],
    });
    expect(issue.mock.calls[0]?.[1].expiresAt).toMatch(/Z$/);
    expect(issue.mock.calls[0]?.[2]).toEqual(expect.any(String));

    await user.click(
      within(dialog).getByRole("button", { name: "I saved it — dismiss" }),
    );
    expect(screen.queryByText(rawToken)).not.toBeInTheDocument();
    expect(
      screen.getByText(`${tokenRecord.prefix}••••••••`),
    ).toBeInTheDocument();
  });

  it("never invents a raw credential when an idempotent issue is replayed", async () => {
    vi.spyOn(api, "serviceAccounts").mockResolvedValue({ items: [account] });
    vi.spyOn(api, "serviceAccountTokens").mockResolvedValue({
      items: [tokenRecord],
    });
    const issue = vi
      .spyOn(api, "createServiceAccountToken")
      .mockRejectedValueOnce(new Error("response lost"))
      .mockResolvedValue({ tokenRecord });
    const user = userEvent.setup();
    render(panel(), { wrapper: wrapper() });

    await user.click(
      await screen.findByRole("button", { name: "Credentials" }),
    );
    await user.type(
      screen.getByRole("textbox", { name: "Token name" }),
      "production deploy",
    );
    await user.click(
      screen.getByRole("button", { name: "Issue one-time token" }),
    );
    await screen.findByText("response lost");
    await user.click(
      screen.getByRole("button", { name: "Issue one-time token" }),
    );

    expect(
      await screen.findByText("Token created, credential no longer available"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("alertdialog", { name: "Copy this token now" }),
    ).not.toBeInTheDocument();
    expect(issue).toHaveBeenCalledTimes(2);
    expect(issue.mock.calls[0]?.[2]).toBe(issue.mock.calls[1]?.[2]);
  });

  it("requires exact confirmations before revoking tokens or disabling accounts", async () => {
    vi.spyOn(api, "serviceAccounts").mockResolvedValue({ items: [account] });
    vi.spyOn(api, "serviceAccountTokens").mockResolvedValue({
      items: [tokenRecord],
    });
    const revoke = vi
      .spyOn(api, "revokeServiceAccountToken")
      .mockResolvedValue(undefined);
    const disable = vi
      .spyOn(api, "disableServiceAccount")
      .mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(panel(), { wrapper: wrapper() });

    await user.click(
      await screen.findByRole("button", { name: "Credentials" }),
    );
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    let dialog = screen.getByRole("alertdialog", {
      name: `Revoke ${tokenRecord.name}?`,
    });
    const revokeButton = within(dialog).getByRole("button", {
      name: "Revoke exact token",
    });
    expect(revokeButton).toBeDisabled();
    await user.type(
      within(dialog).getByRole("textbox", { name: "Exact token prefix" }),
      tokenRecord.prefix,
    );
    await user.click(revokeButton);
    await waitFor(() => expect(revoke).toHaveBeenCalledOnce());
    expect(revoke.mock.calls[0]?.slice(0, 2)).toEqual([
      account.id,
      tokenRecord.id,
    ]);
    expect(revoke.mock.calls[0]?.[2]).toEqual(expect.any(String));

    await user.click(screen.getByRole("button", { name: "Disable" }));
    dialog = screen.getByRole("alertdialog", {
      name: `Disable ${account.name}?`,
    });
    const disableButton = within(dialog).getByRole("button", {
      name: "Disable and revoke tokens",
    });
    expect(disableButton).toBeDisabled();
    await user.type(
      within(dialog).getByRole("textbox", {
        name: "Exact service account name",
      }),
      account.name,
    );
    await user.click(disableButton);
    await waitFor(() => expect(disable).toHaveBeenCalledOnce());
    expect(disable.mock.calls[0]?.[0]).toBe(account.id);
    expect(disable.mock.calls[0]?.[1]).toEqual(expect.any(String));
  });
});
