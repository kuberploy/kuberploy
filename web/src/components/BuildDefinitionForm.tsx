import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useRef, useState } from "react";
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
};

type StableAttempt = { signature: string; key: string };

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
  const form = useForm<DefinitionForm>({
    defaultValues: {
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
    },
  });
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
    }: {
      input: CreateBuildDefinition;
      key: string;
    }) => api.createBuildDefinition(application.id, input, key),
    retry: retryNetworkOnce,
    onSuccess: async (definition) => {
      stableAttempt.current = null;
      setParseError(undefined);
      setCreatedDefinitionId(definition.id);
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ["build-definitions", application.id],
        }),
        queryClient.invalidateQueries({
          queryKey: ["build-attempts", application.id],
        }),
      ]);
    },
  });

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
      };
      const signature = JSON.stringify(input);
      const key =
        stableAttempt.current?.signature === signature
          ? stableAttempt.current.key
          : crypto.randomUUID();
      stableAttempt.current = { signature, key };
      create.mutate({ input, key });
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

          <div className="notice notice--warning">
            <div>
              <strong>
                Build-secret and SSH profiles are not available yet
              </strong>
              <p>
                This form accepts no build-secret values, file references, or
                SSH keys.
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
