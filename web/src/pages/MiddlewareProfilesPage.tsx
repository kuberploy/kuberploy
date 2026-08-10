import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
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
  EmptyState,
  ErrorPanel,
  Field,
  Skeleton,
  StatusPill,
} from "../components/ui";

type Scope = MiddlewareProfileAssignment["scope"];

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
  const [formError, setFormError] = useState<string>();
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
      if (capability.scopeType === "platform") return true;
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
    setEditing(undefined);
    setName("");
    setDefinitions([defaultGuidedTraefikMiddleware("headers", "profile-spec")]);
    setFormError(undefined);
  };
  const save = useMutation({
    mutationFn: async () => {
      if (!assignment || definitions.length !== 1)
        throw new Error("Choose exactly one typed middleware family.");
      const spec = guidedTraefikMiddlewaresToValue(definitions)[0]?.spec as
        MiddlewareProfileSpec | undefined;
      if (!spec) throw new Error("Middleware specification is missing.");
      return editing
        ? api.reviseMiddlewareProfile(
            editing.profile.id,
            {
              baseRevision: editing.revision.revision,
              spec,
              assignments: editing.revision.assignments,
            },
            crypto.randomUUID(),
          )
        : api.createMiddlewareProfile(
            { name: name.trim(), spec, assignments: [assignment] },
            crypto.randomUUID(),
          );
    },
    onSuccess: async () => {
      reset();
      await client.invalidateQueries({
        queryKey: ["middleware-profile-catalog"],
      });
      await client.invalidateQueries({
        queryKey: ["assigned-middleware-profiles"],
      });
    },
    onError: (error) => setFormError(errorMessage(error)),
  });
  const deactivate = useMutation({
    mutationFn: (entry: MiddlewareProfileEntry) =>
      api.deactivateMiddlewareProfile(
        entry.profile.id,
        entry.revision.revision,
        crypto.randomUUID(),
      ),
    onSuccess: () =>
      client.invalidateQueries({ queryKey: ["middleware-profile-catalog"] }),
  });
  const clone = useMutation({
    mutationFn: (entry: MiddlewareProfileEntry) => {
      if (!assignment) throw new Error("Select an exact target first.");
      return api.cloneMiddlewareProfile(
        entry.profile.id,
        {
          name: `${entry.profile.name}-copy`.slice(0, 63),
          sourceRevision: entry.revision.revision,
          assignments: [assignment],
        },
        crypto.randomUUID(),
      );
    },
    onSuccess: () =>
      client.invalidateQueries({ queryKey: ["middleware-profile-catalog"] }),
  });

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
    <div className="settings-page">
      <div className="page-heading">
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
            {formError ? <ErrorPanel error={new Error(formError)} /> : null}
            <div className="button-row">
              <Button
                disabled={
                  !canMutate || !name.trim() || definitions.length !== 1
                }
                busy={save.isPending}
                onClick={() => save.mutate()}
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
                    onClick={() => clone.mutate(entry)}
                  >
                    Clone
                  </Button>
                  <Button
                    variant="danger"
                    disabled={
                      !canMutate || entry.profile.lifecycle !== "active"
                    }
                    busy={deactivate.isPending}
                    onClick={() => deactivate.mutate(entry)}
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
    </div>
  );
}
