import { useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type {
  SchedulingProfileAssignment,
  SchedulingProfileEntry,
  SchedulingProfilePreferredNodeAffinity,
  SchedulingProfileSameApplicationPodAntiAffinity,
  SchedulingProfileSpec,
} from "../api/types";
import { SchedulingAffinityFields } from "../components/SchedulingAffinityFields";
import { formatDate, shortId } from "../lib/format";
import {
  Button,
  Card,
  EmptyState,
  ErrorPanel,
  Field,
  Skeleton,
  StatusPill,
} from "../components/ui";

const uuidPattern =
  "[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}";

function parseAssignments(value: string): SchedulingProfileAssignment[] {
  const assignments = value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      const separator = line.indexOf(":");
      const scope = line.slice(0, separator);
      const id = line.slice(separator + 1);
      if (
        separator < 1 ||
        !["team", "project", "environment"].includes(scope) ||
        !new RegExp(`^${uuidPattern}$`).test(id)
      ) {
        throw new Error(
          "Assignments must use one scope:UUID per line (team, project, or environment).",
        );
      }
      return { scope, id } as SchedulingProfileAssignment;
    });
  if (assignments.length === 0)
    throw new Error("Add at least one exact assignment.");
  return assignments;
}

function parseNodeSelector(value: string): Record<string, string> | undefined {
  const entries = value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      const separator = line.indexOf("=");
      if (separator < 1 || separator === line.length - 1)
        throw new Error("Node selectors must use one key=value per line.");
      return [line.slice(0, separator), line.slice(separator + 1)] as const;
    });
  return entries.length > 0 ? Object.fromEntries(entries) : undefined;
}

function parseArrayField<T>(value: string, label: string): T[] | undefined {
  if (value.trim() === "") return undefined;
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    throw new Error(`${label} must be a JSON array.`);
  }
  if (!Array.isArray(parsed)) throw new Error(`${label} must be a JSON array.`);
  return parsed.length > 0 ? (parsed as T[]) : undefined;
}

function linesForAssignments(assignments: SchedulingProfileAssignment[]) {
  return assignments.map((item) => `${item.scope}:${item.id}`).join("\n");
}

function linesForSelector(selector: Record<string, string> | undefined) {
  return Object.entries(selector ?? {})
    .map(([key, value]) => `${key}=${value}`)
    .join("\n");
}

export function SchedulingProfilesPage() {
  const client = useQueryClient();
  const form = useRef<HTMLFormElement>(null);
  const replayKey = useRef(crypto.randomUUID());
  const [editing, setEditing] = useState<SchedulingProfileEntry>();
  const [formError, setFormError] = useState<Error>();
  const [preferredNodeAffinity, setPreferredNodeAffinity] = useState<
    SchedulingProfilePreferredNodeAffinity[]
  >([]);
  const [sameApplicationPodAntiAffinity, setSameApplicationPodAntiAffinity] =
    useState<SchedulingProfileSameApplicationPodAntiAffinity[]>([]);
  const principal = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    staleTime: 60_000,
  });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    retry: false,
    staleTime: 60_000,
  });
  const allowed =
    principal.data?.role === "platform-admin" &&
    principal.data.authentication.kind === "session" &&
    capabilities.data?.features?.schedulingProfiles === true;
  const catalog = useQuery({
    queryKey: ["platform-scheduling-profiles"],
    queryFn: api.platformSchedulingProfiles,
    enabled: allowed,
    retry: false,
  });
  const save = useMutation({
    mutationFn: (input: {
      name: string;
      spec: SchedulingProfileSpec;
      assignments: SchedulingProfileAssignment[];
      editing?: SchedulingProfileEntry;
      key: string;
    }) =>
      input.editing
        ? api.revisePlatformSchedulingProfile(
            input.editing.profile.id,
            {
              baseRevision: input.editing.revision.revision,
              spec: input.spec,
              assignments: input.assignments,
            },
            input.key,
          )
        : api.createPlatformSchedulingProfile(
            {
              name: input.name,
              spec: input.spec,
              assignments: input.assignments,
            },
            input.key,
          ),
    onSuccess: async () => {
      setEditing(undefined);
      setFormError(undefined);
      setPreferredNodeAffinity([]);
      setSameApplicationPodAntiAffinity([]);
      form.current?.reset();
      replayKey.current = crypto.randomUUID();
      await client.invalidateQueries({
        queryKey: ["platform-scheduling-profiles"],
      });
    },
  });
  const deactivate = useMutation({
    mutationFn: (entry: SchedulingProfileEntry) =>
      api.deactivatePlatformSchedulingProfile(
        entry.profile.id,
        entry.revision.revision,
        crypto.randomUUID(),
      ),
    onSuccess: async () => {
      await client.invalidateQueries({
        queryKey: ["platform-scheduling-profiles"],
      });
    },
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    try {
      const data = new FormData(event.currentTarget);
      const name = String(data.get("name") ?? "").trim();
      const description = String(data.get("description") ?? "").trim();
      const priorityClassName = String(
        data.get("priorityClassName") ?? "",
      ).trim();
      const nodeSelector = parseNodeSelector(
        String(data.get("nodeSelector") ?? ""),
      );
      const requiredNodeAffinity = parseArrayField<
        NonNullable<
          SchedulingProfileSpec["pod"]["requiredNodeAffinity"]
        >[number]
      >(
        String(data.get("requiredNodeAffinity") ?? ""),
        "Required node affinity",
      );
      const tolerations = parseArrayField<
        NonNullable<SchedulingProfileSpec["pod"]["tolerations"]>[number]
      >(String(data.get("tolerations") ?? ""), "Tolerations");
      const topologySpread = parseArrayField<
        NonNullable<SchedulingProfileSpec["pod"]["topologySpread"]>[number]
      >(String(data.get("topologySpread") ?? ""), "Topology spread");
      const normalizedPreferredNodeAffinity = preferredNodeAffinity.map(
        (term) => ({
          weight: term.weight,
          requirements: term.requirements.map((requirement) => ({
            key: requirement.key.trim(),
            operator: requirement.operator,
            ...(requirement.values
              ? {
                  values: requirement.values
                    .map((value) => value.trim())
                    .filter(Boolean),
                }
              : {}),
          })),
        }),
      );
      setFormError(undefined);
      save.mutate({
        name,
        editing,
        key: replayKey.current,
        assignments: parseAssignments(String(data.get("assignments") ?? "")),
        spec: {
          ...(description ? { description } : {}),
          pod: {
            ...(nodeSelector ? { nodeSelector } : {}),
            ...(requiredNodeAffinity ? { requiredNodeAffinity } : {}),
            ...(normalizedPreferredNodeAffinity.length > 0
              ? { preferredNodeAffinity: normalizedPreferredNodeAffinity }
              : {}),
            ...(sameApplicationPodAntiAffinity.length > 0
              ? { sameApplicationPodAntiAffinity }
              : {}),
            ...(tolerations ? { tolerations } : {}),
            ...(topologySpread ? { topologySpread } : {}),
            ...(priorityClassName ? { priorityClassName } : {}),
          },
        },
      });
    } catch (error) {
      setFormError(error instanceof Error ? error : new Error("Invalid form"));
    }
  };

  if (principal.isPending || capabilities.isPending)
    return <Skeleton lines={6} />;
  if (!allowed)
    return (
      <EmptyState
        title="Scheduling profile management unavailable"
        description="This page requires a human platform-admin session and an available scheduling profile store."
      />
    );

  const defaults = editing?.revision;
  return (
    <div className="settings-page">
      <div className="page-heading">
        <div>
          <span className="eyebrow">Platform settings</span>
          <h1>Scheduling profiles</h1>
          <p>
            Publish immutable, assignable Pod scheduling policy. Kuberploy never
            mutates Nodes, taints, NodePools, or NodeClasses from this workflow.
          </p>
        </div>
      </div>
      <Card>
        <div className="card__header card__header--inside">
          <div>
            <h2>
              {editing ? "Append an immutable revision" : "Create a profile"}
            </h2>
            <p>
              Tenant deployment forms can only select an exact assigned
              revision; effective Pod placement fields remain server-owned.
            </p>
          </div>
        </div>
        <form
          key={
            editing
              ? `${editing.profile.id}:${editing.revision.revision}`
              : "create"
          }
          ref={form}
          className="form-grid"
          onChange={() => {
            replayKey.current = crypto.randomUUID();
            save.reset();
          }}
          onSubmit={submit}
        >
          <Field label="Profile name" required>
            <input
              name="name"
              defaultValue={editing?.profile.name ?? ""}
              disabled={Boolean(editing)}
              required
              maxLength={63}
              pattern="[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?"
              placeholder="stable-amd64"
            />
          </Field>
          <Field label="Description">
            <input
              name="description"
              defaultValue={defaults?.spec.description ?? ""}
              maxLength={512}
            />
          </Field>
          <Field
            label="Exact assignments"
            hint="One team:UUID, project:UUID, or environment:UUID per line."
            required
          >
            <textarea
              name="assignments"
              defaultValue={linesForAssignments(defaults?.assignments ?? [])}
              rows={3}
              required
              spellCheck={false}
            />
          </Field>
          <Field label="Node selectors" hint="One key=value per line.">
            <textarea
              name="nodeSelector"
              defaultValue={linesForSelector(defaults?.spec.pod.nodeSelector)}
              rows={3}
              spellCheck={false}
            />
          </Field>
          <Field label="Priority class">
            <input
              name="priorityClassName"
              defaultValue={defaults?.spec.pod.priorityClassName ?? ""}
              maxLength={63}
              pattern="[a-z0-9](?:[-a-z0-9]*[a-z0-9])?"
            />
          </Field>
          <Field label="Required node affinity" hint="Bounded JSON array.">
            <textarea
              name="requiredNodeAffinity"
              defaultValue={JSON.stringify(
                defaults?.spec.pod.requiredNodeAffinity ?? [],
                null,
                2,
              )}
              rows={5}
              spellCheck={false}
            />
          </Field>
          <SchedulingAffinityFields
            preferred={preferredNodeAffinity}
            antiAffinity={sameApplicationPodAntiAffinity}
            onPreferredChange={(value) => {
              setPreferredNodeAffinity(value);
              replayKey.current = crypto.randomUUID();
              save.reset();
            }}
            onAntiAffinityChange={(value) => {
              setSameApplicationPodAntiAffinity(value);
              replayKey.current = crypto.randomUUID();
              save.reset();
            }}
          />
          <Field
            label="Tolerations"
            hint="Bounded JSON array; broad tolerations are rejected."
          >
            <textarea
              name="tolerations"
              defaultValue={JSON.stringify(
                defaults?.spec.pod.tolerations ?? [],
                null,
                2,
              )}
              rows={5}
              spellCheck={false}
            />
          </Field>
          <Field label="Topology spread" hint="Bounded JSON array.">
            <textarea
              name="topologySpread"
              defaultValue={JSON.stringify(
                defaults?.spec.pod.topologySpread ?? [],
                null,
                2,
              )}
              rows={5}
              spellCheck={false}
            />
          </Field>
          <div className="button-row">
            <Button type="submit" busy={save.isPending}>
              {editing ? "Append revision" : "Create immutable profile"}
            </Button>
            {editing ? (
              <Button
                type="button"
                variant="secondary"
                onClick={() => {
                  setEditing(undefined);
                  setFormError(undefined);
                  setPreferredNodeAffinity([]);
                  setSameApplicationPodAntiAffinity([]);
                  replayKey.current = crypto.randomUUID();
                }}
              >
                Cancel revision
              </Button>
            ) : null}
          </div>
        </form>
        {formError ? (
          <ErrorPanel error={formError} title="Check the profile fields" />
        ) : null}
        {save.error ? (
          <ErrorPanel error={save.error} title="Profile was not saved" />
        ) : null}
      </Card>
      <Card>
        <div className="card__header card__header--inside">
          <div>
            <h2>Current immutable revisions</h2>
            <p>Revision and assignment digests are fixed after publication.</p>
          </div>
        </div>
        {catalog.isPending ? <Skeleton lines={5} /> : null}
        {catalog.error ? <ErrorPanel error={catalog.error} /> : null}
        {deactivate.error ? (
          <ErrorPanel
            error={deactivate.error}
            title="Profile was not deactivated"
          />
        ) : null}
        {catalog.data?.items.length === 0 ? (
          <EmptyState
            title="No scheduling profiles"
            description="Create the first immutable assigned profile above."
          />
        ) : null}
        <div className="helm-history-list">
          {catalog.data?.items.map((entry) => (
            <article className="helm-history-item" key={entry.profile.id}>
              <div>
                <strong>{entry.profile.name}</strong>
                <small>
                  Revision {entry.revision.revision} ·{" "}
                  {formatDate(entry.revision.createdAt)} ·{" "}
                  <code>{shortId(entry.profile.id)}</code>
                </small>
                <small>
                  {entry.revision.assignments
                    .map((item) => `${item.scope}:${item.id}`)
                    .join(", ")}
                </small>
                <small>
                  Spec <code>{entry.revision.specDigest}</code>
                </small>
                <small>
                  Assignments <code>{entry.revision.assignmentsDigest}</code>
                </small>
              </div>
              <div className="button-row">
                <StatusPill value={entry.profile.lifecycle} />
                {entry.profile.lifecycle === "active" ? (
                  <>
                    <Button
                      variant="secondary"
                      onClick={() => {
                        setEditing(entry);
                        setPreferredNodeAffinity(
                          structuredClone(
                            entry.revision.spec.pod.preferredNodeAffinity ?? [],
                          ),
                        );
                        setSameApplicationPodAntiAffinity(
                          structuredClone(
                            entry.revision.spec.pod
                              .sameApplicationPodAntiAffinity ?? [],
                          ),
                        );
                        replayKey.current = crypto.randomUUID();
                      }}
                    >
                      Revise
                    </Button>
                    <Button
                      variant="danger"
                      busy={deactivate.isPending}
                      onClick={() => {
                        if (
                          window.confirm(
                            `Deactivate ${entry.profile.name} revision ${entry.revision.revision}? Existing exact workload material remains auditable, but new selections will be rejected.`,
                          )
                        )
                          deactivate.mutate(entry);
                      }}
                    >
                      Deactivate
                    </Button>
                  </>
                ) : null}
              </div>
            </article>
          ))}
        </div>
      </Card>
    </div>
  );
}
