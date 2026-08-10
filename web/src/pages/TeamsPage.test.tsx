import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { GitHubInstallation, Team, TeamMember } from "../api/types";
import {
  InvitationSecret,
  InstallationSharingConfirmation,
  RemoveMemberConfirmation,
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
          token: "kp_invite_secret_value",
          expiresAt: "2026-08-11T00:00:00Z",
        }}
        onDismiss={vi.fn()}
      />,
    );

    const link = screen.getByLabelText("One-time invitation link").textContent;
    expect(link).toBeTruthy();
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
});
