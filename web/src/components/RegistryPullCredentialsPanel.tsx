import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import type { Application, Project } from "../api/types";
import { Button, Card, EmptyState, ErrorPanel, Field, StatusPill } from "./ui";

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
  const scopeKey = `${project.id}:${application.id}`;
  const scopeRef = useRef(scopeKey);
  const createAttempt = useRef<{ signature: string; key: string } | null>(null);
  const removeAttempt = useRef<{ signature: string; key: string } | null>(null);
  const selectionAttempt = useRef<{
    signature: string;
    key: string;
  } | null>(null);
  scopeRef.current = scopeKey;
  useEffect(() => {
    setName("");
    setTargetId("");
    setSelection("");
    createAttempt.current = null;
    removeAttempt.current = null;
    selectionAttempt.current = null;
  }, [scopeKey]);
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
  const selectedCredentialUnavailable = Boolean(
    selectedValue &&
    selectedValue !== "public" &&
    catalog.data &&
    !catalog.data.items.some((credential) => credential.id === selectedValue),
  );
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
    mutationFn: ({
      input,
      projectId,
      idempotencyKey,
    }: {
      input: { name: string; registryTargetId: string };
      projectId: string;
      idempotencyKey: string;
      scopeKey: string;
    }) =>
      api.createProjectRegistryPullCredential(projectId, input, idempotencyKey),
    onSuccess: async (_value, input) => {
      if (scopeRef.current !== input.scopeKey) return;
      const sameDraft =
        name.trim() === input.input.name &&
        targetId === input.input.registryTargetId;
      if (sameDraft) {
        if (createAttempt.current?.key === input.idempotencyKey) {
          createAttempt.current = null;
        }
        setName("");
        setTargetId("");
      }
      await refresh();
    },
  });
  const save = useMutation({
    mutationFn: ({
      value,
      applicationId,
      idempotencyKey,
    }: {
      value: string;
      applicationId: string;
      idempotencyKey: string;
      scopeKey: string;
    }) =>
      api.putApplicationRegistryPullSelection(
        applicationId,
        value === "public"
          ? { type: "public" }
          : { type: "project-credential", projectCredentialId: value },
        idempotencyKey,
      ),
    onSuccess: (_value, input) => {
      if (scopeRef.current !== input.scopeKey) return;
      if (selectionAttempt.current?.key === input.idempotencyKey) {
        selectionAttempt.current = null;
      }
      return refresh();
    },
    onError: (_error, input) => {
      if (scopeRef.current !== input.scopeKey) return;
      setSelection(
        current.data?.type === "project-credential"
          ? (current.data.projectCredentialId ?? "public")
          : "public",
      );
    },
  });
  const remove = useMutation({
    mutationFn: (input: {
      projectId: string;
      credentialId: string;
      idempotencyKey: string;
      scopeKey: string;
    }) =>
      api.deleteProjectRegistryPullCredential(
        input.projectId,
        input.credentialId,
        input.idempotencyKey,
      ),
    onSuccess: (_value, input) => {
      if (scopeRef.current !== input.scopeKey) return undefined;
      if (removeAttempt.current?.key === input.idempotencyKey) {
        removeAttempt.current = null;
      }
      return refresh();
    },
  });

  useEffect(() => {
    create.reset();
    save.reset();
    remove.reset();
  }, [scopeKey]);

  const createCredential = () => {
    const input = { name: name.trim(), registryTargetId: targetId };
    const signature = JSON.stringify(input);
    const idempotencyKey =
      createAttempt.current?.signature === signature
        ? createAttempt.current.key
        : crypto.randomUUID();
    createAttempt.current = { signature, key: idempotencyKey };
    create.mutate({ input, projectId: project.id, idempotencyKey, scopeKey });
  };

  const saveSelection = (value: string) => {
    const signature = JSON.stringify({ applicationId: application.id, value });
    const idempotencyKey =
      selectionAttempt.current?.signature === signature
        ? selectionAttempt.current.key
        : crypto.randomUUID();
    selectionAttempt.current = { signature, key: idempotencyKey };
    save.mutate({
      value,
      applicationId: application.id,
      idempotencyKey,
      scopeKey,
    });
  };

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
        <span aria-hidden="true">↧</span>
        <div>
          <h2>Image pull credentials</h2>
          <p>
            Choose what Kubernetes uses to pull this service image. Builder push
            and cache credentials are configured separately.
          </p>
        </div>
      </div>
      {create.error ? (
        <ErrorPanel error={create.error} title="Credential was not created" />
      ) : null}
      {save.error ? (
        <ErrorPanel error={save.error} title="Pull strategy was not saved" />
      ) : null}
      {remove.error ? (
        <ErrorPanel error={remove.error} title="Credential was not removed" />
      ) : null}
      <div className="form-grid form-grid--two">
        <Field
          label="Pull strategy"
          hint="Public sends no credential. A project credential is resolved to a locked runtime Secret by the server."
        >
          <select
            aria-label="Pull strategy"
            value={selectedValue}
            disabled={
              !canManage ||
              current.isPending ||
              catalog.isPending ||
              save.isPending
            }
            onChange={(event) => {
              const value = event.target.value;
              setSelection(value);
              saveSelection(value);
            }}
          >
            <option value="public">Public registry / no credential</option>
            {selectedCredentialUnavailable ? (
              <option value={selectedValue}>
                Current project credential unavailable — choose another
              </option>
            ) : null}
            {(catalog.data?.items ?? []).map((credential) => (
              <option key={credential.id} value={credential.id}>
                {credential.name} — {credential.registryServer}
              </option>
            ))}
          </select>
        </Field>
        <div>
          <StatusPill
            value={
              selectedValue === "public"
                ? "public"
                : selectedCredentialUnavailable
                  ? "unavailable"
                  : "active"
            }
            label={
              selectedValue === "public"
                ? "Public pull"
                : selectedCredentialUnavailable
                  ? "Credential unavailable"
                  : "Project credential"
            }
          />
          <p className="field__hint">
            {selectedCredentialUnavailable
              ? "The current project credential is no longer in your authorized catalog. Choose Public or another available credential before deploying."
              : "The selected credential affects runtime image pulls only. It does not enable or disable GitHub builds, webhooks, or auto-deploy."}
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
              onClick={createCredential}
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
                  onClick={() => {
                    if (
                      window.confirm(
                        `Remove project pull credential ${credential.name}? Services will no longer be able to select it.`,
                      )
                    ) {
                      const signature = JSON.stringify({
                        projectId: project.id,
                        credentialId: credential.id,
                      });
                      const idempotencyKey =
                        removeAttempt.current?.signature === signature
                          ? removeAttempt.current.key
                          : crypto.randomUUID();
                      removeAttempt.current = {
                        signature,
                        key: idempotencyKey,
                      };
                      remove.mutate({
                        projectId: project.id,
                        credentialId: credential.id,
                        idempotencyKey,
                        scopeKey,
                      });
                    }
                  }}
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
