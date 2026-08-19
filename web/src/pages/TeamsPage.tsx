import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type {
  GitHubInstallation,
  Team,
  TeamMember,
  UserInvitation,
} from "../api/types";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  PageHeader,
  Skeleton,
} from "../components/ui";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "../components/shadcn/dialog";
import { formatDate } from "../lib/format";
import { buildInvitationLink } from "../lib/invitationLink";

type TeamForm = { name: string; slug: string };
type MemberForm = { userId: string; role: "owner" | "member" };
type InvitationForm = { email: string };
type SharingInput = {
  visibility: "private" | "team";
  teamId?: string;
};

export function TeamsPage() {
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [selectedTeamId, setSelectedTeamId] = useState("");
  const [shareTarget, setShareTarget] = useState<GitHubInstallation | null>(
    null,
  );
  const [removeTarget, setRemoveTarget] = useState<TeamMember | null>(null);
  const [invitation, setInvitation] = useState<UserInvitation | null>(null);

  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
  });
  const teams = useQuery({ queryKey: ["teams"], queryFn: api.teams });
  const users = useQuery({ queryKey: ["users"], queryFn: api.users });
  const installations = useQuery({
    queryKey: ["github-installations"],
    queryFn: api.githubInstallations,
  });
  const members = useQuery({
    queryKey: ["teams", selectedTeamId, "members"],
    queryFn: () => api.teamMembers(selectedTeamId),
    enabled: Boolean(selectedTeamId),
  });

  const teamForm = useForm<TeamForm>({
    defaultValues: { name: "", slug: "" },
  });
  const memberForm = useForm<MemberForm>({
    defaultValues: { userId: "", role: "member" },
  });
  const invitationForm = useForm<InvitationForm>({
    defaultValues: { email: "" },
  });
  const teamAttempt = useRef<{ signature: string; key: string } | null>(null);
  const memberAttempt = useRef<{ signature: string; key: string } | null>(null);
  const memberRoleAttempts = useRef(
    new Map<string, { signature: string; key: string }>(),
  );
  const invitationAttempt = useRef<{
    signature: string;
    key: string;
  } | null>(null);
  const sharingAttempt = useRef<{
    installationId: string;
    signature: string;
    key: string;
  } | null>(null);
  const removeTargetRef = useRef<TeamMember | null>(null);
  removeTargetRef.current = removeTarget;

  useEffect(() => {
    memberForm.reset({ userId: "", role: "member" });
  }, [memberForm, selectedTeamId]);

  const createTeam = useMutation({
    mutationFn: ({
      input,
      idempotencyKey,
    }: {
      input: { name: string; slug?: string };
      idempotencyKey: string;
    }) => api.createTeam(input, idempotencyKey),
    onSuccess: async (team, input) => {
      if (teamAttempt.current?.key === input.idempotencyKey) {
        const current = teamForm.getValues();
        const submitted = {
          name: input.input.name,
          slug: input.input.slug ?? "",
        };
        if (JSON.stringify(current) === JSON.stringify(submitted)) {
          teamAttempt.current = null;
          teamForm.reset();
          setCreateOpen(false);
          setSelectedTeamId(team.id);
        }
      }
      await queryClient.invalidateQueries({ queryKey: ["teams"] });
    },
  });
  const addMember = useMutation({
    mutationFn: ({
      teamId,
      input,
      idempotencyKey,
    }: {
      teamId: string;
      input: MemberForm;
      idempotencyKey: string;
    }) => api.addTeamMember(teamId, input, idempotencyKey),
    onSuccess: async (_value, input) => {
      if (
        memberAttempt.current?.key === input.idempotencyKey &&
        input.teamId === selectedTeamId
      ) {
        const current = memberForm.getValues();
        if (JSON.stringify(current) === JSON.stringify(input.input)) {
          memberAttempt.current = null;
          memberForm.reset({ userId: "", role: "member" });
        }
      }
      await queryClient.invalidateQueries({
        queryKey: ["teams", input.teamId, "members"],
      });
    },
  });
  const updateMemberRole = useMutation({
    mutationFn: ({
      member,
      role,
      idempotencyKey,
    }: {
      member: TeamMember;
      role: MemberForm["role"];
      idempotencyKey: string;
    }) =>
      api.addTeamMember(
        member.teamId,
        { userId: member.userId, role },
        idempotencyKey,
      ),
    onSuccess: async (_value, input) => {
      const attempt = memberRoleAttempts.current.get(input.member.userId);
      if (attempt?.key === input.idempotencyKey) {
        memberRoleAttempts.current.delete(input.member.userId);
      }
      await queryClient.invalidateQueries({
        queryKey: ["teams", input.member.teamId, "members"],
      });
    },
  });
  const removeMember = useMutation({
    mutationFn: (member: TeamMember) =>
      api.removeTeamMember(member.teamId, member.userId),
    onSuccess: async (_, member) => {
      const currentTarget = removeTargetRef.current;
      if (
        member.teamId !== selectedTeamId ||
        !currentTarget ||
        currentTarget.teamId !== member.teamId ||
        currentTarget.userId !== member.userId
      ) {
        await queryClient.invalidateQueries({
          queryKey: ["teams", member.teamId, "members"],
        });
        return;
      }
      setRemoveTarget(null);
      await queryClient.invalidateQueries({
        queryKey: ["teams", member.teamId, "members"],
      });
    },
  });
  const createInvitation = useMutation({
    mutationFn: ({
      input,
      idempotencyKey,
    }: {
      input: InvitationForm;
      idempotencyKey: string;
    }) => api.createInvitation(input, idempotencyKey),
    onSuccess: (created, input) => {
      if (invitationAttempt.current?.key !== input.idempotencyKey) return;
      if (invitationForm.getValues().email === input.input.email) {
        invitationAttempt.current = null;
        invitationForm.reset();
        setInvitation(created);
      }
    },
  });
  const changeSharing = useMutation({
    mutationFn: (input: {
      installationId: string;
      value: SharingInput;
      idempotencyKey: string;
    }) => {
      return api.updateGitHubInstallationSharing(
        input.installationId,
        input.value,
        input.idempotencyKey,
      );
    },
    onSuccess: async (_value, input) => {
      if (
        shareTarget?.id === input.installationId &&
        sharingAttempt.current?.key === input.idempotencyKey
      ) {
        sharingAttempt.current = null;
        setShareTarget(null);
      }
      await queryClient.invalidateQueries({
        queryKey: ["github-installations"],
      });
    },
  });

  useEffect(() => {
    const firstTeam = teams.data?.items[0];
    const selectionStillExists = teams.data?.items.some(
      (team) => team.id === selectedTeamId,
    );
    if (!selectionStillExists) {
      setSelectedTeamId(firstTeam?.id ?? "");
      setRemoveTarget(null);
    }
  }, [selectedTeamId, teams.data]);

  const submitTeam = (values: TeamForm) => {
    const input = { name: values.name, slug: values.slug || undefined };
    const signature = JSON.stringify(input);
    const idempotencyKey =
      teamAttempt.current?.signature === signature
        ? teamAttempt.current.key
        : crypto.randomUUID();
    teamAttempt.current = { signature, key: idempotencyKey };
    createTeam.mutate({ input, idempotencyKey });
  };

  const submitMember = (input: MemberForm) => {
    const signature = JSON.stringify({ teamId: selectedTeamId, input });
    const idempotencyKey =
      memberAttempt.current?.signature === signature
        ? memberAttempt.current.key
        : crypto.randomUUID();
    memberAttempt.current = { signature, key: idempotencyKey };
    addMember.mutate({ teamId: selectedTeamId, input, idempotencyKey });
  };

  const submitInvitation = (input: InvitationForm) => {
    const signature = JSON.stringify(input);
    const idempotencyKey =
      invitationAttempt.current?.signature === signature
        ? invitationAttempt.current.key
        : crypto.randomUUID();
    invitationAttempt.current = { signature, key: idempotencyKey };
    createInvitation.mutate({ input, idempotencyKey });
  };

  const selectedTeam = teams.data?.items.find(
    (team) => team.id === selectedTeamId,
  );
  const usersById = useMemo(
    () => new Map(users.data?.items.map((user) => [user.id, user]) ?? []),
    [users.data],
  );
  const memberIds = useMemo(
    () => new Set(members.data?.items.map((member) => member.userId) ?? []),
    [members.data],
  );
  const availableUsers =
    users.data?.items.filter((user) => !memberIds.has(user.id)) ?? [];
  const canManageSelectedTeam =
    me.data?.role === "platform-admin" ||
    members.data?.items.some(
      (member) => member.userId === me.data?.id && member.role === "owner",
    ) ||
    capabilities.data?.capabilities?.some(
      (capability) =>
        capability.actions?.includes("team-members:write") &&
        ((capability.scopeType === "platform" &&
          capability.scopeId === "platform") ||
          (capability.scopeType === "team" &&
            capability.scopeId === selectedTeamId)),
    );
  const teamsById = useMemo(
    () => new Map(teams.data?.items.map((team) => [team.id, team]) ?? []),
    [teams.data],
  );

  const loadError = teams.error ?? users.error ?? installations.error;

  return (
    <div className="page">
      <PageHeader
        eyebrow="Identity & source access"
        title="Teams & GitHub Apps"
        description="Group users for collaboration and explicitly choose whether each accessible GitHub App installation stays private or is shared with one team."
        actions={
          <Button onClick={() => setCreateOpen(true)}>
            <Icon name="plus" /> Create team
          </Button>
        }
      />

      {loadError ? (
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([
              teams.refetch(),
              users.refetch(),
              installations.refetch(),
            ])
          }
        />
      ) : null}

      {createOpen ? (
        <Card className="creation-panel">
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">New collaboration boundary</span>
              <h2>Create a team</h2>
            </div>
            <button
              className="icon-button"
              onClick={() => setCreateOpen(false)}
              aria-label="Close team form"
            >
              <Icon name="close" />
            </button>
          </div>
          <form
            className="inline-form"
            onSubmit={teamForm.handleSubmit(submitTeam)}
          >
            <Field
              label="Team name"
              required
              error={teamForm.formState.errors.name?.message}
            >
              <input
                placeholder="Platform engineering"
                {...teamForm.register("name", {
                  required: "Enter a team name.",
                })}
              />
            </Field>
            <Field
              label="Slug"
              hint="Optional; generated by the API when omitted."
            >
              <input
                placeholder="platform-engineering"
                {...teamForm.register("slug")}
              />
            </Field>
            <Button type="submit" busy={createTeam.isPending}>
              Create team
            </Button>
            {createTeam.error ? (
              <div className="form-error">{errorMessage(createTeam.error)}</div>
            ) : null}
          </form>
        </Card>
      ) : null}

      <div className="teams-grid">
        <Card className="team-list-card">
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">Accessible teams</span>
              <h2>Teams</h2>
            </div>
            <span className="count-badge">{teams.data?.items.length ?? 0}</span>
          </div>
          {teams.isPending ? (
            <Skeleton lines={5} />
          ) : teams.data?.items.length ? (
            <div className="team-selector" role="list">
              {teams.data.items.map((team) => (
                <button
                  key={team.id}
                  type="button"
                  role="listitem"
                  className={`team-selector__item ${
                    selectedTeamId === team.id
                      ? "team-selector__item--active"
                      : ""
                  }`}
                  onClick={() => setSelectedTeamId(team.id)}
                >
                  <span className="team-avatar">
                    {team.name.slice(0, 2).toUpperCase()}
                  </span>
                  <span>
                    <strong>{team.name}</strong>
                    <small>{team.slug}</small>
                  </span>
                  <Icon name="chevron" />
                </button>
              ))}
            </div>
          ) : (
            <EmptyState
              icon="user"
              title="No team yet"
              description="Create a team before sharing a GitHub App installation."
              action={
                <Button variant="secondary" onClick={() => setCreateOpen(true)}>
                  <Icon name="plus" /> Create team
                </Button>
              }
              compact
            />
          )}
        </Card>

        <Card className="team-members-card">
          <div className="card__header card__header--inside">
            <div>
              <span className="eyebrow">Membership</span>
              <h2>{selectedTeam?.name ?? "Select a team"}</h2>
            </div>
            {selectedTeam ? <code>{selectedTeam.slug}</code> : null}
          </div>
          {!selectedTeam ? (
            <EmptyState
              icon="user"
              title="Choose a team"
              description="Select a team to inspect its owners and members."
              compact
            />
          ) : members.isPending ? (
            <Skeleton lines={5} />
          ) : members.error ? (
            <ErrorPanel
              error={members.error}
              onRetry={() => void members.refetch()}
            />
          ) : (
            <>
              <div
                className="member-list"
                aria-label={`${selectedTeam.name} members`}
              >
                {members.data?.items.map((member) => {
                  const user = member.user ?? usersById.get(member.userId);
                  return (
                    <div className="member-row" key={member.userId}>
                      <span className="user-avatar">
                        {(user?.displayName ?? "U").slice(0, 1).toUpperCase()}
                      </span>
                      <span>
                        <strong>{user?.displayName ?? member.userId}</strong>
                        <small>{user?.email ?? `User ${member.userId}`}</small>
                      </span>
                      <span
                        className={`member-role member-role--${member.role}`}
                      >
                        {member.role === "owner" ? "Owner" : "Member"}
                      </span>
                      {canManageSelectedTeam ? (
                        <>
                          <TeamMemberRoleEditor
                            member={member}
                            busy={
                              updateMemberRole.isPending &&
                              updateMemberRole.variables?.member.userId ===
                                member.userId
                            }
                            error={
                              updateMemberRole.variables?.member.userId ===
                              member.userId
                                ? updateMemberRole.error
                                : null
                            }
                            onSave={(role) => {
                              const signature = JSON.stringify({
                                teamId: member.teamId,
                                userId: member.userId,
                                role,
                              });
                              const previous = memberRoleAttempts.current.get(
                                member.userId,
                              );
                              const idempotencyKey =
                                previous?.signature === signature
                                  ? previous.key
                                  : crypto.randomUUID();
                              memberRoleAttempts.current.set(member.userId, {
                                signature,
                                key: idempotencyKey,
                              });
                              updateMemberRole.mutate({
                                member,
                                role,
                                idempotencyKey,
                              });
                            }}
                          />
                          <button
                            type="button"
                            className="icon-button member-remove-button"
                            aria-label={`Remove ${user?.displayName ?? member.userId} from ${selectedTeam.name}`}
                            onClick={() => {
                              removeMember.reset();
                              setRemoveTarget(member);
                            }}
                          >
                            <Icon name="close" />
                          </button>
                        </>
                      ) : null}
                    </div>
                  );
                })}
                {!members.data?.items.length ? (
                  <p className="muted-copy">This team has no members.</p>
                ) : null}
              </div>
              {canManageSelectedTeam ? (
                <form
                  className="member-form"
                  onSubmit={memberForm.handleSubmit(submitMember)}
                >
                  <Field
                    label="Add user"
                    required
                    error={memberForm.formState.errors.userId?.message}
                  >
                    <select
                      {...memberForm.register("userId", {
                        required: "Select a user.",
                      })}
                    >
                      <option value="">Select user</option>
                      {availableUsers.map((user) => (
                        <option key={user.id} value={user.id}>
                          {user.email
                            ? `${user.email} · ${user.displayName}`
                            : `${user.displayName} (email unavailable)`}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field label="Team role" required>
                    <select {...memberForm.register("role")}>
                      <option value="member">Member</option>
                      <option value="owner">Owner</option>
                    </select>
                  </Field>
                  <Button
                    type="submit"
                    variant="secondary"
                    busy={addMember.isPending}
                    disabled={!availableUsers.length}
                  >
                    <Icon name="plus" /> Add member
                  </Button>
                  {addMember.error ? (
                    <div className="form-error">
                      {errorMessage(addMember.error)}
                    </div>
                  ) : null}
                </form>
              ) : null}
            </>
          )}
        </Card>
      </div>

      {me.data?.role === "platform-admin" ? (
        <Card className="invitation-card">
          <div className="invitation-card__copy">
            <span className="invitation-card__icon">
              <Icon name="user" />
            </span>
            <div>
              <span className="eyebrow">Platform administrator</span>
              <h2>Invite a user</h2>
              <p>
                Create a short-lived invitation link without configuring an
                email provider. It is displayed only in this browser until you
                dismiss it.
              </p>
            </div>
          </div>
          {invitation ? (
            <InvitationSecret
              invitation={invitation}
              onDismiss={() => setInvitation(null)}
            />
          ) : (
            <form
              className="invite-form"
              onSubmit={invitationForm.handleSubmit(submitInvitation)}
            >
              <Field
                label="Invitee email"
                required
                error={invitationForm.formState.errors.email?.message}
              >
                <input
                  type="email"
                  autoComplete="email"
                  placeholder="teammate@example.com"
                  {...invitationForm.register("email", {
                    required: "Enter the invitee email.",
                  })}
                />
              </Field>
              <Button type="submit" busy={createInvitation.isPending}>
                Create one-time invitation
              </Button>
              {createInvitation.error ? (
                <div className="form-error">
                  {errorMessage(createInvitation.error)}
                </div>
              ) : null}
            </form>
          )}
        </Card>
      ) : null}

      <Card className="installations-card">
        <div className="card__header card__header--inside">
          <div>
            <span className="eyebrow">Repository authorization</span>
            <h2>Accessible GitHub App installations</h2>
            <p>
              The API returns only installations you may use. Credentials and
              installation tokens are never exposed here.
            </p>
          </div>
          <span className="count-badge">
            {installations.data?.items.length ?? 0}
          </span>
        </div>
        {installations.isPending ? (
          <Skeleton lines={6} />
        ) : installations.data?.items.length ? (
          <div className="installation-list">
            {installations.data.items.map((installation) => {
              const sharedTeam = installation.teamId
                ? teamsById.get(installation.teamId)
                : undefined;
              return (
                <article className="installation-row" key={installation.id}>
                  <span className="installation-row__mark">
                    <Icon name="git" />
                  </span>
                  <div className="installation-row__identity">
                    <strong>{installation.accountLogin}</strong>
                    <small>
                      {installation.accountType} · GitHub installation #
                      {installation.githubInstallationId}
                    </small>
                  </div>
                  <div className="installation-row__repositories">
                    <span>Repository access</span>
                    <strong>
                      {installation.repositorySelection} ·{" "}
                      {installation.repositoryCount} repositories
                    </strong>
                  </div>
                  <div className="installation-row__sharing">
                    <span
                      className={`sharing-badge sharing-badge--${installation.visibility}`}
                    >
                      {installation.visibility === "private"
                        ? "Private"
                        : "Team"}
                    </span>
                    <small>
                      {installation.visibility === "private"
                        ? "Installing user only"
                        : (sharedTeam?.name ?? installation.teamId ?? "Team")}
                    </small>
                  </div>
                  <Button
                    variant="secondary"
                    onClick={() => {
                      changeSharing.reset();
                      setShareTarget(installation);
                    }}
                  >
                    Change sharing
                  </Button>
                </article>
              );
            })}
          </div>
        ) : (
          <EmptyState
            icon="git"
            title="No accessible GitHub App installation"
            description="Install the Kuberploy GitHub App first. Its repository grant will appear here without revealing credentials."
            compact
          />
        )}
      </Card>

      {shareTarget ? (
        <InstallationSharingConfirmation
          installation={shareTarget}
          teams={teams.data?.items ?? []}
          busy={changeSharing.isPending}
          error={changeSharing.error}
          onCancel={() => setShareTarget(null)}
          onConfirm={(input) => {
            const signature = JSON.stringify({
              installationId: shareTarget.id,
              input,
            });
            const idempotencyKey =
              sharingAttempt.current?.installationId === shareTarget.id &&
              sharingAttempt.current.signature === signature
                ? sharingAttempt.current.key
                : crypto.randomUUID();
            sharingAttempt.current = {
              installationId: shareTarget.id,
              signature,
              key: idempotencyKey,
            };
            changeSharing.mutate({
              installationId: shareTarget.id,
              value: input,
              idempotencyKey,
            });
          }}
        />
      ) : null}
      {removeTarget ? (
        <RemoveMemberConfirmation
          member={removeTarget}
          teamName={teamsById.get(removeTarget.teamId)?.name ?? "this team"}
          busy={removeMember.isPending}
          error={removeMember.error}
          onCancel={() => {
            removeMember.reset();
            setRemoveTarget(null);
          }}
          onConfirm={() => removeMember.mutate(removeTarget)}
        />
      ) : null}
    </div>
  );
}

export function InvitationSecret({
  invitation,
  onDismiss,
}: {
  invitation: UserInvitation;
  onDismiss: () => void;
}) {
  const [copyStatus, setCopyStatus] = useState("");
  const invitationLink = buildInvitationLink(invitation.token);

  const copyInvitation = async () => {
    if (!navigator.clipboard) {
      setCopyStatus(
        "Clipboard access is unavailable; select and copy the link manually.",
      );
      return;
    }
    try {
      await navigator.clipboard.writeText(invitationLink);
      setCopyStatus("Copied. Share it through a secure channel now.");
    } catch {
      setCopyStatus("Copy failed; select and copy the link manually.");
    }
  };

  return (
    <div className="invitation-secret" role="status">
      <div className="notice notice--warning">
        <div>
          <strong>Copy this invitation link now</strong>
          <p>
            Kuberploy will not show it again. The token stays in the URL
            fragment, so browsers do not send it to the server or in referrers.
            Share it through a secure channel.
          </p>
        </div>
      </div>
      <code aria-label="One-time invitation link">{invitationLink}</code>
      <dl>
        <div>
          <dt>Invitee</dt>
          <dd>{invitation.email}</dd>
        </div>
        <div>
          <dt>Invitation</dt>
          <dd>{invitation.id}</dd>
        </div>
        <div>
          <dt>Expires</dt>
          <dd>{formatDate(invitation.expiresAt)}</dd>
        </div>
      </dl>
      {copyStatus ? <p aria-live="polite">{copyStatus}</p> : null}
      <div className="invitation-secret__actions">
        <Button variant="secondary" onClick={() => void copyInvitation()}>
          <Icon name="code" /> Copy invitation link
        </Button>
        <Button variant="ghost" onClick={onDismiss}>
          I saved it; dismiss
        </Button>
      </div>
    </div>
  );
}

export function TeamMemberRoleEditor({
  member,
  busy,
  error,
  onSave,
}: {
  member: TeamMember;
  busy: boolean;
  error: unknown;
  onSave: (role: MemberForm["role"]) => void;
}) {
  const [role, setRole] = useState<MemberForm["role"]>(member.role);
  const displayName = member.user?.displayName ?? member.userId;

  useEffect(() => {
    setRole(member.role);
  }, [member.role]);

  return (
    <div className="member-role-editor">
      <select
        aria-label={`Role for ${displayName}`}
        value={role}
        disabled={busy}
        onChange={(event) => setRole(event.target.value as MemberForm["role"])}
      >
        <option value="member">Member</option>
        <option value="owner">Owner</option>
      </select>
      <Button
        variant="ghost"
        disabled={busy || role === member.role}
        busy={busy}
        onClick={() => onSave(role)}
      >
        Save role
      </Button>
      {error ? (
        <span className="form-error" role="alert">
          {errorMessage(error)}
        </span>
      ) : null}
    </div>
  );
}

export function RemoveMemberConfirmation({
  member,
  teamName,
  busy,
  error,
  onCancel,
  onConfirm,
}: {
  member: TeamMember;
  teamName: string;
  busy: boolean;
  error: unknown;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const displayName = member.user?.displayName ?? member.userId;
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onCancel();
      }}
    >
      <DialogContent
        className="confirmation-dialog max-w-none"
        role="alertdialog"
        showCloseButton={false}
      >
        <span className="confirmation-dialog__icon">
          <Icon name="user" />
        </span>
        <span className="eyebrow">Membership access change</span>
        <DialogTitle>Remove {displayName}?</DialogTitle>
        <DialogDescription>
          This removes the user from {teamName} and revokes their current
          sessions so removed access cannot remain active.
        </DialogDescription>
        {member.role === "owner" ? (
          <p className="sharing-dialog__hint">
            The API will reject this change if this is the team's final owner.
          </p>
        ) : null}
        {error ? (
          <div className="notice notice--error" role="alert">
            {errorMessage(error)}
          </div>
        ) : null}
        <div className="confirmation-dialog__actions">
          <Button variant="ghost" disabled={busy} onClick={onCancel}>
            Cancel
          </Button>
          <Button variant="danger" busy={busy} onClick={onConfirm}>
            Remove member
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function InstallationSharingConfirmation({
  installation,
  teams,
  busy,
  error,
  onCancel,
  onConfirm,
}: {
  installation: GitHubInstallation;
  teams: Team[];
  busy: boolean;
  error: unknown;
  onCancel: () => void;
  onConfirm: (input: SharingInput) => void;
}) {
  const [visibility, setVisibility] = useState<"private" | "team">(
    installation.visibility,
  );
  const [teamId, setTeamId] = useState(
    installation.teamId ?? teams[0]?.id ?? "",
  );
  const [confirmed, setConfirmed] = useState(false);
  const previousTeam = teams.find((team) => team.id === installation.teamId);
  const targetTeam = teams.find((team) => team.id === teamId);
  const changed =
    visibility !== installation.visibility ||
    (visibility === "team" && teamId !== installation.teamId);
  const valid = visibility === "private" || Boolean(teamId);
  const before =
    installation.visibility === "private"
      ? "Private"
      : `Team: ${previousTeam?.name ?? installation.teamId ?? "unknown"}`;
  const after =
    visibility === "private"
      ? "Private"
      : `Team: ${(targetTeam?.name ?? teamId) || "not selected"}`;

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onCancel();
      }}
    >
      <DialogContent
        className="confirmation-dialog sharing-dialog max-w-none"
        role="alertdialog"
        showCloseButton={false}
      >
        <span className="confirmation-dialog__icon">
          <Icon name="git" />
        </span>
        <span className="eyebrow">Explicit access change</span>
        <DialogTitle>
          Change sharing for {installation.accountLogin}?
        </DialogTitle>
        <DialogDescription>
          This changes who can deploy repositories authorized by GitHub App
          installation #{installation.githubInstallationId}. It does not expose
          or copy installation credentials.
        </DialogDescription>

        <div
          className="sharing-options"
          role="radiogroup"
          aria-label="Sharing visibility"
        >
          <label>
            <input
              type="radio"
              name="sharing-visibility"
              value="private"
              checked={visibility === "private"}
              onChange={() => {
                setVisibility("private");
                setConfirmed(false);
              }}
            />
            <span>
              <strong>Private</strong>
              <small>
                Only the user who installed the GitHub App can use it.
              </small>
            </span>
          </label>
          <label className={!teams.length ? "sharing-option--disabled" : ""}>
            <input
              type="radio"
              name="sharing-visibility"
              value="team"
              checked={visibility === "team"}
              disabled={!teams.length}
              onChange={() => {
                setVisibility("team");
                setConfirmed(false);
              }}
            />
            <span>
              <strong>Team</strong>
              <small>Every member of one selected team can use it.</small>
            </span>
          </label>
        </div>

        {visibility === "team" ? (
          <Field label="Share with team" required>
            <select
              value={teamId}
              onChange={(event) => {
                setTeamId(event.target.value);
                setConfirmed(false);
              }}
            >
              <option value="">Select team</option>
              {teams.map((team) => (
                <option key={team.id} value={team.id}>
                  {team.name}
                </option>
              ))}
            </select>
          </Field>
        ) : null}

        <dl className="confirmation-identity">
          <div>
            <dt>Installation</dt>
            <dd>{installation.accountLogin}</dd>
          </div>
          <div>
            <dt>Current</dt>
            <dd>{before}</dd>
          </div>
          <div>
            <dt>After</dt>
            <dd>{after}</dd>
          </div>
        </dl>
        <label className="confirmation-check">
          <input
            type="checkbox"
            checked={confirmed}
            disabled={!changed || !valid}
            onChange={(event) => setConfirmed(event.target.checked)}
          />
          <span>
            I understand this changes repository deployment access for the exact
            GitHub App installation and team shown above.
          </span>
        </label>
        {!changed ? (
          <p className="sharing-dialog__hint" role="status">
            Choose a different visibility or team to continue.
          </p>
        ) : null}
        {error ? (
          <div className="notice notice--error" role="alert">
            {errorMessage(error)}
          </div>
        ) : null}
        <div className="confirmation-dialog__actions">
          <Button variant="ghost" disabled={busy} onClick={onCancel}>
            Cancel
          </Button>
          <Button
            disabled={!changed || !valid || !confirmed}
            busy={busy}
            onClick={() =>
              onConfirm(
                visibility === "private"
                  ? { visibility: "private" }
                  : { visibility: "team", teamId },
              )
            }
          >
            Apply sharing change <Icon name="arrow" />
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
