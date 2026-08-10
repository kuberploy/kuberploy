import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "../api/client";
import type {
  Capability,
  Environment,
  Operation,
  VariableSetPreview,
  VariableSetSnapshot,
} from "../api/types";
import { Icon } from "../components/Icon";
import {
  Button,
  Card,
  ErrorPanel,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
import { shortId } from "../lib/format";

const emptyVariableSet =
  'apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\nvalues:\n  EXAMPLE_FLAG: "true"\n';

function scopeLabel(scope: VariableSetSnapshot["scope"]) {
  return scope === "project" ? "Project variables" : "Environment variables";
}

export function canWriteVariableScope(
  capabilities: Capability[],
  humanSession: boolean,
  snapshot: VariableSetSnapshot,
  environment: Environment,
  projectTeamId?: string,
) {
  if (
    !humanSession ||
    snapshot.environmentId !== environment.id ||
    snapshot.projectId !== environment.projectId
  )
    return false;
  return capabilities.some((capability) => {
    if (!capability.actions?.includes("deployment-config:write")) return false;
    switch (capability.scopeType) {
      case "platform":
        return capability.scopeId === "platform";
      case "team":
        return Boolean(projectTeamId) && capability.scopeId === projectTeamId;
      case "project":
        return capability.scopeId === snapshot.projectId;
      case "environment":
        return (
          snapshot.scope === "environment" &&
          capability.scopeId === snapshot.environmentId
        );
      case "namespace":
        return (
          snapshot.scope === "environment" &&
          capability.scopeId === environment.namespace
        );
      default:
        return false;
    }
  });
}

function VariableSetEditor({
  environment,
  snapshot,
  canWrite,
}: {
  environment: Environment;
  snapshot: VariableSetSnapshot;
  canWrite: boolean;
}) {
  const queryClient = useQueryClient();
  const initialRaw = snapshot.present
    ? (snapshot.rawYaml ?? "")
    : emptyVariableSet;
  const [rawYaml, setRawYaml] = useState(initialRaw);
  const [preview, setPreview] = useState<VariableSetPreview | null>(null);
  const [previewRaw, setPreviewRaw] = useState("");
  const [saveKey, setSaveKey] = useState("");
  const [operation, setOperation] = useState<Operation | null>(null);
  const previewMutation = useMutation({
    mutationFn: () =>
      api.previewVariableSet(
        environment.id,
        snapshot.scope,
        rawYaml,
        snapshot.present ? snapshot.etag : undefined,
        snapshot.path,
      ),
    onSuccess: (value) => {
      setPreview(value);
      setPreviewRaw(rawYaml);
      setSaveKey(crypto.randomUUID());
      setOperation(null);
    },
  });
  const saveMutation = useMutation({
    mutationFn: () => {
      if (!preview || previewRaw !== rawYaml || !saveKey)
        throw new Error("Preview this exact YAML before saving.");
      return api.saveVariableSet(
        environment.id,
        snapshot.scope,
        rawYaml,
        preview.previewToken,
        saveKey,
      );
    },
    onSuccess: async (value) => {
      setOperation(value);
      await queryClient.invalidateQueries({
        queryKey: ["variable-sets", environment.id],
      });
    },
  });
  const exactPreview = preview !== null && previewRaw === rawYaml;
  const publication =
    environment.protectionPolicy === "development"
      ? "Direct commit"
      : "Protected pull request";

  return (
    <Card className="variable-set-editor">
      <div className="card__header">
        <div>
          <span className="eyebrow">{snapshot.scope} scope</span>
          <h2>{scopeLabel(snapshot.scope)}</h2>
          <p>
            {snapshot.scope === "project"
              ? "Inherited by applications through this concrete environment Git authority."
              : "Overrides project values for this environment."}
          </p>
        </div>
        <StatusPill value={snapshot.present ? "active" : "absent"} />
      </div>
      <dl className="detail-list detail-list--compact">
        <div>
          <dt>Exact Git path</dt>
          <dd>
            <code>{snapshot.path}</code>
          </dd>
        </div>
        <div>
          <dt>Indexed revision</dt>
          <dd>{shortId(snapshot.indexedRevision, 12)}</dd>
        </div>
        <div>
          <dt>Publication</dt>
          <dd>{publication} · fixed by environment policy</dd>
        </div>
      </dl>
      {snapshot.scope === "project" ? (
        <div className="notice" role="note">
          <div>
            <strong>Shared project scope</strong>
            <p>
              A project VariableSet change may affect every environment sharing
              this project source in the same Git authority.
            </p>
          </div>
        </div>
      ) : null}
      <label className="field">
        <span className="field__label">{scopeLabel(snapshot.scope)} YAML</span>
        <textarea
          className="variable-set-yaml"
          aria-label={`${scopeLabel(snapshot.scope)} YAML`}
          spellCheck={false}
          value={rawYaml}
          readOnly={!canWrite}
          onChange={(event) => {
            setRawYaml(event.target.value);
            setPreview(null);
            setPreviewRaw("");
            setSaveKey("");
            setOperation(null);
            previewMutation.reset();
            saveMutation.reset();
          }}
        />
      </label>
      {canWrite ? (
        <div className="editor-actions">
          <Button
            variant="secondary"
            busy={previewMutation.isPending}
            onClick={() => previewMutation.mutate()}
          >
            <Icon name="code" /> Preview Git diff
          </Button>
          <Button
            busy={saveMutation.isPending}
            disabled={!exactPreview || operation !== null}
            onClick={() => saveMutation.mutate()}
          >
            <Icon name="git" /> Save through Git
          </Button>
        </div>
      ) : (
        <p className="muted-copy">
          Read-only: an exact scoped deployment configuration write capability
          and interactive session are required to preview or save this source.
        </p>
      )}
      {previewMutation.error ? (
        <ErrorPanel error={previewMutation.error} title="Preview failed" />
      ) : null}
      {saveMutation.error ? (
        <ErrorPanel error={saveMutation.error} title="Save failed" />
      ) : null}
      {exactPreview ? (
        <div className="variable-set-preview" aria-label="Git diff preview">
          <div className="eyebrow">Exact Git diff</div>
          <pre>{preview.gitDiff || "No textual changes."}</pre>
        </div>
      ) : null}
      {operation ? (
        <div className="notice notice--success" role="status">
          <div>
            <strong>Git operation accepted</strong>
            <p>
              {publication} operation {shortId(operation.id, 12)} is queued.
            </p>
          </div>
          <Link
            to="/operations/$operationId"
            params={{ operationId: operation.id }}
            className="button button--secondary"
          >
            Track operation
          </Link>
        </div>
      ) : null}
    </Card>
  );
}

export function VariableSetsView({ environmentId }: { environmentId: string }) {
  const environment = useQuery({
    queryKey: ["environment", environmentId],
    queryFn: () => api.environment(environmentId),
  });
  const sources = useQuery({
    queryKey: ["variable-sets", environmentId],
    queryFn: () => api.variableSets(environmentId),
  });
  const me = useQuery({ queryKey: ["me"], queryFn: api.me });
  const capabilities = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
  });
  const needsProjectContext = (capabilities.data?.capabilities ?? []).some(
    (capability) =>
      capability.scopeType === "team" &&
      capability.actions?.includes("deployment-config:write"),
  );
  const projects = useQuery({
    queryKey: ["projects"],
    queryFn: api.projects,
    enabled: needsProjectContext,
  });
  const project = projects.data?.items.find(
    (value) => value.id === environment.data?.projectId,
  );
  const loadError =
    environment.error ??
    sources.error ??
    me.error ??
    capabilities.error ??
    (needsProjectContext ? projects.error : null);

  return (
    <div className="page">
      <PageHeader
        eyebrow="Git-backed configuration"
        title={
          environment.data
            ? `${environment.data.name} variables`
            : "Project & environment variables"
        }
        description="Manage ordinary values in the exact inherited Git sources. Application values remain highest precedence, and secret bindings stay application-scoped."
        actions={
          <Link to="/projects" className="button button--secondary">
            <Icon name="chevron" /> Projects
          </Link>
        }
      />
      {loadError ? (
        <ErrorPanel
          error={loadError}
          onRetry={() =>
            void Promise.all([
              environment.refetch(),
              sources.refetch(),
              me.refetch(),
              capabilities.refetch(),
              ...(needsProjectContext ? [projects.refetch()] : []),
            ])
          }
        />
      ) : null}
      {environment.isPending ||
      sources.isPending ||
      me.isPending ||
      capabilities.isPending ||
      (needsProjectContext && projects.isPending) ? (
        <Card>
          <Skeleton lines={8} />
        </Card>
      ) : environment.data && sources.data?.items.length === 2 ? (
        <div className="variable-set-grid">
          {sources.data.items.map((snapshot) => (
            <VariableSetEditor
              key={`${snapshot.scope}:${snapshot.indexedRevision}:${snapshot.etag ?? "absent"}`}
              environment={environment.data}
              snapshot={snapshot}
              canWrite={canWriteVariableScope(
                capabilities.data?.capabilities ?? [],
                me.data?.authentication.kind === "session",
                snapshot,
                environment.data,
                project?.teamId,
              )}
            />
          ))}
        </div>
      ) : !loadError ? (
        <ErrorPanel
          error={
            new Error(
              "The exact two-scope VariableSet snapshot is unavailable.",
            )
          }
          onRetry={() => void sources.refetch()}
        />
      ) : null}
    </div>
  );
}

export function VariableSetsPage() {
  const { environmentId } = useParams({ strict: false });
  return <VariableSetsView environmentId={environmentId ?? ""} />;
}
