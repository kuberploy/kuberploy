import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, errorMessage } from "../api/client";
import type {
  AccessGrant,
  AccessRole,
  AccessScopeType,
  Application,
  Capability,
  Environment,
  Project,
} from "../api/types";
import { Button, Field, Skeleton } from "./ui";

type GrantForm = {
  subjectType: "user" | "team";
  subjectUserId: string;
  subjectTeamId: string;
  role: Exclude<AccessRole, "platform-admin">;
  scope: string;
  logsRead: boolean;
};

type ScopeOption = {
  value: string;
  type: Exclude<AccessScopeType, "platform">;
  id: string;
  label: string;
};

const roleRank: Record<AccessRole, number> = {
  viewer: 10,
  developer: 20,
  "project-admin": 30,
  "organization-admin": 40,
  "platform-admin": 50,
};

const uuidPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function ProjectAccessPanel({
  project,
  environments,
  applications,
  capabilities,
  onClose,
}: {
  project: Project;
  environments: Environment[];
  applications: Application[];
  capabilities: Capability[];
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const createAttempt = useRef<{ signature: string; key: string } | null>(null);
  const [confirmGrant, setConfirmGrant] = useState<AccessGrant | null>(null);
  const [confirmation, setConfirmation] = useState("");
  const [confirmIdempotencyKey, setConfirmIdempotencyKey] = useState("");
  const confirmAttemptRef = useRef<{
    grantId: string;
    idempotencyKey: string;
  } | null>(null);
  confirmAttemptRef.current = confirmGrant
    ? { grantId: confirmGrant.id, idempotencyKey: confirmIdempotencyKey }
    : null;
  const grants = useQuery({
    queryKey: ["project-access-grants", project.id],
    queryFn: () => api.projectAccessGrants(project.id),
  });
  const teams = useQuery({ queryKey: ["teams"], queryFn: api.teams });
  const scopeOptions = useMemo<ScopeOption[]>(() => {
    const options: ScopeOption[] = [];
    if (project.teamId) {
      options.push({
        value: `team:${project.teamId}`,
        type: "team",
        id: project.teamId,
        label: `Organization · ${project.teamId}`,
      });
    }
    options.push({
      value: `project:${project.id}`,
      type: "project",
      id: project.id,
      label: `Project · ${project.name}`,
    });
    for (const environment of environments) {
      options.push({
        value: `environment:${environment.id}`,
        type: "environment",
        id: environment.id,
        label: `Environment · ${environment.name}`,
      });
      options.push({
        value: `namespace:${environment.namespace}`,
        type: "namespace",
        id: environment.namespace,
        label: `Namespace · ${environment.namespace}`,
      });
    }
    for (const application of applications) {
      options.push({
        value: `application:${application.id}`,
        type: "application",
        id: application.id,
        label: `Application · ${application.name}`,
      });
    }
    return options;
  }, [applications, environments, project]);
  const managingCapabilities = useMemo(
    () =>
      capabilities.filter(
        (capability) =>
          capability.role &&
          capability.actions?.includes("access-grants:create") &&
          capability.actions.includes("access-grants:delete"),
      ),
    [capabilities],
  );
  const capabilityCoversScope = (
    capability: Capability,
    scope: ScopeOption,
  ) => {
    if (capability.scopeType === "platform")
      return capability.scopeId === "platform";
    if (
      capability.scopeType === "team" &&
      capability.scopeId === project.teamId
    ) {
      return true;
    }
    return (
      capability.scopeType === "project" &&
      capability.scopeId === project.id &&
      scope.type !== "team"
    );
  };
  const canManageRoleAtScope = (role: AccessRole, scope: ScopeOption) =>
    managingCapabilities.some(
      (capability) =>
        capability.role &&
        roleRank[capability.role] >= roleRank[role] &&
        capabilityCoversScope(capability, scope),
    );
  const manageableScopeOptions = scopeOptions.filter((scope) =>
    canManageRoleAtScope("viewer", scope),
  );
  const form = useForm<GrantForm>({
    defaultValues: {
      subjectType: "user",
      subjectUserId: "",
      subjectTeamId: "",
      role: "developer",
      scope: `project:${project.id}`,
      logsRead: false,
    },
  });
  const selectedRole = form.watch("role");
  const roleScopeOptions = manageableScopeOptions.filter((scope) => {
    if (selectedRole === "organization-admin") return scope.type === "team";
    if (selectedRole === "project-admin") return scope.type === "project";
    return canManageRoleAtScope(selectedRole, scope);
  });
  const assignableRoles = (
    ["viewer", "developer", "project-admin", "organization-admin"] as const
  ).filter((role) =>
    manageableScopeOptions.some((scope) => {
      if (role === "organization-admin" && scope.type !== "team") return false;
      if (role === "project-admin" && scope.type !== "project") return false;
      return canManageRoleAtScope(role, scope);
    }),
  );
  useEffect(() => {
    const current = form.getValues("scope");
    if (!roleScopeOptions.some((scope) => scope.value === current)) {
      form.setValue("scope", roleScopeOptions[0]?.value ?? "");
    }
  }, [form, roleScopeOptions]);
  const createGrant = useMutation({
    mutationFn: (input: {
      projectId: string;
      value: GrantForm;
      idempotencyKey: string;
    }) => {
      const { value } = input;
      const selected = scopeOptions.find(
        (scope) => scope.value === value.scope,
      );
      if (!selected) throw new Error("Select an exact access scope.");
      return api.createProjectAccessGrant(
        input.projectId,
        {
          ...(value.subjectType === "team"
            ? { subjectTeamId: value.subjectTeamId }
            : { subjectUserId: value.subjectUserId.trim() }),
          role: value.role,
          scopeType: selected.type,
          scopeId: selected.id,
          permissions: value.logsRead ? ["logs.read"] : [],
        },
        input.idempotencyKey,
      );
    },
    onSuccess: async (_result, input) => {
      if (input.projectId === project.id) {
        const current = form.getValues();
        const currentSignature = JSON.stringify({
          ...current,
          subjectUserId: current.subjectUserId.trim(),
        });
        if (currentSignature === JSON.stringify(input.value)) {
          createAttempt.current = null;
          form.reset({
            subjectType: "user",
            subjectUserId: "",
            subjectTeamId: "",
            role: "developer",
            scope: `project:${project.id}`,
            logsRead: false,
          });
        }
      }
      await queryClient.invalidateQueries({
        queryKey: ["project-access-grants", input.projectId],
      });
    },
  });
  const submitGrant = (value: GrantForm) => {
    const normalized = {
      ...value,
      subjectUserId: value.subjectUserId.trim(),
    };
    const signature = JSON.stringify(normalized);
    const key =
      createAttempt.current?.signature === signature
        ? createAttempt.current.key
        : crypto.randomUUID();
    createAttempt.current = { signature, key };
    createGrant.mutate({
      projectId: project.id,
      value: normalized,
      idempotencyKey: key,
    });
  };
  const canDeleteGrant = (grant: AccessGrant) => {
    const scope = scopeOptions.find(
      (option) =>
        option.type === grant.scopeType && option.id === grant.scopeId,
    );
    return (
      grant.source === "explicit" &&
      !!scope &&
      managingCapabilities.some(
        (capability) =>
          capability.role &&
          roleRank[capability.role] >= roleRank[grant.role] &&
          capabilityCoversScope(capability, scope),
      )
    );
  };
  const deleteGrant = useMutation({
    mutationFn: (input: {
      projectId: string;
      grant: AccessGrant;
      idempotencyKey: string;
    }) =>
      api.deleteProjectAccessGrant(
        input.projectId,
        input.grant.id,
        input.idempotencyKey,
      ),
    onSuccess: async (_result, input) => {
      if (
        input.projectId === project.id &&
        confirmAttemptRef.current?.grantId === input.grant.id &&
        confirmAttemptRef.current.idempotencyKey === input.idempotencyKey
      ) {
        setConfirmGrant(null);
        setConfirmation("");
        setConfirmIdempotencyKey("");
      }
      await queryClient.invalidateQueries({
        queryKey: ["project-access-grants", input.projectId],
      });
    },
  });
  useEffect(() => {
    form.reset({
      subjectType: "user",
      subjectUserId: "",
      subjectTeamId: "",
      role: "developer",
      scope: `project:${project.id}`,
      logsRead: false,
    });
    createAttempt.current = null;
    setConfirmGrant(null);
    setConfirmation("");
    setConfirmIdempotencyKey("");
    createGrant.reset();
    deleteGrant.reset();
  }, [project.id]);

  return (
    <div className="access-panel" aria-label={`${project.name} access`}>
      <div className="access-panel__header">
        <div>
          <span className="eyebrow">Authorization</span>
          <h3>Project access</h3>
          <p>
            Grants are additive. Team owners remain organization administrators,
            and team members remain developers for team projects.
          </p>
        </div>
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      </div>

      <form className="access-form" onSubmit={form.handleSubmit(submitGrant)}>
        <Field label="Subject type" required>
          <select {...form.register("subjectType")}>
            <option value="user">User</option>
            <option value="team">Team</option>
          </select>
        </Field>
        {form.watch("subjectType") === "user" ? (
          <Field
            label="Exact user ID"
            required
            hint="Use the immutable user ID; names are display-only and can change."
            error={form.formState.errors.subjectUserId?.message}
          >
            <input
              placeholder="00000000-0000-4000-8000-000000000000"
              {...form.register("subjectUserId", {
                required: "Enter the exact user ID.",
                pattern: {
                  value: uuidPattern,
                  message: "Enter a canonical UUID user ID.",
                },
              })}
            />
          </Field>
        ) : (
          <Field label="Team" required>
            <select
              {...form.register("subjectTeamId", {
                required: "Select an exact team.",
              })}
            >
              <option value="">Select team</option>
              {teams.data?.items.map((team) => (
                <option key={team.id} value={team.id}>
                  {team.name}
                </option>
              ))}
            </select>
          </Field>
        )}
        <Field label="Role" required>
          <select {...form.register("role")}>
            {assignableRoles.map((role) => (
              <option key={role} value={role}>
                {role === "viewer"
                  ? "Viewer"
                  : role === "developer"
                    ? "Developer"
                    : role === "project-admin"
                      ? "Project admin"
                      : "Organization admin"}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Exact scope" required>
          <select {...form.register("scope")}>
            {roleScopeOptions.map((scope) => (
              <option key={scope.value} value={scope.value}>
                {scope.label}
              </option>
            ))}
          </select>
        </Field>
        <label className="access-form__check">
          <input type="checkbox" {...form.register("logsRead")} />
          Add logs.read (optional for viewers)
        </label>
        <Button type="submit" busy={createGrant.isPending}>
          Add grant
        </Button>
      </form>
      {createGrant.error ? (
        <div className="form-error">{errorMessage(createGrant.error)}</div>
      ) : null}

      {grants.isPending ? (
        <Skeleton lines={3} />
      ) : grants.error ? (
        <div className="form-error">{errorMessage(grants.error)}</div>
      ) : grants.data.items.length ? (
        <div className="access-list">
          {grants.data.items.map((grant) => (
            <div className="access-row" key={grant.id}>
              <div>
                <strong>{grant.role}</strong>
                <span>
                  {grant.scopeType} · <code>{grant.scopeId}</code>
                </span>
                <small>
                  {grant.subjectTeamId ? "Team" : "User"}{" "}
                  <code>{grant.subjectTeamId ?? grant.subjectUserId}</code>
                  {grant.permissions.length
                    ? ` · ${grant.permissions.join(", ")}`
                    : ""}
                </small>
              </div>
              {canDeleteGrant(grant) ? (
                <Button
                  variant="danger"
                  onClick={() => {
                    setConfirmGrant(grant);
                    setConfirmation("");
                    setConfirmIdempotencyKey(crypto.randomUUID());
                  }}
                >
                  Remove
                </Button>
              ) : null}
            </div>
          ))}
        </div>
      ) : (
        <p className="muted-copy">No explicit grants for this project.</p>
      )}

      {confirmGrant ? (
        <div className="access-confirm" role="alertdialog" aria-modal="true">
          <strong>Confirm the exact grant</strong>
          <p>
            Type <code>{confirmGrant.id}</code> to revoke this assignment and
            invalidate every affected user session.
          </p>
          <input
            aria-label="Exact grant ID confirmation"
            value={confirmation}
            onChange={(event) => setConfirmation(event.target.value)}
          />
          <div>
            <Button
              variant="danger"
              disabled={confirmation !== confirmGrant.id}
              busy={deleteGrant.isPending}
              onClick={() =>
                deleteGrant.mutate({
                  projectId: project.id,
                  grant: confirmGrant,
                  idempotencyKey: confirmIdempotencyKey,
                })
              }
            >
              Revoke exact grant
            </Button>
            <Button
              variant="secondary"
              onClick={() => {
                setConfirmGrant(null);
                setConfirmation("");
                setConfirmIdempotencyKey("");
              }}
            >
              Cancel
            </Button>
          </div>
          {deleteGrant.error ? (
            <div className="form-error">{errorMessage(deleteGrant.error)}</div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
