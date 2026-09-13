import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentProps } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api/client";
import type {
  Application,
  ConfigBundle,
  Deployment,
  Operation,
} from "../api/types";
import { defaultConfigYaml } from "../lib/configDraft";
import { ConfigEditor } from "./ConfigEditor";

vi.mock("./MonacoYamlEditor", () => ({
  MonacoYamlEditor: ({
    value,
    onChange,
    readOnly,
  }: {
    value: string;
    onChange: (value: string) => void;
    readOnly?: boolean;
  }) => (
    <textarea
      aria-label="AppConfig YAML"
      value={value}
      readOnly={readOnly}
      onChange={(event) => onChange(event.target.value)}
    />
  ),
}));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("configuration publication tracking", () => {
  async function fixture() {
    vi.useFakeTimers();
    const rawYaml = defaultConfigYaml({
      name: "API",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      port: 8080,
    });
    const initial: ConfigBundle = {
      kind: "ConfigBundle",
      etag: `"sha256:${"a".repeat(64)}"`,
      targetHeadRevision: "a".repeat(40),
      indexedRevision: "a".repeat(40),
      configRevision: "a".repeat(40),
      freshness: "fresh",
      documents: [{ id: "app.yaml", documentId: "app.yaml", rawYaml }],
    };
    const operation: Operation = {
      id: "config-operation",
      kind: "deployment.config.update",
      status: "queued",
      state: "queued",
      targetType: "deployment",
      targetId: "config-deployment",
      requestId: "request-1",
      generation: 2,
      progress: [],
      createdAt: "2026-09-13T00:00:00Z",
      updatedAt: "2026-09-13T00:00:00Z",
    };
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: {},
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-1",
          actions: ["deployment-config:write"],
        },
      ],
    });
    const configuration = vi
      .spyOn(api, "deploymentConfig")
      .mockResolvedValue(initial);
    const operationRequest = vi
      .spyOn(api, "operation")
      .mockResolvedValue(operation);
    const save = vi
      .spyOn(api, "saveDeploymentConfig")
      .mockResolvedValue(operation);
    const preview = vi.spyOn(api, "previewDeploymentConfig").mockResolvedValue({
      previewToken: "p".repeat(43),
      gitDiff: "+cpu: 500m",
      renderedDiff: "+cpu: 500m",
      semanticChanges: [],
      warnings: [],
      expiresAt: "2026-09-13T00:10:00Z",
      renderIdentity: {
        contract: "appconfig-rendered-preview.v1",
        chartName: "kuberploy-runtime",
        chartVersion: "1.2.3",
        chartDigest: `sha256:${"a".repeat(64)}`,
        rendererImage: `docker.io/alpine/helm:4.2.3@sha256:${"b".repeat(64)}`,
        rendererVersion: "4.2.3",
        policyVersion: "external-helm-p0.v1",
      },
      renderIdentityDigest: `sha256:${"c".repeat(64)}`,
    });
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    render(
      <QueryClientProvider client={client}>
        <ConfigEditor
          application={{ id: "app-1", projectId: "project-1", name: "API" }}
          deployment={{
            id: operation.targetId,
            applicationId: "app-1",
            environmentId: "environment-1",
            image: `registry.example/api@sha256:${"b".repeat(64)}`,
            runtime: {
              replicas: 1,
              ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
              resources: { requests: { cpu: "50m", memory: "100Mi" } },
            },
          }}
        />
      </QueryClientProvider>,
    );
    await act(() => vi.advanceTimersByTimeAsync(20));
    fireEvent.click(screen.getByRole("tab", { name: /Advanced YAML/ }));
    const editor = screen.getByRole("textbox", { name: "AppConfig YAML" });
    const submitted = rawYaml.replace("50m", "500m");
    fireEvent.change(editor, { target: { value: submitted } });
    const previewButton = screen.getByRole("button", {
      name: /Preview configuration/,
    });
    const commitButton = screen.getByRole("button", {
      name: /Commit configuration/,
    });
    const submit = async () => {
      fireEvent.click(previewButton);
      await act(() => vi.advanceTimersByTimeAsync(20));
      fireEvent.click(commitButton);
      await act(() => vi.advanceTimersByTimeAsync(20));
    };
    return {
      initial,
      operation,
      configuration,
      operationRequest,
      save,
      preview,
      client,
      editor,
      submitted,
      previewButton,
      commitButton,
      submit,
    };
  }

  it.each([false, true])(
    "waits for publication, refreshes its base, and preserves a newer draft: %s",
    async (editWhilePending) => {
      const f = await fixture();
      await f.submit();
      expect(screen.getByText("Saving configuration")).toBeVisible();
      expect(f.configuration).toHaveBeenCalledTimes(1);
      expect(f.previewButton).toBeDisabled();
      expect(f.commitButton).toBeDisabled();
      expect(f.editor).toHaveValue(f.submitted);
      const newer = f.submitted.replace("500m", "600m");
      if (editWhilePending)
        fireEvent.change(f.editor, { target: { value: newer } });
      const published = {
        ...f.initial,
        etag: `"sha256:${"b".repeat(64)}"`,
        configRevision: "b".repeat(40),
        // An unrelated Git write may follow this App's own publication.
        indexedRevision: "c".repeat(40),
        targetHeadRevision: "c".repeat(40),
        documents: [
          {
            id: "app.yaml",
            documentId: "app.yaml",
            rawYaml: `${f.submitted}\n# server formatting\n`,
          },
        ],
      };
      f.operationRequest.mockResolvedValue({
        ...f.operation,
        status: "succeeded",
        state: "succeeded",
        gitRevision: "b".repeat(40),
      });
      f.configuration
        .mockRejectedValueOnce(
          new ApiError(503, {
            title: "Projection pending",
            status: 503,
            code: "GitProjectionNotReady",
          }),
        )
        .mockResolvedValue(published);
      await act(() => vi.advanceTimersByTimeAsync(2_020));
      expect(f.configuration).toHaveBeenLastCalledWith("config-deployment", {
        atLeastRevision: "b".repeat(40),
        waitSeconds: 5,
      });
      expect(f.previewButton).toBeDisabled();
      await act(() => vi.advanceTimersByTimeAsync(2_020));
      expect(screen.getByText("Configuration saved")).toBeVisible();
      expect(f.editor).toHaveValue(
        editWhilePending ? newer : published.documents[0]?.rawYaml,
      );
      expect(f.previewButton).toBeEnabled();
      if (!editWhilePending)
        fireEvent.change(f.editor, { target: { value: newer } });
      fireEvent.click(f.previewButton);
      await act(() => vi.advanceTimersByTimeAsync(20));
      expect(f.preview).toHaveBeenLastCalledWith(
        "config-deployment",
        expect.objectContaining({
          documents: [{ documentId: "app.yaml", rawYaml: newer }],
        }),
        published.etag,
      );
      expect(f.commitButton).toBeEnabled();
    },
  );

  it.each([false, true])(
    "handles a later same-App publication without silently rebasing newer edits: %s",
    async (editWhilePending) => {
      const f = await fixture();
      await f.submit();
      const newerDraft = f.submitted.replace("500m", "600m");
      if (editWhilePending)
        fireEvent.change(f.editor, { target: { value: newerDraft } });
      const externalConfig: ConfigBundle = {
        ...f.initial,
        etag: `"sha256:${"c".repeat(64)}"`,
        configRevision: "c".repeat(40),
        indexedRevision: "c".repeat(40),
        targetHeadRevision: "c".repeat(40),
        documents: [
          {
            id: "app.yaml",
            documentId: "app.yaml",
            rawYaml: f.submitted.replace("500m", "900m"),
          },
        ],
      };
      f.operationRequest.mockResolvedValue({
        ...f.operation,
        status: "succeeded",
        state: "succeeded",
        gitRevision: "b".repeat(40),
      });
      f.configuration.mockResolvedValue(externalConfig);
      await act(() => vi.advanceTimersByTimeAsync(2_020));
      if (editWhilePending) {
        expect(
          screen.getByText("Configuration changed while you were editing"),
        ).toBeVisible();
        expect(
          screen.getByText(
            /Your newer draft and its original revision are preserved/,
          ),
        ).toBeVisible();
        expect(f.editor).toHaveValue(newerDraft);
        f.preview.mockRejectedValue(
          new ApiError(412, { title: "Configuration changed", status: 412 }),
        );
        fireEvent.click(f.previewButton);
        await act(() => vi.advanceTimersByTimeAsync(20));
        expect(f.preview).toHaveBeenLastCalledWith(
          "config-deployment",
          expect.anything(),
          f.initial.etag,
        );
        expect(f.commitButton).toBeDisabled();
      } else {
        expect(screen.getByText("Configuration saved")).toBeVisible();
        expect(f.editor).toHaveValue(externalConfig.documents[0]?.rawYaml);
        fireEvent.click(f.previewButton);
        await act(() => vi.advanceTimersByTimeAsync(20));
        expect(f.preview).toHaveBeenLastCalledWith(
          "config-deployment",
          expect.anything(),
          externalConfig.etag,
        );
      }
    },
  );

  it("retains a real conflicting draft instead of silently rebasing it onto a background refresh", async () => {
    const f = await fixture();
    f.preview.mockRejectedValue(
      new ApiError(412, {
        title: "Configuration changed",
        status: 412,
        detail: "Another change was published. Reload and preview again.",
      }),
    );
    await act(async () => {
      f.client.setQueryData(["deployment-config", "config-deployment"], {
        ...f.initial,
        etag: `"sha256:${"c".repeat(64)}"`,
      });
    });
    await act(() => vi.advanceTimersByTimeAsync(20));
    fireEvent.click(f.previewButton);
    await act(() => vi.advanceTimersByTimeAsync(20));
    expect(f.preview).toHaveBeenLastCalledWith(
      "config-deployment",
      expect.anything(),
      f.initial.etag,
    );
    expect(
      screen.getByText(
        "Another change was published. Reload and preview again.",
      ),
    ).toBeVisible();
    expect(f.editor).toHaveValue(f.submitted);
    expect(f.commitButton).toBeDisabled();
    expect(f.save).not.toHaveBeenCalled();
  });

  it.each([false, true])(
    "preserves a protected save awaiting review, then reloads only on explicit draft replacement: %s",
    async (editWhilePending) => {
      const f = await fixture();
      const pullRequest = {
        number: 23,
        url: "https://github.com/example/config/pull/23",
        state: "open" as const,
        candidateRevision: "b".repeat(40),
      };
      const reviewOperation = {
        ...f.operation,
        state: "succeeded",
        status: "succeeded",
        pullRequest,
      };
      f.operationRequest.mockResolvedValue(reviewOperation);
      await f.submit();
      const newer = f.submitted.replace("500m", "600m");
      if (editWhilePending)
        fireEvent.change(f.editor, { target: { value: newer } });
      expect(screen.getByText("Awaiting pull request merge")).toBeVisible();
      expect(screen.queryByText("Configuration saved")).not.toBeInTheDocument();
      expect(
        screen.getByRole("link", { name: "Review pull request" }),
      ).toHaveAttribute("href", pullRequest.url);
      expect(f.configuration).toHaveBeenCalledTimes(1);
      expect(f.editor).toHaveValue(editWhilePending ? newer : f.submitted);
      expect(f.previewButton).toBeDisabled();
      await act(() => vi.advanceTimersByTimeAsync(2_020));
      expect(f.operationRequest.mock.calls.length).toBeGreaterThan(1);

      f.operationRequest.mockResolvedValue({
        ...reviewOperation,
        pullRequest: { ...pullRequest, state: "closed" },
      });
      await act(() => vi.advanceTimersByTimeAsync(2_020));
      expect(screen.getByText("Pull request closed")).toBeVisible();
      expect(screen.queryByText("Configuration saved")).not.toBeInTheDocument();
      expect(f.configuration).toHaveBeenCalledTimes(1);
      expect(f.editor).toHaveValue(editWhilePending ? newer : f.submitted);
      expect(f.previewButton).toBeDisabled();
      const calls = f.operationRequest.mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(30_000));
      expect(f.operationRequest).toHaveBeenCalledTimes(calls);
      const published: ConfigBundle = {
        ...f.initial,
        etag: `"sha256:${"d".repeat(64)}"`,
        configRevision: "d".repeat(40),
        documents: [
          { id: "app.yaml", documentId: "app.yaml", rawYaml: f.submitted },
        ],
      };
      f.configuration.mockResolvedValue(published);
      fireEvent.click(
        screen.getByRole("button", {
          name: "Replace draft with saved configuration",
        }),
      );
      await act(() => vi.advanceTimersByTimeAsync(20));
      expect(screen.getByText("Saved configuration loaded")).toBeVisible();
      expect(f.editor).toHaveValue(f.submitted);
      fireEvent.click(f.previewButton);
      await act(() => vi.advanceTimersByTimeAsync(20));
      expect(f.preview).toHaveBeenLastCalledWith(
        "config-deployment",
        expect.anything(),
        published.etag,
      );
    },
  );

  it("keeps edits made while an explicit post-review reload is in flight", async () => {
    const f = await fixture();
    f.operationRequest.mockResolvedValue({
      ...f.operation,
      state: "succeeded",
      status: "succeeded",
      pullRequest: {
        number: 23,
        url: "https://github.com/example/config/pull/23",
        state: "closed",
        candidateRevision: "b".repeat(40),
      },
    });
    await f.submit();
    let resolveReload!: (bundle: ConfigBundle) => void;
    f.configuration.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveReload = resolve;
        }),
    );
    fireEvent.click(
      screen.getByRole("button", {
        name: "Replace draft with saved configuration",
      }),
    );
    await act(() => vi.advanceTimersByTimeAsync(20));
    const newer = f.submitted.replace("500m", "600m");
    fireEvent.change(f.editor, { target: { value: newer } });
    await act(async () => {
      resolveReload({ ...f.initial, etag: `"sha256:${"d".repeat(64)}"` });
    });
    await act(() => vi.advanceTimersByTimeAsync(20));
    expect(screen.getByText("Draft changed during reload")).toBeVisible();
    expect(f.editor).toHaveValue(newer);
    expect(f.previewButton).toBeDisabled();
    expect(
      screen.queryByText("Saved configuration loaded"),
    ).not.toBeInTheDocument();
  });

  it("stops at a failed operation, preserves edits, and provides an explicit status check", async () => {
    const f = await fixture();
    f.operationRequest.mockResolvedValue({
      ...f.operation,
      status: "failed",
      state: "failed",
      problem: {
        title: "Publication failed",
        status: 409,
        detail: "The branch changed before publication.",
      },
    });
    await f.submit();
    expect(screen.getByText("Configuration update failed")).toBeVisible();
    expect(
      screen.getByText("The branch changed before publication."),
    ).toBeVisible();
    const calls = f.operationRequest.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(f.operationRequest).toHaveBeenCalledTimes(calls);
    expect(f.editor).toHaveValue(f.submitted);
    expect(f.previewButton).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Check save status" }));
    await act(() => vi.advanceTimersByTimeAsync(20));
    expect(f.operationRequest).toHaveBeenCalledTimes(calls + 1);
    expect(f.save).toHaveBeenCalledTimes(1);
  });

  it.each([false, true])(
    "stops automatic status checks on an authorization error and retries only on request, with open review %s",
    async (review) => {
      const f = await fixture();
      const current = review
        ? {
            ...f.operation,
            state: "succeeded",
            status: "succeeded",
            pullRequest: {
              number: 23,
              url: "https://github.com/example/config/pull/23",
              state: "open" as const,
              candidateRevision: "b".repeat(40),
            },
          }
        : f.operation;
      if (review) {
        f.operationRequest.mockResolvedValue(current);
        await f.submit();
      }
      f.operationRequest.mockRejectedValue(
        new ApiError(403, { title: "Access denied", status: 403 }),
      );
      if (review) await act(() => vi.advanceTimersByTimeAsync(2_020));
      else await f.submit();
      expect(screen.getByText("Save status needs attention")).toBeVisible();
      expect(f.previewButton).toBeDisabled();
      const calls = f.operationRequest.mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(30_000));
      expect(f.operationRequest).toHaveBeenCalledTimes(calls);
      f.operationRequest.mockResolvedValue(current);
      fireEvent.click(
        screen.getByRole("button", { name: "Check save status" }),
      );
      await act(() => vi.advanceTimersByTimeAsync(20));
      expect(f.operationRequest).toHaveBeenCalledTimes(calls + 1);
      expect(
        screen.getByText(
          review ? "Awaiting pull request merge" : "Saving configuration",
        ),
      ).toBeVisible();
      expect(f.editor).toHaveValue(f.submitted);
    },
  );

  it.each([false, true])(
    "bounds a stalled save's polling and offers a fresh bounded check, with open review %s",
    async (review) => {
      const f = await fixture();
      if (review)
        f.operationRequest.mockResolvedValue({
          ...f.operation,
          state: "succeeded",
          status: "succeeded",
          pullRequest: {
            number: 23,
            url: "https://github.com/example/config/pull/23",
            state: "open",
            candidateRevision: "b".repeat(40),
          },
        });
      await f.submit();
      await act(() => vi.advanceTimersByTimeAsync(15 * 60_000));
      expect(screen.getByText("Save status needs attention")).toBeVisible();
      const calls = f.operationRequest.mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(30_000));
      expect(f.operationRequest).toHaveBeenCalledTimes(calls);
      expect(f.previewButton).toBeDisabled();
      fireEvent.click(
        screen.getByRole("button", { name: "Check save status" }),
      );
      await act(() => vi.advanceTimersByTimeAsync(2_020));
      expect(f.operationRequest.mock.calls.length).toBeGreaterThan(calls);
      expect(f.save).toHaveBeenCalledTimes(1);
      expect(f.editor).toHaveValue(f.submitted);
    },
  );
});

describe("deployment ConfigEditor preview binding", () => {
  it.each([
    ["github", "build"],
    ["git-ssh", "ssh"],
  ] as const)(
    "guides a new %s draft to its first build without requesting a missing configuration",
    (sourceKind, source) => {
      const configuration = vi.spyOn(api, "deploymentConfig");
      render(
        <ConfigEditor
          deployment={{
            id: "draft-1",
            applicationId: "app-1",
            environmentId: "environment-1",
            state: "stopped",
            runtime: {
              resources: { requests: { cpu: "50m", memory: "100Mi" } },
              replicas: 1,
              ports: [{ name: "http", containerPort: 3000, protocol: "TCP" }],
            },
          }}
          application={{
            id: "app-1",
            projectId: "project-1",
            name: "New App",
            sourceKind,
          }}
        />,
      );
      expect(
        screen.getByRole("heading", { name: "Build this App first" }),
      ).toBeInTheDocument();
      const link = screen.getByRole("link", { name: "Open Source & build" });
      expect(link).toHaveAttribute(
        "href",
        `/projects/project-1/environments/environment-1/apps/app-1?tab=source&source=${source}&environmentId=environment-1`,
      );
      expect(configuration).not.toHaveBeenCalled();
      expect(
        screen.queryByRole("button", { name: "Commit configuration" }),
      ).not.toBeInTheDocument();
    },
  );

  it("preserves real configuration failures for an image-backed stopped source App", async () => {
    vi.spyOn(api, "deploymentConfig").mockRejectedValue(
      new Error("Configuration projection missing"),
    );
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: {},
      capabilities: [],
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <ConfigEditor
          deployment={{
            id: "saved-1",
            applicationId: "app-1",
            environmentId: "environment-1",
            state: "stopped",
            image: `registry.example/app@sha256:${"a".repeat(64)}`,
            runtime: {
              resources: { requests: { cpu: "50m", memory: "100Mi" } },
              replicas: 1,
              ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
            },
          }}
          application={{
            id: "app-1",
            projectId: "project-1",
            name: "Saved App",
            sourceKind: "github",
          }}
        />
      </QueryClientProvider>,
    );
    expect(
      await screen.findByText("Configuration could not be loaded"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Build this App first" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Commit configuration" }),
    ).toBeDisabled();
  });

  it("waits for the server document before initializing Guided fields", async () => {
    const deployment: Deployment = {
      id: "deployment-delayed",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: {},
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-1",
          actions: ["deployment-config:write"],
        },
      ],
    });
    let resolveConfig!: (
      value: Awaited<ReturnType<typeof api.deploymentConfig>>,
    ) => void;
    vi.spyOn(api, "deploymentConfig").mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveConfig = resolve;
        }),
    );
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor deployment={deployment} application={application} />
      </QueryClientProvider>,
    );

    expect(
      screen.queryByRole("combobox", { name: /Deployment strategy/i }),
    ).not.toBeInTheDocument();
    await act(async () =>
      resolveConfig({
        kind: "ConfigBundle",
        etag: `"cfg-sha256-${"a".repeat(64)}"`,
        targetHeadRevision: "",
        indexedRevision: "",
        configRevision: "server-revision",
        freshness: "projection-only",
        documents: [
          {
            id: "app.yaml",
            documentId: "app.yaml",
            rawYaml: `apiVersion: config.kuberploy.io/v1alpha1
kind: AppConfig
metadata:
  id: application-1
  name: API
spec:
  runtime:
    replicas: 2
    strategy:
      type: Recreate
    resources:
      requests:
        cpu: 100m
        memory: 128Mi
`,
          },
        ],
      }),
    );

    expect(
      await screen.findByRole("combobox", { name: /Deployment strategy/i }),
    ).toHaveValue("Recreate");
    expect(screen.getByRole("textbox", { name: /CPU request/i })).toHaveValue(
      "100m",
    );
    expect(
      screen.getByRole("textbox", { name: /Memory request/i }),
    ).toHaveValue("128Mi");
  });

  it("enables save only for the exact successful preview and retains a failed draft", async () => {
    const etag = `"cfg-sha256-${"a".repeat(64)}"`;
    const rawYaml =
      "apiVersion: config.kuberploy.io/v1alpha1\nkind: AppConfig\n";
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { traefikMiddlewares: true },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-1",
          actions: ["deployment-config:write"],
        },
      ],
    });
    vi.spyOn(api, "deploymentConfig").mockResolvedValue({
      kind: "ConfigBundle",
      etag,
      targetHeadRevision: "",
      indexedRevision: "",
      configRevision: etag,
      freshness: "projection-only",
      documents: [
        {
          id: "app.yaml",
          documentId: "app.yaml",
          rawYaml,
          editablePointers: ["/spec/runtime/replicas"],
          lockedPointers: ["/spec/delivery"],
        },
      ],
    });
    const preview = vi
      .spyOn(api, "previewDeploymentConfig")
      .mockResolvedValueOnce({
        previewToken: "p".repeat(43),
        gitDiff: "+replicas: 2",
        renderedDiff: "+replicas: 2",
        semanticChanges: [],
        warnings: [],
        expiresAt: "2026-08-09T00:10:00Z",
        renderIdentity: {
          contract: "appconfig-rendered-preview.v1",
          chartName: "kuberploy-runtime",
          chartVersion: "1.2.3",
          chartDigest: `sha256:${"a".repeat(64)}`,
          rendererImage: `docker.io/alpine/helm:4.2.3@sha256:${"b".repeat(64)}`,
          rendererVersion: "4.2.3",
          policyVersion: "external-helm-p0.v1",
        },
        renderIdentityDigest: `sha256:${"c".repeat(64)}`,
      })
      .mockRejectedValueOnce(new Error("Locked field at /spec/delivery"));
    const saveRequest = vi
      .spyOn(api, "saveDeploymentConfig")
      .mockRejectedValue(new Error("ambiguous network failure"));
    const deployment: Deployment = {
      id: "deployment-1",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const props: ComponentProps<typeof ConfigEditor> = {
      deployment,
      application,
    };
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor {...props} />
      </QueryClientProvider>,
    );
    await screen.findByText("1 locked fields");
    const save = screen.getByRole("button", { name: /Commit configuration/i });
    const previewButton = screen.getByRole("button", {
      name: /Preview configuration/i,
    });
    expect(save).toBeDisabled();
    const command = screen.getByRole("textbox", {
      name: "Container command (YAML list)",
    });
    await user.clear(command);
    await user.type(command, "/bin/sh -c 'echo owned'");
    expect(
      (await screen.findAllByText(/never a shell string/i)).length,
    ).toBeGreaterThan(0);
    expect(previewButton).toBeDisabled();
    fireEvent.change(command, { target: { value: "[]" } });
    await waitFor(() => expect(previewButton).toBeEnabled());

    await user.click(previewButton);
    await waitFor(() => expect(save).toBeEnabled());
    expect(preview).toHaveBeenCalledWith(
      "deployment-1",
      expect.anything(),
      etag,
    );
    await user.click(save);
    expect(
      await screen.findByText("ambiguous network failure"),
    ).toBeInTheDocument();
    await user.click(save);
    await waitFor(() => expect(saveRequest).toHaveBeenCalledTimes(2));
    expect(saveRequest.mock.calls[0]?.[4]).toBeTruthy();
    expect(saveRequest.mock.calls[1]?.[4]).toBe(saveRequest.mock.calls[0]?.[4]);

    await user.click(screen.getByRole("tab", { name: /Advanced YAML/i }));
    const editor = screen.getByRole("textbox", { name: "AppConfig YAML" });
    await user.type(editor, "# changed\n");
    expect(save).toBeDisabled();
    const retainedDraft = (editor as HTMLTextAreaElement).value;
    await user.click(previewButton);
    expect(
      await screen.findByText("Locked field at /spec/delivery"),
    ).toBeInTheDocument();
    expect(editor).toHaveValue(retainedDraft);
    expect(save).toBeDisabled();
  });

  it("does not replace a newer YAML draft when an earlier save completes", async () => {
    const etag = `"cfg-sha256-${"a".repeat(64)}"`;
    const rawYaml = defaultConfigYaml({
      name: "API",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      port: 8080,
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: {},
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-1",
          actions: ["deployment-config:write"],
        },
      ],
    });
    vi.spyOn(api, "deploymentConfig").mockResolvedValue({
      kind: "ConfigBundle",
      etag,
      targetHeadRevision: "",
      indexedRevision: "",
      configRevision: etag,
      freshness: "projection-only",
      documents: [{ id: "app.yaml", documentId: "app.yaml", rawYaml }],
    });
    vi.spyOn(api, "previewDeploymentConfig").mockResolvedValue({
      previewToken: "p".repeat(43),
      gitDiff: "+replicas: 2",
      renderedDiff: "+replicas: 2",
      semanticChanges: [],
      warnings: [],
      expiresAt: "2026-08-09T00:10:00Z",
      renderIdentity: {
        contract: "appconfig-rendered-preview.v1",
        chartName: "kuberploy-runtime",
        chartVersion: "1.2.3",
        chartDigest: `sha256:${"a".repeat(64)}`,
        rendererImage: `docker.io/alpine/helm:4.2.3@sha256:${"b".repeat(64)}`,
        rendererVersion: "4.2.3",
        policyVersion: "external-helm-p0.v1",
      },
      renderIdentityDigest: `sha256:${"c".repeat(64)}`,
    });
    let resolveSave!: (
      value: Awaited<ReturnType<typeof api.saveDeploymentConfig>>,
    ) => void;
    const saveRequest = vi
      .spyOn(api, "saveDeploymentConfig")
      .mockImplementation(
        () =>
          new Promise((resolve) => {
            resolveSave = resolve;
          }),
      );
    const deployment: Deployment = {
      id: "deployment-save-race",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor deployment={deployment} application={application} />
      </QueryClientProvider>,
    );

    const previewButton = await screen.findByRole("button", {
      name: /Preview configuration/i,
    });
    await waitFor(() => expect(previewButton).toBeEnabled());
    await user.click(previewButton);
    const save = await screen.findByRole("button", {
      name: /Commit configuration/i,
    });
    await waitFor(() => expect(save).toBeEnabled());
    await user.click(save);
    await waitFor(() => expect(saveRequest).toHaveBeenCalledOnce());

    await user.click(screen.getByRole("tab", { name: /Advanced YAML/i }));
    const editor = screen.getByRole("textbox", { name: "AppConfig YAML" });
    const newerDraft = `${rawYaml}\n# newer draft\n`;
    fireEvent.change(editor, { target: { value: newerDraft } });

    const accepted: Operation = {
      id: "earlier-save",
      kind: "deployment.config.update",
      status: "queued",
      state: "queued",
      targetType: "deployment",
      targetId: deployment.id,
      requestId: "request-1",
      generation: 2,
      progress: [],
      createdAt: "2026-09-13T00:00:00Z",
      updatedAt: "2026-09-13T00:00:00Z",
    };
    vi.spyOn(api, "operation").mockResolvedValue(accepted);
    await act(async () => {
      resolveSave(accepted);
    });
    await waitFor(() => expect(editor).toHaveValue(newerDraft));
  });

  it("loads the exact application/environment External DNS catalog for guided selection", async () => {
    const user = userEvent.setup();
    const deployment: Deployment = {
      id: "deployment-1",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    const etag = `"cfg-sha256-${"a".repeat(64)}"`;
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: {
        externalDNSConfiguration: true,
        externalDNS: true,
        traefikMiddlewares: true,
      },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-1",
          actions: ["deployment-config:write"],
        },
      ],
    });
    vi.spyOn(api, "deploymentConfig").mockResolvedValue({
      kind: "ConfigBundle",
      etag,
      targetHeadRevision: "",
      indexedRevision: "",
      configRevision: etag,
      freshness: "projection-only",
      documents: [
        {
          id: "app.yaml",
          documentId: "app.yaml",
          rawYaml: defaultConfigYaml({
            name: "API",
            image: deployment.image,
            port: 8080,
          }),
        },
      ],
    });
    const catalog = vi
      .spyOn(api, "applicationExternalDNSIntegrations")
      .mockResolvedValue({
        items: [
          {
            id: "integration-1",
            slug: "public-dns",
            name: "Public DNS",
            mode: "managed",
            providerKind: "cloudflare",
            allowedDomainSuffixes: ["example.com"],
            runtimeAvailable: true,
          },
        ],
        truncated: false,
        configurationState: "configured",
        controllerReadiness: "ready",
        runtimeAvailable: true,
      });
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor deployment={deployment} application={application} />
      </QueryClientProvider>,
    );

    await waitFor(() =>
      expect(catalog).toHaveBeenCalledWith(
        "application-1",
        "environment-1",
        100,
      ),
    );
    await user.type(screen.getByLabelText(/^Hostname/), "api.example.com");
    await user.click(screen.getByRole("radio", { name: /Automatic DNS/i }));
    const dnsIntegration = await screen.findByLabelText(/^DNS integration/);
    await openSelect(dnsIntegration);
    expect(
      screen.getByRole("option", { name: /^Public DNS/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Select an External DNS integration"),
    ).toBeVisible();
    await selectOption(dnsIntegration, "public-dns");
    expect(screen.getByText(/External DNS revision is ready/i)).toBeVisible();
  });

  it("keeps Guided and Advanced YAML read-only without scoped config write", async () => {
    const user = userEvent.setup();
    const deployment: Deployment = {
      id: "deployment-readonly",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { traefikMiddlewares: true },
      capabilities: [
        {
          scopeType: "application",
          scopeId: "application-1",
          actions: ["deployment-config:read"],
        },
      ],
    });
    vi.spyOn(api, "deploymentConfig").mockResolvedValue({
      kind: "ConfigBundle",
      etag: `"cfg-sha256-${"a".repeat(64)}"`,
      targetHeadRevision: "",
      indexedRevision: "",
      configRevision: "readonly",
      freshness: "projection-only",
      documents: [
        {
          id: "app.yaml",
          documentId: "app.yaml",
          rawYaml: defaultConfigYaml({
            name: "api",
            image: deployment.image,
            port: 8080,
          }),
        },
      ],
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor deployment={deployment} application={application} />
      </QueryClientProvider>,
    );

    expect(await screen.findByText("Configuration is read-only")).toBeVisible();
    expect(screen.getByLabelText(/^Hostname/)).toBeDisabled();
    expect(
      screen.getByRole("button", { name: /Add middleware/i }),
    ).toBeDisabled();
    expect(
      screen.getByRole("button", { name: /Preview configuration/i }),
    ).toBeDisabled();

    await user.click(screen.getByRole("tab", { name: /Advanced YAML/i }));
    expect(
      screen.getByRole("textbox", { name: "AppConfig YAML" }),
    ).toHaveAttribute("readonly");
  });

  it("gives the editor tablist one tab stop and answers the arrow keys", async () => {
    const user = userEvent.setup();
    const deployment: Deployment = {
      id: "deployment-tablist",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"c".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          scopeType: "application",
          scopeId: "application-1",
          actions: ["deployment-config:read"],
        },
      ],
    });
    vi.spyOn(api, "deploymentConfig").mockResolvedValue({
      kind: "ConfigBundle",
      etag: `"cfg-sha256-${"a".repeat(64)}"`,
      targetHeadRevision: "",
      indexedRevision: "",
      configRevision: "tablist",
      freshness: "projection-only",
      documents: [
        {
          id: "app.yaml",
          documentId: "app.yaml",
          rawYaml: defaultConfigYaml({
            name: "api",
            image: deployment.image,
            port: 8080,
          }),
        },
      ],
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor deployment={deployment} application={application} />
      </QueryClientProvider>,
    );

    const tabs = await screen.findAllByRole("tab");
    // A tablist is one tab stop, not one per tab.
    expect(tabs.map((tab) => tab.tabIndex)).toEqual([0, -1, -1]);

    tabs[0]!.focus();
    await user.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(tabs[1]);
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(tabs[2]);
    await user.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(tabs[0]);
    await user.keyboard("{Home}");
    expect(document.activeElement).toBe(tabs[0]);
  });

  it("gates only Guided middleware controls on runtime readiness while preserving Advanced YAML", async () => {
    const user = userEvent.setup();
    const deployment: Deployment = {
      id: "deployment-feature-gate",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: { traefikMiddlewares: false },
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-1",
          actions: ["deployment-config:write"],
        },
      ],
    });
    vi.spyOn(api, "deploymentConfig").mockResolvedValue({
      kind: "ConfigBundle",
      etag: `"cfg-sha256-${"a".repeat(64)}"`,
      targetHeadRevision: "",
      indexedRevision: "",
      configRevision: "feature-gated",
      freshness: "projection-only",
      documents: [
        {
          id: "app.yaml",
          documentId: "app.yaml",
          rawYaml: defaultConfigYaml({
            name: "api",
            image: deployment.image,
            port: 8080,
          }),
        },
      ],
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor deployment={deployment} application={application} />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByText(
        /has not reported the Traefik middleware runtime capability ready/i,
      ),
    ).toBeVisible();
    expect(screen.getByLabelText(/^Hostname/)).toBeEnabled();
    expect(
      screen.getByRole("button", { name: /Add middleware/i }),
    ).toBeDisabled();

    await user.click(screen.getByRole("tab", { name: /Advanced YAML/i }));
    expect(
      screen.getByRole("textbox", { name: "AppConfig YAML" }),
    ).not.toHaveAttribute("readonly");
  });

  it("preserves drafts that Guided mode cannot safely represent and directs the user to YAML", async () => {
    const user = userEvent.setup();
    const deployment: Deployment = {
      id: "deployment-legacy-secret",
      applicationId: "application-1",
      environmentId: "environment-1",
      image: `registry.example/api@sha256:${"b".repeat(64)}`,
      runtime: {
        replicas: 1,
        ports: [{ name: "http", containerPort: 8080, protocol: "TCP" }],
        resources: { requests: { cpu: "50m", memory: "100Mi" } },
      },
    };
    const application: Application = {
      id: "application-1",
      projectId: "project-1",
      name: "API",
    };
    const rawYaml = `apiVersion: config.kuberploy.io/v1alpha1
kind: AppConfig
metadata:
  name: api
spec:
  runtime:
    replicas: 1
    ports:
      - name: http
        containerPort: 8080
        protocol: TCP
    env:
      - name: TOKEN
        valueFrom:
          secretBindingRef:
            name: incomplete
`;
    vi.spyOn(api, "capabilities").mockResolvedValue({
      features: {},
      capabilities: [],
    });
    vi.spyOn(api, "deploymentConfig").mockResolvedValue({
      kind: "ConfigBundle",
      etag: `"cfg-sha256-${"a".repeat(64)}"`,
      targetHeadRevision: "",
      indexedRevision: "",
      configRevision: "legacy-secret",
      freshness: "projection-only",
      documents: [{ id: "app.yaml", documentId: "app.yaml", rawYaml }],
    });
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ConfigEditor deployment={deployment} application={application} />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByText("This configuration needs Advanced YAML"),
    ).toBeVisible();
    await user.click(
      screen.getByRole("button", { name: "Inspect Advanced YAML" }),
    );
    expect(screen.getByRole("textbox", { name: "AppConfig YAML" })).toHaveValue(
      rawYaml,
    );
  });
});
