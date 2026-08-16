import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { ApiError, api } from "../api/client";
import type {
  Application,
  BuildArgument,
  Capability,
  CreateBuildDefinition,
  Project,
  RegistryTarget,
} from "../api/types";
import { hasBuildApplicationCapability } from "../lib/buildAccess";
import { canonicalBranchRef, gitRefLabel } from "../lib/format";
import { Icon } from "./Icon";
import { Button, ErrorPanel, Field } from "./ui";

type DefinitionForm = {
  installationId: string;
  repositoryId: string;
  registryTargetId: string;
  refType: "branch" | "tag";
  triggerRef: string;
  contextPath: string;
  dockerfilePath: string;
  amd64: boolean;
  arm64: boolean;
  buildArgs: string;
  cacheTrustLane: string;
  cacheImports: number;
  profileResource: string;
  timeoutSeconds: number;
  profileEgress: string;
  maxAttempts: number;
  secretProfileIds: string[];
  sshProfileIds: string[];
};

type StableAttempt = { signature: string; key: string };

const defaultDefinitionValues: DefinitionForm = {
  installationId: "",
  repositoryId: "",
  registryTargetId: "",
  refType: "branch",
  triggerRef: "main",
  contextPath: ".",
  dockerfilePath: "Dockerfile",
  amd64: true,
  arm64: false,
  buildArgs: "",
  cacheTrustLane: "protected",
  cacheImports: 2,
  profileResource: "standard",
  timeoutSeconds: 900,
  profileEgress: "registry-and-source",
  maxAttempts: 3,
  secretProfileIds: [],
  sshProfileIds: [],
};

const namePattern = /^[a-z][a-z0-9_.-]{0,62}$/;
const buildArgPattern = /^[A-Z_][A-Z0-9_]{0,127}$/;
function parseBuildArgs(raw: string): BuildArgument[] {
  const lines = raw
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
  if (lines.length > 64) throw new Error("Use at most 64 build arguments.");
  const seen = new Set<string>();
  const args = lines.map((line, index) => {
    const separator = line.indexOf("=");
    if (separator < 1) {
      throw new Error(`Build argument line ${index + 1} must use NAME=value.`);
    }
    const name = line.slice(0, separator).trim();
    const value = line.slice(separator + 1);
    if (!buildArgPattern.test(name)) {
      throw new Error(
        `Build argument ${name || `line ${index + 1}`} has an invalid name.`,
      );
    }
    if (
      value.length > 4096 ||
      /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(value)
    ) {
      throw new Error(`Build argument ${name} has an invalid value.`);
    }
    if (seen.has(name)) throw new Error(`Build argument ${name} is repeated.`);
    seen.add(name);
    return { name, value };
  });
  return args.sort((left, right) => left.name.localeCompare(right.name));
}

function retryNetworkOnce(failureCount: number, error: unknown) {
  return error instanceof ApiError && error.status === 0 && failureCount < 1;
}

export function BuildDefinitionForm({
  application,
  project,
  capabilities,
  humanSession,
  registryTargets,
}: {
  application: Application;
  project: Project;
  capabilities: Capability[];
  humanSession: boolean;
  registryTargets: RegistryTarget[];
}) {
  const queryClient = useQueryClient();
  const stableAttempt = useRef<StableAttempt | null>(null);
  const scopeRef = useRef(application.id);
  scopeRef.current = application.id;
  const resetFormRef = useRef<() => void>(() => undefined);
  const resetCreateRef = useRef<() => void>(() => undefined);
  const [parseError, setParseError] = useState<unknown>();
  const [createdDefinitionId, setCreatedDefinitionId] = useState("");
  const canCreate =
    humanSession &&
    hasBuildApplicationCapability(
      capabilities,
      "build-definitions:write",
      application,
      project,
    );
  const installations = useQuery({
    queryKey: ["github-installations"],
    queryFn: api.githubInstallations,
    enabled: canCreate,
    retry: false,
  });
  const secretProfiles = useQuery({
    queryKey: ["build-secret-profiles", application.id],
    queryFn: () => api.buildSecretProfiles(application.id),
    enabled: canCreate,
    retry: false,
  });
  const form = useForm<DefinitionForm>({
    defaultValues: defaultDefinitionValues,
  });
  useEffect(() => {
    if (!secretProfiles.isSuccess || !secretProfiles.data) return;
    const buildIDs = new Set(
      secretProfiles.data.build.map((profile) => profile.id),
    );
    const sshIDs = new Set(
      secretProfiles.data.ssh.map((profile) => profile.id),
    );
    const current = form.getValues();
    const nextBuildIDs = current.secretProfileIds.filter((id) =>
      buildIDs.has(id),
    );
    const nextSSHIDs = current.sshProfileIds.filter((id) => sshIDs.has(id));
    if (
      JSON.stringify(nextBuildIDs) !== JSON.stringify(current.secretProfileIds)
    ) {
      form.setValue("secretProfileIds", nextBuildIDs, { shouldDirty: true });
    }
    if (JSON.stringify(nextSSHIDs) !== JSON.stringify(current.sshProfileIds)) {
      form.setValue("sshProfileIds", nextSSHIDs, { shouldDirty: true });
    }
  }, [form, secretProfiles.data, secretProfiles.isSuccess]);
  const installationId = form.watch("installationId");
  const repositories = useQuery({
    queryKey: ["github-installation-repositories", installationId],
    queryFn: () => api.githubInstallationRepositories(installationId),
    enabled: canCreate && Boolean(installationId),
    retry: false,
  });
  const activeRepositories = useMemo(
    () =>
      (repositories.data?.items ?? []).filter(
        (repository) => repository.lifecycle === "active",
      ),
    [repositories.data],
  );
  const create = useMutation({
    mutationFn: ({
      input,
      key,
      draftSignature,
      applicationId,
    }: {
      input: CreateBuildDefinition;
      key: string;
      draftSignature: string;
      applicationId: string;
    }) => api.createBuildDefinition(applicationId, input, key),
    retry: retryNetworkOnce,
    onSuccess: async (definition, input) => {
      if (scopeRef.current !== input.applicationId) return;
      const sameDraft =
        JSON.stringify(form.getValues()) === input.draftSignature;
      if (sameDraft) {
        if (stableAttempt.current?.key === input.key) {
          stableAttempt.current = null;
        }
        setParseError(undefined);
        setCreatedDefinitionId(definition.id);
      }
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ["build-definitions", input.applicationId],
        }),
        queryClient.invalidateQueries({
          queryKey: ["build-attempts", input.applicationId],
        }),
      ]);
    },
  });
  resetFormRef.current = () => form.reset(defaultDefinitionValues);
  resetCreateRef.current = create.reset;

  useEffect(() => {
    scopeRef.current = application.id;
    stableAttempt.current = null;
    resetFormRef.current();
    setParseError(undefined);
    setCreatedDefinitionId("");
    resetCreateRef.current();
  }, [application.id]);

  const submit = (value: DefinitionForm) => {
    setParseError(undefined);
    setCreatedDefinitionId("");
    try {
      const platforms: CreateBuildDefinition["platforms"] = [];
      if (value.amd64) platforms.push("linux/amd64");
      if (value.arm64) platforms.push("linux/arm64");
      if (!platforms.length) throw new Error("Select at least one platform.");
      const enteredRef = gitRefLabel(value.triggerRef.trim());
      const triggerRef =
        value.refType === "tag"
          ? `refs/tags/${enteredRef}`
          : canonicalBranchRef(enteredRef);
      const input: CreateBuildDefinition = {
        installationId: value.installationId,
        repositoryId: value.repositoryId,
        registryTargetId: value.registryTargetId,
        triggerRef,
        contextPath: value.contextPath.trim(),
        dockerfilePath: value.dockerfilePath.trim(),
        platforms,
        buildArgs: parseBuildArgs(value.buildArgs),
        cacheTrustLane: value.cacheTrustLane.trim(),
        cacheImports: value.cacheImports,
        profile: {
          resource: value.profileResource.trim(),
          timeoutSeconds: value.timeoutSeconds,
          egress: value.profileEgress.trim(),
        },
        maxAttempts: value.maxAttempts,
        ...(value.secretProfileIds.length
          ? { secretProfileIds: value.secretProfileIds }
          : {}),
        ...(value.sshProfileIds.length
          ? { sshProfileIds: value.sshProfileIds }
          : {}),
      };
      const signature = JSON.stringify(input);
      const key =
        stableAttempt.current?.signature === signature
          ? stableAttempt.current.key
          : crypto.randomUUID();
      stableAttempt.current = { signature, key };
      create.mutate({
        input,
        key,
        draftSignature: JSON.stringify(value),
        applicationId: application.id,
      });
    } catch (error) {
      setParseError(error);
    }
  };

  if (!humanSession) {
    return (
      <div className="notice notice--warning">
        <div>
          <strong>Build-definition changes require a human session</strong>
          <p>This web form is hidden for service-account authentication.</p>
        </div>
      </div>
    );
  }
  if (!canCreate) {
    return (
      <div className="notice">
        <div>
          <strong>Build definitions are read-only</strong>
          <p>
            Your effective application scope does not include
            build-definitions:write.
          </p>
        </div>
      </div>
    );
  }

  const noInstallations =
    !installations.isPending && !installations.data?.items.length;
  const noTargets = registryTargets.length === 0;

  return (
    <form
      className="build-definition-form"
      onSubmit={form.handleSubmit(submit)}
    >
      <section className="service-settings-section">
        <div className="service-settings-section__header">
          <div>
            <span className="eyebrow">Source</span>
            <h2>Repository</h2>
            <p>
              Choose the GitHub repository and branch or tag that should trigger
              this service build.
            </p>
          </div>
        </div>
        <div className="build-definition-form__grid">
          <Field
            label="GitHub installation"
            required
            error={form.formState.errors.installationId?.message}
          >
            <select
              {...form.register("installationId", {
                required: "Select a linked installation.",
                onChange: () => form.setValue("repositoryId", ""),
              })}
            >
              <option value="">Select installation</option>
              {installations.data?.items.map((installation) => (
                <option key={installation.id} value={installation.id}>
                  {installation.accountLogin}
                </option>
              ))}
            </select>
          </Field>
          <Field
            label="Repository"
            required
            hint={
              installationId
                ? "Only verified active repositories are listed."
                : "Select an installation first."
            }
            error={form.formState.errors.repositoryId?.message}
          >
            <select
              disabled={!installationId || repositories.isPending}
              {...form.register("repositoryId", {
                required: "Select a verified repository.",
              })}
            >
              <option value="">Select repository</option>
              {activeRepositories.map((repository) => (
                <option key={repository.id} value={repository.id}>
                  {repository.ownerLogin}/{repository.name}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Source type" required>
            <select {...form.register("refType")}>
              <option value="branch">Branch</option>
              <option value="tag">Tag</option>
            </select>
          </Field>
          <Field
            label={form.watch("refType") === "tag" ? "Tag" : "Branch"}
            required
            hint="Enter the readable name only; Kuberploy stores the canonical Git ref."
            error={form.formState.errors.triggerRef?.message}
          >
            <input
              {...form.register("triggerRef", {
                required: "Enter a branch or tag name.",
                pattern: {
                  value: /^[A-Za-z0-9][A-Za-z0-9._/-]*$/,
                  message: "Use a readable branch or tag name.",
                },
                maxLength: {
                  value: 255,
                  message: "Use at most 255 characters.",
                },
              })}
            />
          </Field>
        </div>
      </section>

      <section className="service-settings-section">
        <div className="service-settings-section__header">
          <div>
            <span className="eyebrow">Build</span>
            <h2>Dockerfile</h2>
            <p>
              Configure the build context, output registry, and target
              platforms. The defaults work for most services.
            </p>
          </div>
        </div>
        <div className="build-definition-form__grid">
          <Field
            label="Registry target"
            required
            hint="The built image is pushed here. Runtime pull credentials are configured separately."
            error={form.formState.errors.registryTargetId?.message}
          >
            <select
              {...form.register("registryTargetId", {
                required: "Select an accessible registry target.",
              })}
            >
              <option value="">Select target</option>
              {registryTargets.map((target) => (
                <option key={target.id} value={target.id}>
                  {target.name} · {target.mode}
                </option>
              ))}
            </select>
          </Field>
          <Field
            label="Build context"
            required
            error={form.formState.errors.contextPath?.message}
          >
            <input
              {...form.register("contextPath", {
                required: "Enter a repository-relative context path.",
                maxLength: {
                  value: 512,
                  message: "Use at most 512 characters.",
                },
              })}
            />
          </Field>
          <Field
            label="Dockerfile"
            required
            error={form.formState.errors.dockerfilePath?.message}
          >
            <input
              {...form.register("dockerfilePath", {
                required: "Enter a repository-relative Dockerfile path.",
                maxLength: {
                  value: 512,
                  message: "Use at most 512 characters.",
                },
              })}
            />
          </Field>
        </div>

        <fieldset className="build-platforms">
          <legend>Platforms</legend>
          <label>
            <input type="checkbox" {...form.register("amd64")} /> linux/amd64
          </label>
          <label>
            <input type="checkbox" {...form.register("arm64")} /> linux/arm64
          </label>
        </fieldset>

        <Field
          label="Docker build arguments"
          hint="Build time only. One NAME=value per line; runtime environment values are never passed to the builder."
        >
          <textarea
            rows={5}
            placeholder="APP_ENV=production"
            {...form.register("buildArgs")}
          />
        </Field>

        <div className="notice notice--warning">
          <div>
            <strong>Docker build arguments are not secret storage</strong>
            <p>
              Values may be retained in image history or build cache. Do not
              place passwords, tokens, or private keys here.
            </p>
          </div>
        </div>
      </section>

      <details className="service-settings-advanced">
        <summary>
          <span>
            <strong>Advanced build policy</strong>
            <small>Cache, resources, egress, timeout, and retry limits</small>
          </span>
          <Icon name="chevron" />
        </summary>
        <div className="service-settings-advanced__content">
          <div className="build-definition-form__grid build-definition-form__grid--compact">
            <Field label="Cache trust lane" required>
              <input
                {...form.register("cacheTrustLane", {
                  required: "Enter a cache trust lane.",
                  pattern: {
                    value: namePattern,
                    message: "Use a canonical profile name.",
                  },
                })}
              />
            </Field>
            <Field label="Cache imports" required hint="1–8 generations">
              <input
                type="number"
                min={1}
                max={8}
                {...form.register("cacheImports", {
                  valueAsNumber: true,
                  min: 1,
                  max: 8,
                })}
              />
            </Field>
            <Field label="Resource profile" required>
              <input
                {...form.register("profileResource", {
                  required: true,
                  pattern: namePattern,
                })}
              />
            </Field>
            <Field label="Timeout (seconds)" required hint="60–7200">
              <input
                type="number"
                min={60}
                max={7200}
                {...form.register("timeoutSeconds", {
                  valueAsNumber: true,
                  min: 60,
                  max: 7200,
                })}
              />
            </Field>
            <Field label="Egress profile" required>
              <input
                {...form.register("profileEgress", {
                  required: true,
                  pattern: namePattern,
                })}
              />
            </Field>
            <Field label="Infrastructure attempts" required hint="1–5">
              <input
                type="number"
                min={1}
                max={5}
                {...form.register("maxAttempts", {
                  valueAsNumber: true,
                  min: 1,
                  max: 5,
                })}
              />
            </Field>
          </div>

          <div className="build-secret-profile-picker">
            <div>
              <strong>Managed build credentials</strong>
              <p>
                Select operator-managed profiles for BuildKit secret or SSH
                mounts. Secret values never enter this form or API request.
              </p>
            </div>
            {secretProfiles.isPending ? (
              <p>Loading approved profiles…</p>
            ) : null}
            {secretProfiles.error ? (
              <ErrorPanel error={secretProfiles.error} />
            ) : null}
            {!secretProfiles.isPending &&
            !secretProfiles.error &&
            !secretProfiles.data?.build.length &&
            !secretProfiles.data?.ssh.length ? (
              <p>No managed build-secret or SSH profiles are configured.</p>
            ) : null}
            {secretProfiles.data?.build.length ? (
              <fieldset>
                <legend>Build-secret profiles</legend>
                {secretProfiles.data.build.map((profile) => (
                  <label key={profile.id}>
                    <input
                      type="checkbox"
                      value={profile.id}
                      {...form.register("secretProfileIds")}
                    />
                    {profile.label}
                  </label>
                ))}
              </fieldset>
            ) : null}
            {secretProfiles.data?.ssh.length ? (
              <fieldset>
                <legend>SSH profiles</legend>
                {secretProfiles.data.ssh.map((profile) => (
                  <label key={profile.id}>
                    <input
                      type="checkbox"
                      value={profile.id}
                      {...form.register("sshProfileIds")}
                    />
                    {profile.label}
                  </label>
                ))}
              </fieldset>
            ) : null}
          </div>
          <div className="notice notice--warning">
            <div>
              <strong>Never put credentials in Docker build arguments</strong>
              <p>
                Build arguments can remain in image history or cache. Use an
                approved profile above for private build inputs.
              </p>
            </div>
          </div>
        </div>
      </details>

      {installations.error ? <ErrorPanel error={installations.error} /> : null}
      {repositories.error ? <ErrorPanel error={repositories.error} /> : null}
      {noInstallations ? (
        <div className="notice notice--warning">
          <div>
            <strong>Link a GitHub installation first</strong>
            <p>
              The source catalog is empty; no arbitrary clone URL is accepted.
            </p>
          </div>
        </div>
      ) : null}
      {noTargets ? (
        <div className="notice notice--warning">
          <div>
            <strong>No accessible registry target</strong>
            <p>
              Attach a registry policy to this application or ask a platform
              administrator to configure a target.
            </p>
          </div>
        </div>
      ) : null}
      {parseError ? (
        <ErrorPanel
          title="Definition is not safe to submit"
          error={parseError}
        />
      ) : null}
      {create.error ? (
        <ErrorPanel
          title="Could not create build definition"
          error={create.error}
        />
      ) : null}
      {createdDefinitionId ? (
        <div className="notice notice--success" role="status">
          <div>
            <strong>Immutable build definition created</strong>
            <p>
              Push the matching ref to start a build. Definition{" "}
              {createdDefinitionId} remains available in history.
            </p>
          </div>
        </div>
      ) : null}
      <div className="build-definition-form__actions">
        <Button
          type="submit"
          busy={create.isPending}
          disabled={noInstallations || noTargets}
        >
          <Icon name="plus" /> Create immutable definition
        </Button>
      </div>
    </form>
  );
}
