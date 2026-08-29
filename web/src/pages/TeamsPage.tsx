import {
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type {
  GitHubInstallation,
  Team,
  TeamMember,
  User,
  UserInvitation,
} from "../api/types";
import { Icon } from "../components/Icon";
import {
  Select,
  Button,
  Card,
  CardHeader,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
  EmptyState,
  ErrorPanel,
  Eyebrow,
  Field,
  MutedCopy,
  Notice,
  Page,
  PageHeader,
  Skeleton,
} from "../components/ui";
import { formatDate } from "../lib/format";
import { buildInvitationLink } from "../lib/invitationLink";
import { useCopyToClipboard } from "../lib/clipboard";
import { cn } from "@/lib/utils";

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
  // The picked team is a preference; which team is actually shown is derived
  // from the teams that exist in this render.
  const [teamChoice, setSelectedTeamId] = useState("");
  const [shareTarget, setShareTarget] = useState<GitHubInstallation | null>(
    null,
  );
  const [removeTarget, setRemoveTarget] = useState<TeamMember | null>(null);
  const [deleteTeamTarget, setDeleteTeamTarget] = useState<Team | null>(null);
  const [deleteUserTarget, setDeleteUserTarget] = useState<User | null>(null);
  const [invitation, setInvitation] = useState<UserInvitation | null>(null);

  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
  });
  const teams = useQuery({ queryKey: ["teams"], queryFn: api.teams });
  const selectedTeamId = teams.data?.items.some(
    (team) => team.id === teamChoice,
  )
    ? teamChoice
    : (teams.data?.items[0]?.id ?? "");
  const users = useQuery({ queryKey: ["users"], queryFn: api.users });
  const installations = useQuery({
    queryKey: ["github-installations"],
    queryFn: api.githubInstallations,
  });
  const accessibleTeamIDs = useMemo(
    () =>
      Array.from(
        new Set((teams.data?.items ?? []).map((team) => team.id)),
      ).sort(),
    [teams.data],
  );
  const accessibleTeamMembers = useQueries({
    queries: accessibleTeamIDs.map((teamId) => ({
      queryKey: ["github-installation-team-members", teamId],
      queryFn: () => api.teamMembers(teamId),
    })),
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
  const deleteTeamAttempt = useRef<{ targetId: string; key: string } | null>(
    null,
  );
  const deleteUserAttempt = useRef<{ targetId: string; key: string } | null>(
    null,
  );
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
  const deleteTeam = useMutation({
    mutationFn: (input: { team: Team; idempotencyKey: string }) =>
      api.deleteTeam(input.team.id, input.team.name, input.idempotencyKey),
    onSuccess: async (_value, input) => {
      if (
        deleteTeamTarget?.id === input.team.id &&
        deleteTeamAttempt.current?.key === input.idempotencyKey
      ) {
        deleteTeamAttempt.current = null;
        setDeleteTeamTarget(null);
      }
      await queryClient.invalidateQueries({ queryKey: ["teams"] });
    },
  });
  const deleteUser = useMutation({
    mutationFn: (input: { user: User; idempotencyKey: string }) =>
      api.deleteUser(
        input.user.id,
        input.user.email ?? "",
        input.idempotencyKey,
      ),
    onSuccess: async (_value, input) => {
      if (
        deleteUserTarget?.id === input.user.id &&
        deleteUserAttempt.current?.key === input.idempotencyKey
      ) {
        deleteUserAttempt.current = null;
        setDeleteUserTarget(null);
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["users"] }),
        queryClient.invalidateQueries({ queryKey: ["teams"] }),
      ]);
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
  const teamOwnerIDs = useMemo(() => {
    const owners = new Map<string, Set<string>>();
    accessibleTeamIDs.forEach((teamId, index) => {
      const members =
        accessibleTeamMembers[index]?.data?.items.filter(
          (member) => member.teamId === teamId,
        ) ?? [];
      owners.set(
        teamId,
        new Set(
          members
            .filter((member) => member.role === "owner")
            .map((member) => member.userId),
        ),
      );
    });
    return owners;
  }, [accessibleTeamIDs, accessibleTeamMembers]);
  const shareableTeams = useMemo(() => {
    if (me.data?.role === "platform-admin") return teams.data?.items ?? [];
    const userID = me.data?.id;
    if (!userID) return [];
    return (teams.data?.items ?? []).filter((team) =>
      teamOwnerIDs.get(team.id)?.has(userID),
    );
  }, [me.data, teamOwnerIDs, teams.data]);

  const loadError = teams.error ?? users.error ?? installations.error;

  return (
    <Page>
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
        <Card className="mb-5 py-5 px-6 border-mint-line">
          <CardHeader>
            <div>
              <Eyebrow>New collaboration boundary</Eyebrow>
              <h2>Create a team</h2>
            </div>
            <button
              className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid w-8 h-8 place-items-center border border-line rounded-lg text-ink-soft bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px]"
              onClick={() => setCreateOpen(false)}
              aria-label="Close team form"
            >
              <Icon name="close" />
            </button>
          </CardHeader>
          <form
            className="grid grid-cols-[1fr_1fr_auto] items-end gap-3 to-580:grid-cols-[1fr]"
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
              <div className="col-[1_/_-1] text-tone-bad text-meta">
                {errorMessage(createTeam.error)}
              </div>
            ) : null}
          </form>
        </Card>
      ) : null}

      <div className="grid grid-cols-[minmax(260px,_0.72fr)_minmax(440px,_1.28fr)] gap-5 mb-5 to-1120:grid-cols-[minmax(230px,_0.65fr)_minmax(390px,_1.35fr)] to-820:grid-cols-[1fr] page-to-760:grid-cols-[minmax(0,_1fr)]">
        <Card className="self-start">
          <CardHeader>
            <div>
              <Eyebrow>Accessible teams</Eyebrow>
              <h2>Teams</h2>
            </div>
            <span className="grid min-w-[27px] min-h-[27px] place-items-center py-0 px-2 border border-line rounded-full text-ink-soft bg-surface-soft text-meta font-semibold">
              {teams.data?.items.length ?? 0}
            </span>
          </CardHeader>
          {teams.isPending ? (
            <Skeleton lines={5} />
          ) : teams.data?.items.length ? (
            <div className="flex flex-col gap-1.5" role="list">
              {teams.data.items.map((team) => (
                <button
                  key={team.id}
                  type="button"
                  role="listitem"
                  className={cn(
                    "grid min-h-[57px] w-full grid-cols-[35px_minmax(0,1fr)_15px] items-center gap-2.5 rounded-[9px] border border-transparent px-2.5 py-2 text-left text-ink hover:bg-surface-soft",
                    "[&>span:nth-child(2)]:min-w-0",
                    // Names and slugs are identifiers: ellipsize rather than wrap.
                    "[&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_strong]:text-meta",
                    "[&_small]:mt-[3px] [&_small]:block [&_small]:overflow-hidden [&_small]:text-ellipsis [&_small]:whitespace-nowrap [&_small]:text-xs [&_small]:text-ink-faint",
                    "[&>svg]:w-[13px] [&>svg]:text-ink-faint",
                    selectedTeamId === team.id &&
                      "border-mint-line bg-mint-soft",
                  )}
                  onClick={() => setSelectedTeamId(team.id)}
                >
                  <span className="grid w-[34px] h-[34px] place-items-center border border-mint-line rounded-[9px] text-mint-dark bg-mint-soft text-meta font-bold">
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

        <Card className="[&_code]:text-xs [&_code]:text-ink-faint">
          <CardHeader>
            <div>
              <Eyebrow>Membership</Eyebrow>
              <h2>{selectedTeam?.name ?? "Select a team"}</h2>
            </div>
            {selectedTeam ? (
              <div className="flex flex-none items-center flex-wrap gap-y-2 gap-x-3 [&>code]:overflow-hidden [&>code]:max-w-[22ch] [&>code]:text-ink-faint [&>code]:text-xs [&>code]:text-ellipsis [&>code]:whitespace-nowrap">
                <code>{selectedTeam.slug}</code>
                {canManageSelectedTeam ? (
                  <Button
                    variant="danger"
                    onClick={() => {
                      deleteTeam.reset();
                      setDeleteTeamTarget(selectedTeam);
                    }}
                  >
                    Delete team
                  </Button>
                ) : null}
              </div>
            ) : null}
          </CardHeader>
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
                className="border border-line rounded-lg overflow-hidden"
                aria-label={`${selectedTeam.name} members`}
              >
                {members.data?.items.map((member) => {
                  const user = member.user ?? usersById.get(member.userId);
                  return (
                    <div
                      className="[&>span:nth-child(2)]:min-w-0 [&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_small]:block [&_small]:mt-1 [&_small]:overflow-hidden [&_small]:text-ink-faint [&_small]:text-xs [&_small]:text-ellipsis [&_small]:whitespace-nowrap grid min-h-[58px] grid-cols-[35px_minmax(0,_1fr)_auto_auto] items-center gap-3 py-2 px-3 border-b border-b-line last:border-b-0"
                      key={member.userId}
                    >
                      <span className="grid w-[34px] h-[34px] place-items-center border border-mint-line rounded-[9px] text-mint-dark bg-mint-soft text-meta font-bold">
                        {(user?.displayName ?? "U").slice(0, 1).toUpperCase()}
                      </span>
                      <span>
                        <strong>{user?.displayName ?? member.userId}</strong>
                        <small>{user?.email ?? `User ${member.userId}`}</small>
                      </span>
                      <span
                        className={cn(
                          "inline-flex min-h-[23px] items-center rounded-full border px-2 text-xs font-semibold",
                          member.role === "owner"
                            ? "border-tone-busy-line bg-tone-busy-surface text-tone-busy"
                            : "border-line bg-surface-soft text-ink-soft",
                        )}
                      >
                        {member.role === "owner" ? "Owner" : "Member"}
                      </span>
                      {canManageSelectedTeam ? (
                        <>
                          <TeamMemberRoleEditor
                            key={`${member.userId}:${member.role}`}
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
                            className="focus-visible:outline-[3px] focus-visible:outline-focus focus-visible:outline-offset-[2px] grid place-items-center border border-line rounded-lg bg-surface cursor-pointer transition-[color,border-color,background] duration-(--motion-fast) ease-(--ease-standard) [&_svg]:w-3.5 pointer-coarse:min-w-8 pointer-coarse:min-h-8 [&:hover:not(:disabled)]:text-ink [&:hover:not(:disabled)]:border-line-strong [&:hover:not(:disabled)]:bg-surface-soft [&:active:not(:disabled)]:translate-y-[1px] w-7 h-7 text-red [&>svg]:w-3"
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
                  <MutedCopy className="m-0 px-3 py-[18px]">
                    This team has no members.
                  </MutedCopy>
                ) : null}
              </div>
              {canManageSelectedTeam ? (
                <form
                  className="grid grid-cols-[minmax(160px,_1fr)_120px_auto] items-end gap-3 mt-4 pt-4 border-t border-t-line to-580:grid-cols-[1fr] page-to-560:grid-cols-[minmax(0,_1fr)]"
                  onSubmit={memberForm.handleSubmit(submitMember)}
                >
                  <Field
                    label="Add user"
                    required
                    error={memberForm.formState.errors.userId?.message}
                  >
                    <Select
                      {...memberForm.register("userId", {
                        required: "Select a user.",
                      })}
                      value={memberForm.watch("userId")}
                    >
                      <option value="">Select user</option>
                      {availableUsers.map((user) => (
                        <option key={user.id} value={user.id}>
                          {user.email
                            ? `${user.email} · ${user.displayName}`
                            : `${user.displayName} (email unavailable)`}
                        </option>
                      ))}
                    </Select>
                  </Field>
                  <Field label="Team role" required>
                    <Select
                      {...memberForm.register("role")}
                      value={memberForm.watch("role")}
                    >
                      <option value="member">Member</option>
                      <option value="owner">Owner</option>
                    </Select>
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
                    <div className="col-[1_/_-1] text-tone-bad text-meta">
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
        <Card className="grid grid-cols-[minmax(280px,_0.8fr)_minmax(360px,_1.2fr)] items-start gap-7 mb-5 [&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-1.5 [&_h2]:text-base to-820:grid-cols-[1fr] page-to-760:grid-cols-[minmax(0,_1fr)]">
          <div className="grid grid-cols-[42px_1fr] items-start gap-3 [&_p]:m-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.55]">
            <span className="grid w-10 h-10 place-items-center border border-mint-line rounded-[10px] text-mint-dark bg-mint-soft [&_svg]:w-[18px]">
              <Icon name="user" />
            </span>
            <div>
              <Eyebrow>Platform administrator</Eyebrow>
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
              className="grid grid-cols-[minmax(200px,_1fr)_auto] items-end gap-3 to-580:grid-cols-[1fr] page-to-560:grid-cols-[minmax(0,_1fr)]"
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
                <div className="col-[1_/_-1] text-tone-bad text-meta">
                  {errorMessage(createInvitation.error)}
                </div>
              ) : null}
            </form>
          )}
        </Card>
      ) : null}

      {me.data?.role === "platform-admin" ? (
        <Card className="mb-5">
          <CardHeader>
            <div>
              <Eyebrow>Platform administrator</Eyebrow>
              <h2>Users</h2>
              <p>Delete login access while preserving audit history.</p>
            </div>
            <span className="grid min-w-[27px] min-h-[27px] place-items-center py-0 px-2 border border-line rounded-full text-ink-soft bg-surface-soft text-meta font-semibold">
              {users.data?.items.length ?? 0}
            </span>
          </CardHeader>
          <div
            className="border border-line rounded-lg overflow-hidden"
            aria-label="Platform users"
          >
            {users.data?.items.map((user) => (
              <div
                className="[&>span:nth-child(2)]:min-w-0 [&_strong]:block [&_strong]:overflow-hidden [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_small]:block [&_small]:mt-1 [&_small]:overflow-hidden [&_small]:text-ink-faint [&_small]:text-xs [&_small]:text-ellipsis [&_small]:whitespace-nowrap grid min-h-[58px] grid-cols-[35px_minmax(0,_1fr)_auto_auto] items-center gap-3 py-2 px-3 border-b border-b-line last:border-b-0"
                key={user.id}
              >
                <span className="grid w-[34px] h-[34px] place-items-center border border-mint-line rounded-[9px] text-mint-dark bg-mint-soft text-meta font-bold">
                  {user.displayName.slice(0, 1).toUpperCase()}
                </span>
                <span>
                  <strong>{user.displayName}</strong>
                  <small>{user.email ?? "Email unavailable"}</small>
                </span>
                <span
                  className={cn(
                    "inline-flex min-h-[23px] items-center rounded-full border px-2 text-xs font-semibold",
                    user.role === "owner"
                      ? "border-tone-busy-line bg-tone-busy-surface text-tone-busy"
                      : "border-line bg-surface-soft text-ink-soft",
                  )}
                >
                  {user.role}
                </span>
                <Button
                  variant="danger"
                  disabled={user.id === me.data?.id || !user.email}
                  onClick={() => {
                    deleteUser.reset();
                    setDeleteUserTarget(user);
                  }}
                >
                  Delete user
                </Button>
              </div>
            ))}
          </div>
        </Card>
      ) : null}

      <Card className="mb-5">
        <CardHeader>
          <div>
            <Eyebrow>Repository authorization</Eyebrow>
            <h2>Accessible GitHub App installations</h2>
            <p>
              The API returns only installations you may use. Credentials and
              installation tokens are never exposed here.
            </p>
          </div>
          <span className="grid min-w-[27px] min-h-[27px] place-items-center py-0 px-2 border border-line rounded-full text-ink-soft bg-surface-soft text-meta font-semibold">
            {installations.data?.items.length ?? 0}
          </span>
        </CardHeader>
        {installations.isPending ? (
          <Skeleton lines={6} />
        ) : installations.data?.items.length ? (
          <div className="border border-line rounded-[10px] overflow-hidden">
            {installations.data.items.map((installation) => {
              const sharedTeam = installation.teamId
                ? teamsById.get(installation.teamId)
                : undefined;
              const canManageSharing =
                me.data?.role === "platform-admin" ||
                me.data?.id === installation.ownerUserId ||
                (installation.visibility === "team" &&
                  Boolean(
                    installation.teamId &&
                    teamOwnerIDs
                      .get(installation.teamId)
                      ?.has(me.data?.id ?? ""),
                  ));
              return (
                <article
                  className="grid min-h-[75px] grid-cols-[40px_minmax(150px,_1fr)_minmax(150px,_0.9fr)_minmax(120px,_0.7fr)_auto] items-center gap-3 py-3 px-3 border-b border-b-line last:border-b-0 to-1120:grid-cols-[40px_minmax(140px,_1fr)_minmax(130px,_0.9fr)_auto] to-1120:[&>[data-slot='button']]:row-[1_/_span_2] to-1120:[&>[data-slot='button']]:col-[4] to-820:grid-cols-[40px_minmax(0,_1fr)_auto] to-820:[&>[data-slot='button']]:row-[1_/_span_3] to-820:[&>[data-slot='button']]:col-[3] to-580:grid-cols-[35px_minmax(0,_1fr)] to-580:[&>[data-slot='button']]:row-[auto] to-580:[&>[data-slot='button']]:col-[2] to-580:[&>[data-slot='button']]:w-max to-580:[&>[data-slot='button']]:justify-self-start"
                  key={installation.id}
                >
                  <span className="grid w-10 h-10 place-items-center border border-mint-line rounded-[10px] text-mint-dark bg-mint-soft [&_svg]:w-[18px] to-580:w-[35px] to-580:h-[35px]">
                    <Icon name="git" />
                  </span>
                  <div className="flex min-w-0 flex-col items-start gap-1 [&_strong]:overflow-hidden [&_strong]:max-w-full [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_small]:overflow-hidden [&_small]:max-w-full [&_small]:text-ink-faint [&_small]:text-xs [&_small]:text-ellipsis [&_small]:whitespace-nowrap">
                    <strong>{installation.accountLogin}</strong>
                    <small>
                      {installation.accountType} · GitHub installation #
                      {installation.githubInstallationId}
                    </small>
                  </div>
                  <div className="flex min-w-0 flex-col items-start gap-1 [&_strong]:overflow-hidden [&_strong]:max-w-full [&_strong]:text-meta [&_strong]:text-ellipsis [&_strong]:whitespace-nowrap [&_span]:overflow-hidden [&_span]:max-w-full [&_span]:text-ink-faint [&_span]:text-xs [&_span]:text-ellipsis [&_span]:whitespace-nowrap to-820:col-[2] to-580:row-[auto] to-580:col-[2]">
                    <span>Repository access</span>
                    <strong>
                      {installation.repositorySelection} ·{" "}
                      {installation.repositoryCount} repositories
                    </strong>
                  </div>
                  <div className="flex min-w-0 flex-col items-start gap-1 [&_small]:overflow-hidden [&_small]:max-w-full [&_small]:text-ink-faint [&_small]:text-xs [&_small]:text-ellipsis [&_small]:whitespace-nowrap to-1120:col-[2] to-820:col-[2] to-580:row-[auto] to-580:col-[2]">
                    <span
                      className={cn(
                        "inline-flex min-h-[23px] items-center rounded-full border px-2 text-xs font-semibold",
                        installation.visibility === "team"
                          ? "border-tone-busy-line bg-tone-busy-surface text-tone-busy"
                          : "border-line bg-surface-soft text-ink-soft",
                      )}
                    >
                      {installation.visibility === "private"
                        ? "Private"
                        : "Team"}
                    </span>
                    <small>
                      {installation.visibility === "private"
                        ? "Installer and platform admins"
                        : (sharedTeam?.name ?? installation.teamId ?? "Team")}
                    </small>
                  </div>
                  {canManageSharing ? (
                    <Button
                      variant="secondary"
                      onClick={() => {
                        changeSharing.reset();
                        setShareTarget(installation);
                      }}
                    >
                      Change sharing
                    </Button>
                  ) : null}
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
          teams={shareableTeams}
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
      {removeTarget && removeTarget.teamId === selectedTeamId ? (
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
      {deleteTeamTarget ? (
        <ExactDeleteConfirmation
          kind="team"
          label={deleteTeamTarget.name}
          confirmation={deleteTeamTarget.name}
          busy={deleteTeam.isPending}
          error={deleteTeam.error}
          onCancel={() => {
            deleteTeam.reset();
            setDeleteTeamTarget(null);
          }}
          onConfirm={() => {
            const idempotencyKey =
              deleteTeamAttempt.current?.targetId === deleteTeamTarget.id
                ? deleteTeamAttempt.current.key
                : crypto.randomUUID();
            deleteTeamAttempt.current = {
              targetId: deleteTeamTarget.id,
              key: idempotencyKey,
            };
            deleteTeam.mutate({ team: deleteTeamTarget, idempotencyKey });
          }}
        />
      ) : null}
      {deleteUserTarget ? (
        <ExactDeleteConfirmation
          kind="user"
          label={deleteUserTarget.displayName}
          confirmation={deleteUserTarget.email ?? ""}
          busy={deleteUser.isPending}
          error={deleteUser.error}
          onCancel={() => {
            deleteUser.reset();
            setDeleteUserTarget(null);
          }}
          onConfirm={() => {
            const idempotencyKey =
              deleteUserAttempt.current?.targetId === deleteUserTarget.id
                ? deleteUserAttempt.current.key
                : crypto.randomUUID();
            deleteUserAttempt.current = {
              targetId: deleteUserTarget.id,
              key: idempotencyKey,
            };
            deleteUser.mutate({ user: deleteUserTarget, idempotencyKey });
          }}
        />
      ) : null}
    </Page>
  );
}

export function ExactDeleteConfirmation({
  kind,
  label,
  confirmation,
  busy,
  error,
  onCancel,
  onConfirm,
}: {
  kind: "user" | "team";
  label: string;
  confirmation: string;
  busy: boolean;
  error: unknown;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const [value, setValue] = useState("");
  const exact = value === confirmation;
  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onCancel()}>
      <DialogContent
        className="grid w-[min(480px,_100%)] gap-5 p-6 border border-line rounded-overlay bg-surface shadow-overlay [&_h2]:m-0 [&_h2]:text-[19px] [&_h2]:font-semibold [&_h2]:tracking-[-0.025em] [&_h2]:leading-[1.25] to-580:p-5 max-w-none"
        role="alertdialog"
        showCloseButton={false}
      >
        <span className="grid w-10 h-10 place-items-center border border-mint-line rounded-lg text-mint-dark bg-mint-soft [&_svg]:w-[19px] [&_svg]:h-[19px]">
          <Icon name="close" />
        </span>
        <Eyebrow>Permanent access removal</Eyebrow>
        <DialogTitle>
          Delete {kind} {label}?
        </DialogTitle>
        <DialogDescription>
          {kind === "user"
            ? "Login credentials, sessions, memberships, and grants will be removed. Audit history remains anonymized."
            : "Only a team with no projects, GitHub installations, setup handoffs, or secret bindings can be deleted."}
        </DialogDescription>
        <Field label={`Type ${confirmation} to confirm`} required>
          <input
            autoFocus
            value={value}
            aria-label={`Confirm ${kind} deletion`}
            onChange={(event) => setValue(event.target.value)}
          />
        </Field>
        {error ? (
          <Notice tone="error" role="alert">
            {errorMessage(error)}
          </Notice>
        ) : null}
        <div className="to-680:items-stretch to-680:flex-col flex justify-end flex-wrap gap-2 mt-1 to-460:[&_[data-slot='button']]:flex-auto">
          <Button variant="ghost" disabled={busy} onClick={onCancel}>
            Cancel
          </Button>
          <Button
            variant="danger"
            busy={busy}
            disabled={!exact}
            onClick={onConfirm}
          >
            Delete {kind}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function InvitationSecret({
  invitation,
  onDismiss,
}: {
  invitation: UserInvitation;
  onDismiss: () => void;
}) {
  const { state: copyState, copy } = useCopyToClipboard(4000);
  const invitationLink = buildInvitationLink(invitation.token);
  const copyStatus =
    copyState === "copied"
      ? "Copied. Share it through a secure channel now."
      : copyState === "failed"
        ? "Copy failed; select and copy the link manually."
        : "";

  return (
    <div
      className="[&>code]:block [&>code]:overflow-auto [&>code]:p-3 [&>code]:border [&>code]:border-tone-warn-line [&>code]:rounded-lg [&>code]:text-tone-warn [&>code]:bg-tone-warn-surface [&>code]:text-meta [&>code]:whitespace-nowrap [&_dl]:flex [&_dl]:gap-5 [&_dl]:my-3 [&_dl]:mx-0 [&_dl_>_div]:min-w-0 [&_dt]:text-ink-faint [&_dt]:text-xs [&_dd]:mt-1 [&_dd]:mx-0 [&_dd]:mb-0 [&_dd]:overflow-hidden [&_dd]:text-meta [&_dd]:text-ellipsis [&_dd]:whitespace-nowrap [&>p]:my-2 [&>p]:mx-0 [&>p]:text-ink-soft [&>p]:text-xs"
      role="status"
    >
      <Notice tone="warning">
        <div>
          <strong>Copy this invitation link now</strong>
          <p>
            Kuberploy will not show it again. The token stays in the URL
            fragment, so browsers do not send it to the server or in referrers.
            Share it through a secure channel.
          </p>
        </div>
      </Notice>
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
      <div className="flex flex-wrap gap-2">
        <Button variant="secondary" onClick={() => void copy(invitationLink)}>
          <Icon name={copyState === "copied" ? "check" : "copy"} /> Copy
          invitation link
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

  return (
    <div className="flex items-center gap-1.5 [&_[data-slot='button']]:min-h-7 [&_[data-slot='button']]:py-0 [&_[data-slot='button']]:px-2 [&_[data-slot='button']]:text-xs to-580:col-[2_/_-1] to-580:justify-start">
      <Select
        aria-label={`Role for ${displayName}`}
        className="min-h-7 max-w-[88px] rounded-md border-line bg-surface px-1.5 text-xs text-ink-soft"
        value={role}
        disabled={busy}
        onChange={(event) => setRole(event.target.value as MemberForm["role"])}
      >
        <option value="member">Member</option>
        <option value="owner">Owner</option>
      </Select>
      <Button
        variant="ghost"
        disabled={busy || role === member.role}
        busy={busy}
        onClick={() => onSave(role)}
      >
        Save role
      </Button>
      {error ? (
        <span className="col-[1_/_-1] text-tone-bad text-meta" role="alert">
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
        className="grid w-[min(480px,_100%)] gap-5 p-6 border border-line rounded-overlay bg-surface shadow-overlay [&_h2]:m-0 [&_h2]:text-[19px] [&_h2]:font-semibold [&_h2]:tracking-[-0.025em] [&_h2]:leading-[1.25] to-580:p-5 max-w-none"
        role="alertdialog"
        showCloseButton={false}
      >
        <span className="grid w-10 h-10 place-items-center border border-mint-line rounded-lg text-mint-dark bg-mint-soft [&_svg]:w-[19px] [&_svg]:h-[19px]">
          <Icon name="user" />
        </span>
        <Eyebrow>Membership access change</Eyebrow>
        <DialogTitle>Remove {displayName}?</DialogTitle>
        <DialogDescription>
          This removes the user from {teamName} and revokes their current
          sessions so removed access cannot remain active.
        </DialogDescription>
        {member.role === "owner" ? (
          <p className="!mt-2 !mx-0 !mb-0 !text-ink-faint !text-xs">
            The API will reject this change if this is the team's final owner.
          </p>
        ) : null}
        {error ? (
          <Notice tone="error" role="alert">
            {errorMessage(error)}
          </Notice>
        ) : null}
        <div className="to-680:items-stretch to-680:flex-col flex justify-end flex-wrap gap-2 mt-1 to-460:[&_[data-slot='button']]:flex-auto">
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
        className="grid w-[min(480px,_100%)] gap-5 p-6 border border-line rounded-overlay bg-surface shadow-overlay [&_h2]:m-0 [&_h2]:text-[19px] [&_h2]:font-semibold [&_h2]:tracking-[-0.025em] [&_h2]:leading-[1.25] to-580:p-5 w-[min(560px,_[&>.field]:mb-4_max-w-none"
        role="alertdialog"
        showCloseButton={false}
      >
        <span className="grid w-10 h-10 place-items-center border border-mint-line rounded-lg text-mint-dark bg-mint-soft [&_svg]:w-[19px] [&_svg]:h-[19px]">
          <Icon name="git" />
        </span>
        <Eyebrow>Explicit access change</Eyebrow>
        <DialogTitle>
          Change sharing for {installation.accountLogin}?
        </DialogTitle>
        <DialogDescription>
          This changes who can deploy repositories authorized by GitHub App
          installation #{installation.githubInstallationId}. It does not expose
          or copy installation credentials.
        </DialogDescription>

        <div
          className="grid grid-cols-[1fr_1fr] gap-2 mt-4 mx-0 mb-3 [&>label]:grid [&>label]:grid-cols-[16px_1fr] [&>label]:items-start [&>label]:gap-2 [&>label]:min-h-[74px] [&>label]:p-3 [&>label]:border [&>label]:border-line [&>label]:rounded-[9px] [&>label]:cursor-pointer [&>label:has(input:checked)]:border-mint [&>label:has(input:checked)]:bg-mint-soft [&_input]:w-3.5 [&_input]:min-h-3.5 [&_input]:mt-0.5 [&_input]:mx-0 [&_input]:mb-0 [&_input]:accent-mint-dark [&_strong]:block [&_strong]:text-meta [&_small]:block [&_small]:mt-1 [&_small]:text-ink-soft [&_small]:text-xs [&_small]:leading-[1.45] to-580:grid-cols-[1fr]"
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
                Only the installer and platform administrators can use it.
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
            <Select
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
            </Select>
          </Field>
        ) : null}

        <dl className="my-4 mx-0 py-0 px-3 border border-line rounded-lg bg-surface-soft [&>div]:grid [&>div]:grid-cols-[78px_minmax(0,_1fr)] [&>div]:items-center [&>div]:gap-3 [&>div]:min-h-[42px] [&>div]:border-t [&>div]:border-t-line [&>div:first-child]:border-t-0 [&_dt]:text-ink-faint [&_dt]:text-xs [&_dd]:min-w-0 [&_dd]:m-0 [&_dd]:overflow-hidden [&_dd]:text-meta [&_dd]:text-ellipsis [&_dd]:whitespace-nowrap [&_code]:text-xs">
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
        <label className="grid grid-cols-[17px_1fr] items-start gap-2 text-ink-soft cursor-pointer text-meta leading-[1.5] [&_input]:w-[15px] [&_input]:min-h-[15px] [&_input]:m-0 [&_input]:accent-mint-dark">
          <input
            type="checkbox"
            checked={confirmed}
            disabled={!changed || !valid}
            onChange={(event) => setConfirmed(event.target.checked)}
          />
          <span>
            I understand this changes repository App-source access for the exact
            GitHub App installation and team shown above.
          </span>
        </label>
        {!changed ? (
          <p
            className="!mt-2 !mx-0 !mb-0 !text-ink-faint !text-xs"
            role="status"
          >
            Choose a different visibility or team to continue.
          </p>
        ) : null}
        {error ? (
          <Notice tone="error" role="alert">
            {errorMessage(error)}
          </Notice>
        ) : null}
        <div className="to-680:items-stretch to-680:flex-col flex justify-end flex-wrap gap-2 mt-1 to-460:[&_[data-slot='button']]:flex-auto">
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
