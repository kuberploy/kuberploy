import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { api, errorMessage } from "../api/client";
import type {
  MiddlewareProfileAssignment,
  MiddlewareProfileEntry,
  MiddlewareProfileSpec,
} from "../api/types";
import {
  guidedTraefikMiddlewaresToValue,
  guidedTraefikMiddlewareState,
  defaultGuidedTraefikMiddleware,
  type GuidedTraefikMiddleware,
} from "../lib/traefikMiddleware";
import { TraefikMiddlewareEditor } from "../components/TraefikMiddlewareEditor";
import {
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  ErrorPanel,
  Field,
  Skeleton,
  StatusPill,
} from "../components/ui";

type Scope = MiddlewareProfileAssignment["scope"];
type ProfileSaveCommand = {
  idempotencyKey: string;
  editorScope: string;
  editorSession: number;
  assignment?: MiddlewareProfileAssignment;
  definitions: GuidedTraefikMiddleware[];
  profileId?: string;
  baseRevision?: number;
  baseAssignments?: MiddlewareProfileAssignment[];
  name: string;
};

export function MiddlewareProfilesPage() {
  const client = useQueryClient();
  const [environmentId, setEnvironmentId] = useState("");
  const [applicationId, setApplicationId] = useState("");
  const [scope, setScope] = useState<Scope>("application");
  const [name, setName] = useState("");
  const [definitions, setDefinitions] = useState<GuidedTraefikMiddleware[]>([
    defaultGuidedTraefikMiddleware("headers", "profile-spec"),
  ]);
  const [editing, setEditing] = useState<MiddlewareProfileEntry>();
  const [deactivationCandidate, setDeactivationCandidate] =
    useState<MiddlewareProfileEntry>();
  const [formError, setFormError] = useState<string>();
  const editorSessionRef = useRef(0);
  const saveAttempt = useRef<{ signature: string; key: string } | null>(null);
  const deactivateAttempt = useRef<{
    signature: string;
    key: string;
  } | null>(null);
  const cloneAttempt = useRef<{ signature: string; key: string } | null>(null);
  const editorScope = JSON.stringify({
    environmentId,
    applicationId,
    scope,
    profileId: editing?.profile.id ?? null,
    name,
    definitions,
  });
  const editorScopeRef = useRef(editorScope);
  editorScopeRef.current = editorScope;
  const me = useQuery({ queryKey: ["me"], queryFn: api.me, retry: false });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
  });
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
  });
  const applications = useQuery({
    queryKey: ["applications"],
    queryFn: api.applications,
  });
  const environment = environments.data?.items.find(
    ({ id }) => id === environmentId,
  );
  const application = applications.data?.items.find(
    ({ id }) => id === applicationId,
  );
  const project = projects.data?.items.find(
    ({ id }) => id === application?.projectId,
  );
  const ready =
    me.data?.authentication.kind === "session" &&
    capabilities.data?.features?.middlewareProfiles === true;
  const targetValid =
    Boolean(environment && application) &&
    environment?.projectId === application?.projectId;
  const catalog = useQuery({
    queryKey: ["middleware-profile-catalog", environmentId, applicationId],
    queryFn: () => api.middlewareProfileCatalog(environmentId, applicationId),
    enabled: ready && targetValid,
    retry: false,
  });
  const currentEditingEntry = editing
    ? catalog.data?.items.find(
        (entry) => entry.profile.id === editing.profile.id,
      )
    : undefined;
  const editingIsCurrent =
    !editing ||
    Boolean(
      currentEditingEntry &&
      currentEditingEntry.profile.lifecycle === "active" &&
      currentEditingEntry.revision.revision === editing.revision.revision,
    );
  const assignment = useMemo<MiddlewareProfileAssignment | undefined>(() => {
    if (!environment || !application || !project) return undefined;
    return {
      scope,
      id:
        scope === "project"
          ? project.id
          : scope === "environment"
            ? environment.id
            : application.id,
    };
  }, [application, environment, project, scope]);
  const canMutate = (capabilities.data?.capabilities ?? []).some(
    (capability) => {
      if (!assignment) return false;
      const action =
        assignment.scope === "application"
          ? "deployment-config:write"
          : "access-grants:create";
      if (!capability.actions?.includes(action)) return false;
      if (capability.scopeType === "platform")
        return capability.scopeId === "platform";
      if (capability.scopeType === "project")
        return capability.scopeId === project?.id;
      if (capability.scopeType === "environment")
        return capability.scopeId === environment?.id;
      if (capability.scopeType === "application")
        return capability.scopeId === application?.id;
      if (capability.scopeType === "team")
        return capability.scopeId === project?.teamId;
      if (capability.scopeType === "namespace")
        return capability.scopeId === environment?.namespace;
      return false;
    },
  );
  const reset = () => {
    editorSessionRef.current += 1;
    setEditing(undefined);
    setName("");
    setDefinitions([defaultGuidedTraefikMiddleware("headers", "profile-spec")]);
    setFormError(undefined);
  };
  useEffect(() => {
    if (!environment) {
      if (environmentId !== "" || applicationId !== "") {
        setEnvironmentId("");
        setApplicationId("");
        reset();
      }
      return;
    }
    if (!application || application.projectId !== environment.projectId) {
      if (applicationId !== "") {
        setApplicationId("");
        reset();
      }
    }
  }, [application, applicationId, environment, environmentId, reset]);
  const save = useMutation({
    mutationFn: async (input: ProfileSaveCommand) => {
      if (!input.assignment || input.definitions.length !== 1)
        throw new Error("Choose exactly one typed middleware family.");
      const spec = guidedTraefikMiddlewaresToValue(input.definitions)[0]
        ?.spec as MiddlewareProfileSpec | undefined;
      if (!spec) throw new Error("Middleware specification is missing.");
      return input.profileId
        ? api.reviseMiddlewareProfile(
            input.profileId,
            {
              baseRevision: input.baseRevision ?? 0,
              spec,
              assignments: input.baseAssignments ?? [],
            },
            input.idempotencyKey,
          )
        : api.createMiddlewareProfile(
            { name: input.name, spec, assignments: [input.assignment] },
            input.idempotencyKey,
          );
    },
    onSuccess: async (_value, input) => {
      const isCurrentEditor =
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current;
      if (
        isCurrentEditor &&
        saveAttempt.current?.key === input.idempotencyKey
      ) {
        saveAttempt.current = null;
      }
      if (isCurrentEditor) reset();
      await client.invalidateQueries({
        queryKey: ["middleware-profile-catalog"],
      });
      await client.invalidateQueries({
        queryKey: ["assigned-middleware-profiles"],
      });
    },
    onError: (error, input) => {
      if (
        input.editorScope === editorScopeRef.current &&
        input.editorSession === editorSessionRef.current
      ) {
        setFormError(errorMessage(error));
      }
    },
  });
  const deactivate = useMutation({
    mutationFn: ({
      entry,
      idempotencyKey,
    }: {
      entry: MiddlewareProfileEntry;
      idempotencyKey: string;
    }) =>
      api.deactivateMiddlewareProfile(
        entry.profile.id,
        entry.revision.revision,
        idempotencyKey,
      ),
    onSuccess: (_value, input) => {
      if (deactivateAttempt.current?.key === input.idempotencyKey) {
        deactivateAttempt.current = null;
      }
      return client.invalidateQueries({
        queryKey: ["middleware-profile-catalog"],
      });
    },
  });
  const clone = useMutation({
    mutationFn: ({
      entry,
      assignment: requestedAssignment,
      idempotencyKey,
    }: {
      entry: MiddlewareProfileEntry;
      assignment: MiddlewareProfileAssignment;
      idempotencyKey: string;
    }) => {
      return api.cloneMiddlewareProfile(
        entry.profile.id,
        {
          name: `${entry.profile.name}-copy`.slice(0, 63),
          sourceRevision: entry.revision.revision,
          assignments: [requestedAssignment],
        },
        idempotencyKey,
      );
    },
    onSuccess: (_value, input) => {
      if (cloneAttempt.current?.key === input.idempotencyKey) {
        cloneAttempt.current = null;
      }
      return client.invalidateQueries({
        queryKey: ["middleware-profile-catalog"],
      });
    },
  });

  const saveProfile = () => {
    if (!editingIsCurrent) return;
    const signature = JSON.stringify({
      profileId: editing?.profile.id,
      revision: editing?.revision.revision,
      name: name.trim(),
      assignment,
      definitions,
    });
    const idempotencyKey =
      saveAttempt.current?.signature === signature
        ? saveAttempt.current.key
        : crypto.randomUUID();
    saveAttempt.current = { signature, key: idempotencyKey };
    save.mutate({
      idempotencyKey,
      editorScope,
      editorSession: editorSessionRef.current,
      assignment,
      definitions,
      profileId: editing?.profile.id,
      baseRevision: editing?.revision.revision,
      baseAssignments: editing?.revision.assignments,
      name: name.trim(),
    });
  };

  const deactivateProfile = (entry: MiddlewareProfileEntry) => {
    const signature = JSON.stringify({
      profileId: entry.profile.id,
      revision: entry.revision.revision,
    });
    const idempotencyKey =
      deactivateAttempt.current?.signature === signature
        ? deactivateAttempt.current.key
        : crypto.randomUUID();
    deactivateAttempt.current = { signature, key: idempotencyKey };
    deactivate.mutate({ entry, idempotencyKey });
  };

  const cloneProfile = (entry: MiddlewareProfileEntry) => {
    const signature = JSON.stringify({
      profileId: entry.profile.id,
      revision: entry.revision.revision,
      assignment,
    });
    const idempotencyKey =
      cloneAttempt.current?.signature === signature
        ? cloneAttempt.current.key
        : crypto.randomUUID();
    cloneAttempt.current = { signature, key: idempotencyKey };
    if (!assignment) return;
    clone.mutate({ entry, assignment, idempotencyKey });
  };

  if (
    [me, capabilities, projects, environments, applications].some(
      (query) => query.isPending,
    )
  )
    return <Skeleton lines={8} />;
  if (!ready)
    return (
      <EmptyState
        title="Middleware profile management unavailable"
        description="This human-only library opens only when Traefik, Git projection, Argo serving readiness, and the reusable profile store are healthy."
      />
    );

  return (
    <div className="page page--narrow settings-page">
      <div className="page-header page-heading">
        <div>
          <span className="eyebrow">Application policy</span>
          <h1>Middleware profiles</h1>
          <p>
            Manage immutable reusable Traefik HTTP middleware revisions. The
            catalog is filtered to exact scopes you can administer.
          </p>
        </div>
      </div>
      <Card>
        <div className="form-grid form-grid--three">
          <Field label="Environment">
            <select
              value={environmentId}
              onChange={(event) => {
                setEnvironmentId(event.target.value);
                setApplicationId("");
                reset();
              }}
            >
              <option value="">Select environment</option>
              {environments.data?.items.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.name}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Application">
            <select
              value={applicationId}
              disabled={!environment}
              onChange={(event) => {
                setApplicationId(event.target.value);
                reset();
              }}
            >
              <option value="">Select application</option>
              {applications.data?.items
                .filter((item) => item.projectId === environment?.projectId)
                .map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.name}
                  </option>
                ))}
            </select>
          </Field>
          <Field label="New profile scope">
            <select
              value={scope}
              disabled={!targetValid}
              onChange={(event) => setScope(event.target.value as Scope)}
            >
              <option value="application">Application</option>
              <option value="environment">Environment</option>
              <option value="project">Project</option>
            </select>
          </Field>
        </div>
      </Card>
      {targetValid ? (
        <>
          <Card>
            <div className="card__header card__header--inside">
              <div>
                <h2>
                  {editing
                    ? "Append immutable revision"
                    : "Create reusable profile"}
                </h2>
                <p>
                  Exactly one closed, typed Traefik family is stored per
                  revision.
                </p>
              </div>
            </div>
            <Field label="Profile name">
              <input
                value={name}
                disabled={Boolean(editing)}
                maxLength={63}
                onChange={(event) => setName(event.target.value)}
              />
            </Field>
            <TraefikMiddlewareEditor
              definitions={definitions}
              refs={[]}
              issue=""
              routeEnabled={false}
              readOnly={!canMutate || save.isPending}
              applicationId={applicationId}
              environmentId={environmentId}
              onChange={({ definitions: next }) =>
                setDefinitions(next.slice(0, 1))
              }
            />
            {editing && !editingIsCurrent ? (
              <div className="notice notice--warning">
                This profile changed or is no longer active. Reload the current
                catalog before revising it.
              </div>
            ) : null}
            {formError ? <ErrorPanel error={new Error(formError)} /> : null}
            <div className="button-row">
              <Button
                disabled={
                  !canMutate ||
                  !editingIsCurrent ||
                  !name.trim() ||
                  definitions.length !== 1
                }
                busy={save.isPending}
                onClick={saveProfile}
              >
                {editing ? "Append revision" : "Create profile"}
              </Button>
              {editing ? (
                <Button variant="secondary" onClick={reset}>
                  Cancel
                </Button>
              ) : null}
            </div>
          </Card>
          <Card>
            <div className="card__header card__header--inside">
              <div>
                <h2>Authorized reusable library</h2>
                <p>
                  Only profiles assigned here whose complete current scope you
                  can manage are returned.
                </p>
              </div>
            </div>
            {clone.error ? (
              <ErrorPanel error={clone.error} title="Profile was not cloned" />
            ) : null}
            {deactivate.error ? (
              <ErrorPanel
                error={deactivate.error}
                title="Profile was not deactivated"
              />
            ) : null}
            {catalog.error ? <ErrorPanel error={catalog.error} /> : null}
            {(catalog.data?.items ?? []).map((entry) => (
              <div className="list-row" key={entry.profile.id}>
                <div>
                  <strong>{entry.profile.name}</strong>
                  <small>
                    Revision {entry.revision.revision} ·{" "}
                    {Object.keys(entry.revision.spec)[0]}
                  </small>
                </div>
                <StatusPill value={entry.profile.lifecycle} />
                <div className="button-row">
                  <Button
                    variant="secondary"
                    disabled={entry.profile.lifecycle !== "active"}
                    onClick={() => {
                      const parsed = guidedTraefikMiddlewareState(
                        [{ name: "profile-spec", spec: entry.revision.spec }],
                        [],
                      );
                      if (!parsed.definitions[0]) return;
                      editorSessionRef.current += 1;
                      setEditing(entry);
                      setName(entry.profile.name);
                      setDefinitions(parsed.definitions);
                    }}
                  >
                    Revise
                  </Button>
                  <Button
                    variant="secondary"
                    disabled={
                      !canMutate || entry.profile.lifecycle !== "active"
                    }
                    busy={clone.isPending}
                    onClick={() => cloneProfile(entry)}
                  >
                    Clone
                  </Button>
                  <Button
                    variant="danger"
                    disabled={
                      !canMutate || entry.profile.lifecycle !== "active"
                    }
                    busy={deactivate.isPending}
                    onClick={() => setDeactivationCandidate(entry)}
                  >
                    Deactivate
                  </Button>
                </div>
              </div>
            ))}
          </Card>
        </>
      ) : (
        <EmptyState
          compact
          title="Select an exact target"
          description="Choose an environment and application in the same project to load the authorized library."
        />
      )}
      {deactivationCandidate ? (
        <ConfirmDialog
          title={`Deactivate middleware profile ${deactivationCandidate.profile.name}?`}
          description="Existing assignments must be revised before this profile can be removed from use."
          confirmLabel="Deactivate profile"
          icon="close"
          busy={deactivate.isPending}
          onCancel={() => setDeactivationCandidate(undefined)}
          onConfirm={() => {
            const entry = deactivationCandidate;
            setDeactivationCandidate(undefined);
            deactivateProfile(entry);
          }}
        />
      ) : null}
    </div>
  );
}
