import { useEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type {
  BuilderContainerResources,
  BuilderPlatformSettingsInput,
} from "../api/types";
import {
  Button,
  Card,
  CardHeader,
  ErrorPanel,
  Eyebrow,
  Field,
  FormActions,
  FormGrid,
  Page,
  PageHeader,
  Skeleton,
  StatusPill,
} from "../components/ui";
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

const resourceFields: Array<{
  key: keyof BuilderContainerResources;
  label: string;
  placeholder: string;
}> = [
  { key: "cpuRequest", label: "CPU request", placeholder: "500m" },
  { key: "memoryRequest", label: "Memory request", placeholder: "1Gi" },
  {
    key: "ephemeralStorageRequest",
    label: "Storage request",
    placeholder: "10Gi",
  },
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
    <section className="grid gap-5 p-7 [&_+_.service-settings-section]:border-t [&_+_.service-settings-section]:border-t-line [&>.field]:max-w-[calc(50%_-_7px)] to-760:[&>.field]:max-w-[none]">
      <div className="[&_h2]:mt-1 [&_h2]:mx-0 [&_h2]:mb-0 [&_h2]:text-lg [&_h2]:font-semibold [&_h2]:tracking-[-0.02em] [&_p]:mt-1.5 [&_p]:mx-0 [&_p]:mb-0 [&_p]:text-ink-soft [&_p]:text-meta [&_p]:leading-[1.5] flex items-start justify-between gap-5">
        <div>
          <h3>{title}</h3>
          <p>{description}</p>
        </div>
      </div>
      <FormGrid>
        {resourceFields.map((field) => (
          <Field label={field.label} required key={field.key}>
            <input
              value={value[field.key]}
              required
              pattern={quantityPattern.source}
              placeholder={field.placeholder}
              onChange={(event) =>
                onChange({ ...value, [field.key]: event.target.value.trim() })
              }
            />
          </Field>
        ))}
      </FormGrid>
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
    const {
      nodeIsolation,
      maxConcurrentBuilders,
      checkoutResources,
      dindResources,
      agentResources,
    } = settings.data;
    setDraft({
      nodeIsolation,
      maxConcurrentBuilders,
      checkoutResources,
      dindResources,
      agentResources,
    });
  }, [settings.data]);

  const save = useMutation({
    mutationFn: () =>
      api.updateBuilderPlatformSettings(
        settings.data?.revision ?? 0,
        draft,
        saveKey.current,
      ),
    onSuccess: async () => {
      saveKey.current = crypto.randomUUID();
      await queryClient.invalidateQueries({
        queryKey: ["builder-platform-settings"],
      });
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

  if (settings.isPending)
    return (
      <Page narrow className="[&>header]:mb-0">
        <Skeleton lines={10} />
      </Page>
    );
  if (settings.error)
    return (
      <ErrorPanel error={settings.error} onRetry={() => settings.refetch()} />
    );

  return (
    <Page narrow className="[&>header]:mb-0">
      <PageHeader
        eyebrow="Platform settings"
        title="Source builders"
        description="Control privileged DinD scheduling, queue concurrency, and per-container Kubernetes resources."
        actions={
          <StatusPill
            value="recorded"
            label={`Revision ${settings.data?.revision ?? 0}`}
          />
        }
      />

      <form onSubmit={submit}>
        <Card>
          <CardHeader>
            <div>
              <h2>Queue and node placement</h2>
              <p>
                Default mode works on one schedulable node. Dedicated isolation
                requires node label and taint shown below.
              </p>
            </div>
          </CardHeader>
          <FormGrid>
            <Field
              label="Maximum concurrent builders"
              hint="Queued builds wait after this many preparing, running, or cancelling Jobs."
            >
              <input
                type="number"
                min={1}
                max={20}
                required
                value={draft.maxConcurrentBuilders}
                onChange={(event) =>
                  change({
                    ...draft,
                    maxConcurrentBuilders: Number(event.target.value),
                  })
                }
              />
            </Field>
            <Field
              label="Dedicated builder nodes"
              hint="Requires kuberploy.io/node-class=dind-builder and kuberploy.io/dind-builder=true:NoSchedule."
            >
              <label>
                <input
                  type="checkbox"
                  checked={draft.nodeIsolation}
                  onChange={(event) =>
                    change({ ...draft, nodeIsolation: event.target.checked })
                  }
                />
                <span>Enable node selector and toleration</span>
              </label>
            </Field>
          </FormGrid>
        </Card>

        <Card>
          <CardHeader>
            <div>
              <h2>Per-builder resources</h2>
              <p>
                Every build Job contains checkout, privileged Docker daemon, and
                build agent containers.
              </p>
            </div>
          </CardHeader>
          <ResourceEditor
            title="Checkout"
            description="Clones source and prepares the workspace."
            value={draft.checkoutResources}
            onChange={(value) => change({ ...draft, checkoutResources: value })}
          />
          <ResourceEditor
            title="Docker daemon"
            description="Privileged DinD sidecar running BuildKit-backed Docker builds."
            value={draft.dindResources}
            onChange={(value) => change({ ...draft, dindResources: value })}
          />
          <ResourceEditor
            title="Build agent"
            description="Runs build, cache, registry push, and result publication."
            value={draft.agentResources}
            onChange={(value) => change({ ...draft, agentResources: value })}
          />
          {save.error ? <ErrorPanel error={save.error} /> : null}
          <FormActions>
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? "Saving…" : "Save builder settings"}
            </Button>
            {settings.data?.updatedAt ? (
              <span className="">
                Last changed {formatDate(settings.data.updatedAt)}
              </span>
            ) : null}
          </FormActions>
        </Card>
      </form>
    </Page>
  );
}
