import { useEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type {
  BuilderContainerResources,
  BuilderPlatformSettingsInput,
} from "../api/types";
import { Button, Card, ErrorPanel, Field, Skeleton, StatusPill } from "../components/ui";
import { formatDate } from "../lib/format";

const quantityPattern = /^[1-9][0-9]*(?:m|Ki|Mi|Gi|Ti)?$/;

const emptyResources: BuilderContainerResources = {
  cpuRequest: "",
  memoryRequest: "",
  ephemeralStorageRequest: "",
  cpuLimit: "",
  memoryLimit: "",
  ephemeralStorageLimit: "",
};

const emptyDraft: BuilderPlatformSettingsInput = {
  nodeIsolation: false,
  maxConcurrentBuilders: 1,
  checkoutResources: { ...emptyResources },
  dindResources: { ...emptyResources },
  agentResources: { ...emptyResources },
};

const resourceFields: Array<{ key: keyof BuilderContainerResources; label: string; placeholder: string }> = [
  { key: "cpuRequest", label: "CPU request", placeholder: "500m" },
  { key: "memoryRequest", label: "Memory request", placeholder: "1Gi" },
  { key: "ephemeralStorageRequest", label: "Storage request", placeholder: "10Gi" },
  { key: "cpuLimit", label: "CPU limit", placeholder: "4" },
  { key: "memoryLimit", label: "Memory limit", placeholder: "8Gi" },
  { key: "ephemeralStorageLimit", label: "Storage limit", placeholder: "50Gi" },
];

function ResourceEditor({
  title,
  description,
  value,
  onChange,
}: {
  title: string;
  description: string;
  value: BuilderContainerResources;
  onChange: (value: BuilderContainerResources) => void;
}) {
  return (
    <section className="service-settings-section">
      <div className="service-settings-section__header">
        <div>
          <h3>{title}</h3>
          <p>{description}</p>
        </div>
      </div>
      <div className="form-grid">
        {resourceFields.map((field) => (
          <Field label={field.label} required key={field.key}>
            <input
              value={value[field.key]}
              required
              pattern={quantityPattern.source}
              placeholder={field.placeholder}
              onChange={(event) => onChange({ ...value, [field.key]: event.target.value.trim() })}
            />
          </Field>
        ))}
      </div>
    </section>
  );
}

export function BuilderSettingsPage() {
  const queryClient = useQueryClient();
  const settings = useQuery({
    queryKey: ["builder-platform-settings"],
    queryFn: api.builderPlatformSettings,
    retry: false,
  });
  const [draft, setDraft] = useState<BuilderPlatformSettingsInput>(emptyDraft);
  const saveKey = useRef(crypto.randomUUID());

  useEffect(() => {
    if (!settings.data) return;
    const { nodeIsolation, maxConcurrentBuilders, checkoutResources, dindResources, agentResources } = settings.data;
    setDraft({ nodeIsolation, maxConcurrentBuilders, checkoutResources, dindResources, agentResources });
  }, [settings.data]);

  const save = useMutation({
    mutationFn: () =>
      api.updateBuilderPlatformSettings(settings.data?.revision ?? 0, draft, saveKey.current),
    onSuccess: async () => {
      saveKey.current = crypto.randomUUID();
      await queryClient.invalidateQueries({ queryKey: ["builder-platform-settings"] });
    },
  });

  const change = (next: BuilderPlatformSettingsInput) => {
    setDraft(next);
    saveKey.current = crypto.randomUUID();
    save.reset();
  };

  const submit = (event: FormEvent) => {
    event.preventDefault();
    save.mutate();
  };

  if (settings.isPending) return <Skeleton lines={10} />;
  if (settings.error) return <ErrorPanel error={settings.error} onRetry={() => settings.refetch()} />;

  return (
    <div className="page page--narrow settings-page">
      <div className="page-header page-heading">
        <div>
          <span className="eyebrow">Platform settings</span>
          <h1>Source builders</h1>
          <p>Control privileged DinD scheduling, queue concurrency, and per-container Kubernetes resources.</p>
        </div>
        <StatusPill value="recorded" label={`Revision ${settings.data?.revision ?? 0}`} />
      </div>

      <form onSubmit={submit}>
        <Card>
          <div className="card__header card__header--inside">
            <div>
              <h2>Queue and node placement</h2>
              <p>Default mode works on one schedulable node. Dedicated isolation requires node label and taint shown below.</p>
            </div>
          </div>
          <div className="form-grid">
            <Field label="Maximum concurrent builders" hint="Queued builds wait after this many preparing, running, or cancelling Jobs.">
              <input
                type="number"
                min={1}
                max={20}
                required
                value={draft.maxConcurrentBuilders}
                onChange={(event) => change({ ...draft, maxConcurrentBuilders: Number(event.target.value) })}
              />
            </Field>
            <Field label="Dedicated builder nodes" hint="Requires kuberploy.io/node-class=dind-builder and kuberploy.io/dind-builder=true:NoSchedule.">
              <label>
                <input
                  type="checkbox"
                  checked={draft.nodeIsolation}
                  onChange={(event) => change({ ...draft, nodeIsolation: event.target.checked })}
                />
                <span>Enable node selector and toleration</span>
              </label>
            </Field>
          </div>
        </Card>

        <Card>
          <div className="card__header card__header--inside">
            <div>
              <h2>Per-builder resources</h2>
              <p>Every build Job contains checkout, privileged Docker daemon, and build agent containers.</p>
            </div>
          </div>
          <ResourceEditor title="Checkout" description="Clones source and prepares the workspace." value={draft.checkoutResources} onChange={(value) => change({ ...draft, checkoutResources: value })} />
          <ResourceEditor title="Docker daemon" description="Privileged DinD sidecar running BuildKit-backed Docker builds." value={draft.dindResources} onChange={(value) => change({ ...draft, dindResources: value })} />
          <ResourceEditor title="Build agent" description="Runs build, cache, registry push, and result publication." value={draft.agentResources} onChange={(value) => change({ ...draft, agentResources: value })} />
          {save.error ? <ErrorPanel error={save.error} /> : null}
          <div className="form-actions">
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? "Saving…" : "Save builder settings"}
            </Button>
            {settings.data?.updatedAt ? <span className="muted">Last changed {formatDate(settings.data.updatedAt)}</span> : null}
          </div>
        </Card>
      </form>
    </div>
  );
}
