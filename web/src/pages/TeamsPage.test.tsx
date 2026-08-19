import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "../api/client";
import type { GitHubInstallation, Team, TeamMember, User } from "../api/types";
import {
  InvitationSecret,
  InstallationSharingConfirmation,
  RemoveMemberConfirmation,
  TeamMemberRoleEditor,
  TeamsPage,
} from "./TeamsPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.replaceState({}, "", "/");
});

const teams: Team[] = [
  {
    id: "team_platform",
    name: "Platform engineering",
    slug: "platform-engineering",
    createdAt: "2026-08-06T00:00:00Z",
  },
  {
    id: "team_product",
    name: "Product engineering",
    slug: "product-engineering",
    createdAt: "2026-08-06T00:00:00Z",
  },
];

const privateInstallation: GitHubInstallation = {
  id: "installation_1",
  githubInstallationId: 4815162342,
  accountLogin: "kuberploy",
  accountType: "Organization",
  ownerUserId: "user_1",
  visibility: "private",
  repositorySelection: "selected",
  repositoryCount: 3,
  createdAt: "2026-08-06T00:00:00Z",
  updatedAt: "2026-08-06T00:00:00Z",
};

describe("copyable invitation link", () => {
  it("puts the one-time token only in a URL fragment and copies the full link", async () => {
    window.history.replaceState({}, "", "/teams?source=admin");
    const user = userEvent.setup();
    const writeText = vi
      .spyOn(navigator.clipboard, "writeText")
      .mockResolvedValue(undefined);

    render(
      <InvitationSecret
        invitation={{
          id: "invitation_1",
          email: "developer@example.com",
          token: "kp_invite_secret_value",
          expiresAt: "2026-08-11T00:00:00Z",
        }}
        onDismiss={vi.fn()}
      />,
    );

    const link = screen.getByLabelText("One-time invitation link").textContent;
    expect(link).toBeTruthy();
    expect(screen.getByText("developer@example.com")).toBeInTheDocument();
    const invitationURL = new URL(link!);
    expect(invitationURL.pathname).toBe("/");
    expect(invitationURL.search).toBe("");
    expect(invitationURL.search).not.toContain("kp_invite_secret");
    expect(invitationURL.hash).toBe("#invite=kp_invite_secret_value");

    await user.click(
      screen.getByRole("button", { name: "Copy invitation link" }),
    );
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(link));
    expect(screen.getByText(/Copied\. Share it/)).toBeInTheDocument();
  });
});

describe("team creation", () => {
  it("preserves a newer team draft when the earlier create completes", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({ capabilities: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: [] });
    vi.spyOn(api, "users").mockResolvedValue({ items: [] });
    vi.spyOn(api, "githubInstallations").mockResolvedValue({
      items: [],
      nextCursor: undefined,
    });
    let resolveCreate!: (
      value: Awaited<ReturnType<typeof api.createTeam>>,
    ) => void;
    const create = vi.spyOn(api, "createTeam").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveCreate = resolve;
        }),
    );
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={client}>
        <TeamsPage />
      </QueryClientProvider>,
    );

    await user.click(
      await screen.findByRole("button", { name: "Create team" }),
    );
    const name = screen.getByRole("textbox", { name: "Team name" });
    await user.type(name, "First team");
    await user.click(
      screen.getAllByRole("button", { name: "Create team" })[1]!,
    );
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    await user.clear(name);
    await user.type(name, "Newer team");

    resolveCreate({} as Awaited<ReturnType<typeof api.createTeam>>);
    await waitFor(() => expect(name).toHaveValue("Newer team"));
  });

  it("clears a team selection when access to that team disappears", async () => {
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({ capabilities: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: teams });
    vi.spyOn(api, "users").mockResolvedValue({ items: [] });
    vi.spyOn(api, "githubInstallations").mockResolvedValue({
      items: [],
      nextCursor: undefined,
    });
    vi.spyOn(api, "teamMembers").mockResolvedValue({ items: [] });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <TeamsPage />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByRole("heading", { name: teams[0]!.name }),
    ).toBeVisible();
    client.setQueryData(["teams"], { items: [] });

    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: "Select a team" }),
      ).toBeVisible(),
    );
    expect(screen.queryByText(teams[0]!.name)).toBeNull();
  });

  it("uses email as the primary identity in the member picker", async () => {
    const user: User = {
      id: "user_2",
      email: "grace@example.com",
      displayName: "Grace Hopper",
      role: "developer",
    };
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_admin",
      displayName: "Admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({ capabilities: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: teams });
    vi.spyOn(api, "users").mockResolvedValue({ items: [user] });
    vi.spyOn(api, "githubInstallations").mockResolvedValue({
      items: [],
      nextCursor: undefined,
    });
    vi.spyOn(api, "teamMembers").mockResolvedValue({ items: [] });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={client}>
        <TeamsPage />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByRole("option", {
        name: "grace@example.com · Grace Hopper",
      }),
    ).toBeInTheDocument();
  });
});

describe("GitHub App sharing confirmation", () => {
  it("requires an exact team selection and acknowledgement before sharing", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(
      <InstallationSharingConfirmation
        installation={privateInstallation}
        teams={teams}
        busy={false}
        error={null}
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />,
    );

    const submit = screen.getByRole("button", {
      name: /apply sharing change/i,
    });
    expect(submit).toBeDisabled();

    await user.click(screen.getByRole("radio", { name: /^team/i }));
    await user.selectOptions(
      screen.getByRole("combobox", { name: /share with team/i }),
      "team_product",
    );
    expect(screen.getByText("Team: Product engineering")).toBeInTheDocument();
    expect(submit).toBeDisabled();

    await user.click(screen.getByRole("checkbox"));
    expect(submit).toBeEnabled();
    await user.click(submit);

    expect(onConfirm).toHaveBeenCalledWith({
      visibility: "team",
      teamId: "team_product",
    });
  });

  it("omits teamId when making a team installation private", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(
      <InstallationSharingConfirmation
        installation={{
          ...privateInstallation,
          visibility: "team",
          teamId: "team_platform",
        }}
        teams={teams}
        busy={false}
        error={null}
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />,
    );

    await user.click(screen.getByRole("radio", { name: /^private/i }));
    await user.click(screen.getByRole("checkbox"));
    await user.click(
      screen.getByRole("button", { name: /apply sharing change/i }),
    );

    expect(onConfirm).toHaveBeenCalledWith({ visibility: "private" });
  });

  it("reuses the sharing idempotency key after a network failure", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "me").mockResolvedValue({
      id: "user_1",
      displayName: "Admin",
      role: "platform-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({ capabilities: [] });
    vi.spyOn(api, "teams").mockResolvedValue({ items: teams });
    vi.spyOn(api, "users").mockResolvedValue({ items: [] });
    vi.spyOn(api, "teamMembers").mockResolvedValue({ items: [] });
    vi.spyOn(api, "githubInstallations").mockResolvedValue({
      items: [privateInstallation],
      nextCursor: undefined,
    });
    const update = vi
      .spyOn(api, "updateGitHubInstallationSharing")
      .mockRejectedValueOnce(new Error("network unavailable"))
      .mockResolvedValue({
        ...privateInstallation,
        visibility: "team",
        teamId: "team_product",
      });
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });

    render(
      <QueryClientProvider client={client}>
        <TeamsPage />
      </QueryClientProvider>,
    );

    await user.click(
      await screen.findByRole("button", { name: "Change sharing" }),
    );
    await user.click(screen.getByRole("radio", { name: /^team/i }));
    await user.selectOptions(
      screen.getByRole("combobox", { name: /share with team/i }),
      "team_product",
    );
    await user.click(screen.getByRole("checkbox"));
    await user.click(
      screen.getByRole("button", { name: /apply sharing change/i }),
    );
    await waitFor(() => expect(update).toHaveBeenCalledOnce());

    await user.click(
      screen.getByRole("button", { name: /apply sharing change/i }),
    );
    await waitFor(() => expect(update).toHaveBeenCalledTimes(2));
    expect(update.mock.calls[1]?.[2]).toBe(update.mock.calls[0]?.[2]);
  });
});

describe("team member removal confirmation", () => {
  const member: TeamMember = {
    teamId: "team_platform",
    userId: "user_2",
    role: "owner",
    user: {
      id: "user_2",
      displayName: "Grace Hopper",
      role: "developer",
    },
    createdAt: "2026-08-06T00:00:00Z",
  };

  it("identifies the exact member, confirms explicitly, and retains API errors", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(
      <RemoveMemberConfirmation
        member={member}
        teamName="Platform engineering"
        busy={false}
        error={new Error("The final owner cannot be removed.")}
        onCancel={vi.fn()}
        onConfirm={onConfirm}
      />,
    );

    expect(
      screen.getByRole("heading", { name: "Remove Grace Hopper?" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/Platform engineering/)).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "The final owner cannot be removed.",
    );

    await user.click(screen.getByRole("button", { name: "Remove member" }));
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("closes through the shared dialog escape path", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    render(
      <RemoveMemberConfirmation
        member={member}
        teamName="Platform engineering"
        busy={false}
        error={null}
        onCancel={onCancel}
        onConfirm={vi.fn()}
      />,
    );

    expect(screen.getByRole("alertdialog")).toHaveAccessibleName(
      "Remove Grace Hopper?",
    );
    await user.keyboard("{Escape}");
    expect(onCancel).toHaveBeenCalledOnce();
  });
});

describe("team member role editor", () => {
  const member: TeamMember = {
    teamId: "team_platform",
    userId: "user_2",
    role: "member",
    user: {
      id: "user_2",
      displayName: "Grace Hopper",
      role: "developer",
    },
    createdAt: "2026-08-06T00:00:00Z",
  };

  it("only submits an explicit role change", async () => {
    const user = userEvent.setup();
    const onSave = vi.fn();
    render(
      <TeamMemberRoleEditor
        member={member}
        busy={false}
        error={null}
        onSave={onSave}
      />,
    );

    const save = screen.getByRole("button", { name: "Save role" });
    expect(save).toBeDisabled();
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Role for Grace Hopper" }),
      "owner",
    );
    expect(save).toBeEnabled();
    await user.click(save);
    expect(onSave).toHaveBeenCalledWith("owner");
  });
});
