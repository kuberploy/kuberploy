import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api/client";
import type { Application, Project } from "../api/types";
import { Button, Card, EmptyState, Field, StatusPill } from "./ui";

export function RegistryPullCredentialsPanel({
  application,
  project,
  enabled,
  canManage,
}: {
  application: Application;
  project: Project;
  enabled: boolean;
  canManage: boolean;
}) {
  const client = useQueryClient();
  const [name, setName] = useState("");
  const [targetId, setTargetId] = useState("");
  const [selection, setSelection] = useState("");
  const catalog = useQuery({
    queryKey: ["project-registry-pull-credentials", project.id],
    queryFn: () => api.projectRegistryPullCredentials(project.id),
    enabled,
    retry: false,
  });
  const current = useQuery({
    queryKey: ["application-registry-pull-selection", application.id],
    queryFn: () => api.applicationRegistryPullSelection(application.id),
    enabled,
    retry: false,
  });
  const selectedValue =
    selection ||
    (current.data?.type === "project-credential"
      ? current.data.projectCredentialId
      : "public");
  const refresh = async () => {
    await Promise.all([
      client.invalidateQueries({
        queryKey: ["project-registry-pull-credentials", project.id],
      }),
      client.invalidateQueries({
        queryKey: ["application-registry-pull-selection", application.id],
      }),
    ]);
  };
  const create = useMutation({
    mutationFn: () =>
      api.createProjectRegistryPullCredential(
        project.id,
        { name: name.trim(), registryTargetId: targetId },
        crypto.randomUUID(),
      ),
    onSuccess: async () => {
      setName("");
      setTargetId("");
      await refresh();
    },
  });
  const save = useMutation({
    mutationFn: (value: string) =>
      api.putApplicationRegistryPullSelection(
        application.id,
        value === "public"
          ? { type: "public" }
          : { type: "project-credential", projectCredentialId: value },
        crypto.randomUUID(),
      ),
    onSuccess: refresh,
  });
  const remove = useMutation({
    mutationFn: (credentialId: string) =>
      api.deleteProjectRegistryPullCredential(project.id, credentialId),
    onSuccess: refresh,
  });

  if (!enabled) return null;
  if (catalog.error || current.error) {
    return (
      <Card>
        <EmptyState
          title="Image pull credentials are unavailable"
          description="The project credential catalog could not be loaded. No credential selection was changed."
        />
      </Card>
    );
  }

  return (
    <Card>
      <div className="form-card__heading">
        <span>02</span>
        <div>
          <h2>Image pull credentials</h2>
          <p>
            Choose what Kubernetes uses to pull this service image. Builder push
            and cache credentials are configured separately.
          </p>
        </div>
      </div>
      <div className="form-grid form-grid--two">
        <Field
          label="Pull strategy"
          hint="Public sends no credential. A project credential is resolved to a locked runtime Secret by the server."
        >
          <select
            aria-label="Pull strategy"
            value={selectedValue}
            disabled={!canManage || current.isPending || save.isPending}
            onChange={(event) => {
              const value = event.target.value;
              setSelection(value);
              save.mutate(value);
            }}
          >
            <option value="public">Public registry / no credential</option>
            {(catalog.data?.items ?? []).map((credential) => (
              <option key={credential.id} value={credential.id}>
                {credential.name} — {credential.registryServer}
              </option>
            ))}
          </select>
        </Field>
        <div>
          <StatusPill
            value={selectedValue === "public" ? "public" : "active"}
            label={
              selectedValue === "public" ? "Public pull" : "Project credential"
            }
          />
          <p className="field__hint">
            The selected credential affects runtime image pulls only. It does
            not enable or disable GitHub builds, webhooks, or auto-deploy.
          </p>
        </div>
      </div>

      {canManage ? (
        <div className="form-grid form-grid--three">
          <Field label="Credential name">
            <input
              aria-label="Credential name"
              value={name}
              maxLength={64}
              onChange={(event) => setName(event.target.value)}
              placeholder="Production registry"
            />
          </Field>
          <Field label="Registry">
            <select
              aria-label="Registry"
              value={targetId}
              onChange={(event) => setTargetId(event.target.value)}
            >
              <option value="">Select registry</option>
              {(catalog.data?.availableTargets ?? []).map((target) => (
                <option key={target.id} value={target.id}>
                  {target.name} — {target.server}/{target.repositoryPrefix}
                </option>
              ))}
            </select>
          </Field>
          <div className="field field--actions">
            <Button
              type="button"
              variant="secondary"
              disabled={!name.trim() || !targetId || create.isPending}
              onClick={() => create.mutate()}
            >
              Add project credential
            </Button>
          </div>
        </div>
      ) : null}

      {(catalog.data?.items ?? []).length > 0 ? (
        <div className="compact-list">
          {(catalog.data?.items ?? []).map((credential) => (
            <div className="compact-list__item" key={credential.id}>
              <div>
                <strong>{credential.name}</strong>
                <p>
                  {credential.registryServer}/{credential.repositoryPrefix}
                </p>
              </div>
              {canManage ? (
                <Button
                  type="button"
                  variant="ghost"
                  disabled={selectedValue === credential.id || remove.isPending}
                  onClick={() => remove.mutate(credential.id)}
                >
                  Remove
                </Button>
              ) : null}
            </div>
          ))}
        </div>
      ) : null}
    </Card>
  );
}
