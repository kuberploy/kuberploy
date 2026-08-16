import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Capability,
  HelmMutationResult,
  HelmReleaseStatus,
} from "../api/types";
import { HelmApplicationsPanel } from "./HelmApplicationsPanel";

const application = {
  id: "application-payments",
  projectId: "project-payments",
  name: "Payments",
};
const environment = {
  id: "environment-production",
  projectId: "project-payments",
  name: "Production",
  namespace: "payments-production",
};
const nextApplication = {
  id: "application-orders",
  projectId: "project-payments",
  name: "Orders",
};
const nextEnvironment = {
  id: "environment-staging",
  projectId: "project-payments",
  name: "Staging",
  namespace: "orders-staging",
};
const project = {
  id: "project-payments",
  name: "Payments",
  teamId: "team-commerce",
};
const approval = {
  id: "11111111-1111-4111-8111-111111111111",
  revision: 2,
  repository: "oci://registry.example.test/charts/payments",
  version: "1.2.3",
  manifestDigest: `sha256:${"a".repeat(64)}`,
  packageDigest: `sha256:${"b".repeat(64)}`,
  valuesSchemaDigest: `sha256:${"c".repeat(64)}`,
  rendererImage: `renderer@sha256:${"d".repeat(64)}`,
  rendererVersion: "4.2.3" as const,
  policyVersion: "external-helm-p0.v1" as const,
  documentsDigest: `sha256:${"e".repeat(64)}`,
  valuesSchema: { type: "object" },
  defaultValuesYaml: "replicaCount: 2\n",
  createdAt: "2026-08-09T00:00:00Z",
};
const nextApproval = {
  ...approval,
  id: "44444444-4444-4444-8444-444444444444",
  repository: "oci://registry.example.test/charts/orders",
  version: "2.0.0",
  defaultValuesYaml: "replicaCount: 1\n",
};
const revision = {
  id: "22222222-2222-4222-8222-222222222222",
  generation: 4,
  releaseName: "payments-production",
  action: "update" as const,
  desiredEnabled: true,
  approval: { id: approval.id, revision: approval.revision },
  valuesDigest: `sha256:${"f".repeat(64)}`,
  intentDigest: `sha256:${"1".repeat(64)}`,
  requestId: "request-helm-panel",
  createdAt: "2026-08-09T00:05:00Z",
};
const status: HelmReleaseStatus = {
  revision,
  phase: "application-pending",
  renderState: "succeeded",
  payloadState: "ready",
  applicationState: "pending",
};
const capabilities: Capability[] = [
  {
    scopeType: "environment",
    scopeId: environment.id,
    actions: ["helm.read", "helm.deploy", "helm.retry", "helm.rollback"],
  },
];

beforeEach(() => {
  vi.spyOn(api, "helmApprovals").mockResolvedValue({ items: [approval] });
  vi.spyOn(api, "helmRelease").mockResolvedValue(status);
  vi.spyOn(api, "helmReleaseHistory").mockResolvedValue({
    items: [
      status,
      {
        ...status,
        revision: {
          ...revision,
          id: "33333333-3333-4333-8333-333333333333",
          generation: 3,
        },
        phase: "published",
      },
    ],
  });
  vi.spyOn(api, "helmRenderedPreview").mockResolvedValue({
    releaseRevisionId: revision.id,
    generation: revision.generation,
    manifestDigest: `sha256:${"3".repeat(64)}`,
    inventoryDigest: `sha256:${"4".repeat(64)}`,
    resourceCount: 1,
    previewBytes: 37,
    resources: [
      {
        apiVersion: "apps/v1",
        kind: "Deployment",
        namespace: environment.namespace,
        name: "payments",
        sanitizedYaml: "apiVersion: apps/v1\nkind: Deployment\n",
        previewOmitted: false,
      },
    ],
  });
  vi.spyOn(api, "previewHelmValues").mockResolvedValue({
    approval: { id: approval.id, revision: approval.revision },
    normalizedValuesYaml: "replicaCount: 3\n",
    valuesDigest: `sha256:${"2".repeat(64)}`,
    currentValuesDigest: undefined,
    effectiveValues: { replicaCount: 3 },
    changedPaths: ["/replicaCount"],
  });
  vi.spyOn(api, "upsertHelmRelease").mockResolvedValue({
    revision,
    replayed: false,
  });
  vi.spyOn(api, "retryHelmRelease").mockResolvedValue({
    revision: { ...revision, action: "retry" },
    replayed: false,
  });
  vi.spyOn(api, "disableHelmRelease").mockResolvedValue({
    revision: { ...revision, action: "disable" },
    replayed: false,
  });
  vi.spyOn(api, "rollbackHelmRelease").mockResolvedValue({
    revision: { ...revision, action: "rollback" },
    replayed: false,
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPanel(
  options: { humanSession?: boolean; grants?: Capability[] } = {},
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <HelmApplicationsPanel
        application={application}
        environment={environment}
        project={project}
        capabilities={options.grants ?? capabilities}
        featureEnabled
        rollbackFeatureEnabled
        humanSession={options.humanSession ?? true}
      />
    </QueryClientProvider>,
  );
}

describe("Helm application panel", () => {
  it("requires preview before a human submits the closed values input", async () => {
    const user = userEvent.setup();
    renderPanel();
    const editor = await screen.findByLabelText("Helm values YAML");
    const deploy = screen.getByRole("button", {
      name: "Create update revision",
    });
    expect(deploy).toBeDisabled();
    await user.clear(editor);
    await user.type(editor, "replicaCount: 3");
    await user.click(screen.getByRole("button", { name: "Validate values" }));
    expect(await screen.findByText("Values validated")).toBeVisible();
    expect(deploy).toBeEnabled();
    await user.click(deploy);
    await waitFor(() => expect(api.upsertHelmRelease).toHaveBeenCalledOnce());
    expect(api.upsertHelmRelease).toHaveBeenCalledWith(
      application.id,
      environment.id,
      {
        approvalId: approval.id,
        approvalRevision: approval.revision,
        valuesYaml: "replicaCount: 3",
      },
      expect.any(String),
    );
    expect(await screen.findByText("Desired intent accepted")).toBeVisible();
    await waitFor(() =>
      expect(api.helmRenderedPreview).toHaveBeenCalledTimes(2),
    );
  });

  it("does not mark a newer draft approved when an older preview finishes late", async () => {
    const user = userEvent.setup();
    let resolvePreview:
      | ((value: Awaited<ReturnType<typeof api.previewHelmValues>>) => void)
      | undefined;
    vi.mocked(api.previewHelmValues).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolvePreview = resolve;
        }),
    );
    renderPanel();
    const editor = await screen.findByLabelText("Helm values YAML");
    const deploy = screen.getByRole("button", {
      name: "Create update revision",
    });
    await user.clear(editor);
    await user.type(editor, "replicaCount: 3");
    await user.click(screen.getByRole("button", { name: "Validate values" }));
    await user.clear(editor);
    await user.type(editor, "replicaCount: 4");
    resolvePreview?.({
      approval: { id: approval.id, revision: approval.revision },
      normalizedValuesYaml: "replicaCount: 3\n",
      valuesDigest: `sha256:${"2".repeat(64)}`,
      currentValuesDigest: undefined,
      effectiveValues: { replicaCount: 3 },
      changedPaths: ["/replicaCount"],
    });
    await waitFor(() => expect(deploy).toBeDisabled());
    expect(screen.queryByText("Values validated")).not.toBeInTheDocument();
  });

  it("resets scoped approval and destructive state when the target changes", async () => {
    const user = userEvent.setup();
    vi.mocked(api.helmApprovals).mockImplementation(
      async (applicationId, environmentId) => ({
        items:
          applicationId === nextApplication.id &&
          environmentId === nextEnvironment.id
            ? [nextApproval]
            : [approval],
      }),
    );
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const view = render(
      <QueryClientProvider client={client}>
        <HelmApplicationsPanel
          application={application}
          environment={environment}
          project={project}
          capabilities={[
            ...capabilities,
            {
              scopeType: "environment",
              scopeId: nextEnvironment.id,
              actions: ["helm.read", "helm.deploy", "helm.rollback"],
            },
          ]}
          featureEnabled
          rollbackFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    const editor = await screen.findByLabelText("Helm values YAML");
    expect(editor).toHaveValue(approval.defaultValuesYaml);
    await user.clear(editor);
    await user.type(editor, "replicaCount: 9");
    view.rerender(
      <QueryClientProvider client={client}>
        <HelmApplicationsPanel
          application={nextApplication}
          environment={nextEnvironment}
          project={project}
          capabilities={[
            ...capabilities,
            {
              scopeType: "environment",
              scopeId: nextEnvironment.id,
              actions: ["helm.read", "helm.deploy", "helm.rollback"],
            },
          ]}
          featureEnabled
          rollbackFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getByLabelText("Helm values YAML")).toHaveValue(
        nextApproval.defaultValuesYaml,
      ),
    );
    expect(
      screen.queryByDisplayValue("replicaCount: 9"),
    ).not.toBeInTheDocument();
  });

  it("does not reuse a late preview completion after the target changes", async () => {
    const user = userEvent.setup();
    let resolvePreview:
      | ((value: Awaited<ReturnType<typeof api.previewHelmValues>>) => void)
      | undefined;
    vi.mocked(api.helmApprovals).mockResolvedValue({ items: [approval] });
    vi.mocked(api.previewHelmValues).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolvePreview = resolve;
        }),
    );
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const grants = [
      ...capabilities,
      {
        scopeType: "environment" as const,
        scopeId: nextEnvironment.id,
        actions: ["helm.read", "helm.deploy", "helm.rollback"],
      },
    ];
    const view = render(
      <QueryClientProvider client={client}>
        <HelmApplicationsPanel
          application={application}
          environment={environment}
          project={project}
          capabilities={grants}
          featureEnabled
          rollbackFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    const editor = await screen.findByLabelText("Helm values YAML");
    await user.clear(editor);
    await user.type(editor, "replicaCount: 3");
    await user.click(screen.getByRole("button", { name: "Validate values" }));
    view.rerender(
      <QueryClientProvider client={client}>
        <HelmApplicationsPanel
          application={nextApplication}
          environment={nextEnvironment}
          project={project}
          capabilities={grants}
          featureEnabled
          rollbackFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    const deploy = await screen.findByRole("button", {
      name: "Create update revision",
    });
    resolvePreview?.({
      approval: { id: approval.id, revision: approval.revision },
      normalizedValuesYaml: "replicaCount: 3\n",
      valuesDigest: `sha256:${"2".repeat(64)}`,
      currentValuesDigest: undefined,
      effectiveValues: { replicaCount: 3 },
      changedPaths: ["/replicaCount"],
    });
    await waitFor(() => expect(deploy).toBeDisabled());
  });

  it("does not show a late mutation completion after the target changes", async () => {
    const user = userEvent.setup();
    let resolveUpsert:
      | ((value: Awaited<ReturnType<typeof api.upsertHelmRelease>>) => void)
      | undefined;
    vi.mocked(api.upsertHelmRelease).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveUpsert = resolve;
        }),
    );
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const grants = [
      ...capabilities,
      {
        scopeType: "environment" as const,
        scopeId: nextEnvironment.id,
        actions: ["helm.read", "helm.deploy", "helm.rollback"],
      },
    ];
    const view = render(
      <QueryClientProvider client={client}>
        <HelmApplicationsPanel
          application={application}
          environment={environment}
          project={project}
          capabilities={grants}
          featureEnabled
          rollbackFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    const editor = await screen.findByLabelText("Helm values YAML");
    await user.clear(editor);
    await user.type(editor, "replicaCount: 3");
    await user.click(screen.getByRole("button", { name: "Validate values" }));
    await screen.findByText("Values validated");
    await user.click(
      screen.getByRole("button", { name: "Create update revision" }),
    );
    view.rerender(
      <QueryClientProvider client={client}>
        <HelmApplicationsPanel
          application={nextApplication}
          environment={nextEnvironment}
          project={project}
          capabilities={grants}
          featureEnabled
          rollbackFeatureEnabled
          humanSession
        />
      </QueryClientProvider>,
    );
    await screen.findByRole("button", { name: "Create update revision" });
    resolveUpsert?.({ revision, replayed: false });
    await waitFor(() =>
      expect(
        screen.queryByText("Desired intent accepted"),
      ).not.toBeInTheDocument(),
    );
  });

  it("keeps a newer rollback selection after an older rollback completes", async () => {
    const user = userEvent.setup();
    const secondHistoryRevision = {
      ...revision,
      id: "55555555-5555-4555-8555-555555555555",
      generation: 2,
    };
    vi.mocked(api.helmReleaseHistory).mockResolvedValue({
      items: [
        status,
        {
          ...status,
          revision: {
            ...revision,
            id: "33333333-3333-4333-8333-333333333333",
            generation: 3,
          },
          phase: "published",
        },
        {
          ...status,
          revision: secondHistoryRevision,
          phase: "published",
        },
      ],
    });
    let resolveRollback!: (value: HelmMutationResult) => void;
    vi.mocked(api.rollbackHelmRelease).mockImplementationOnce(
      () => new Promise((resolve) => (resolveRollback = resolve)),
    );
    renderPanel();

    await screen.findByText("Immutable history");
    const rollbackButtons = await screen.findAllByRole("button", {
      name: "Roll back to this revision",
    });
    await user.click(rollbackButtons[0]!);
    await user.click(
      screen.getByRole("checkbox", {
        name: /I understand this publishes a new protected Git intent/i,
      }),
    );
    await user.click(screen.getByRole("button", { name: "Confirm rollback" }));
    await waitFor(() => expect(api.rollbackHelmRelease).toHaveBeenCalledOnce());

    await user.click(rollbackButtons[1]!);
    resolveRollback({
      revision: { ...revision, action: "rollback" },
      replayed: false,
    });

    await waitFor(() =>
      expect(
        screen.getByText("Rollback creates a new desired revision"),
      ).toBeVisible(),
    );
  });

  it("shows truthful render/publication state and only redacted inventory", async () => {
    renderPanel();
    expect(await screen.findByText("Phase 1 · offline render")).toBeVisible();
    expect(
      screen.getByText("Phase 2 · protected Git publication"),
    ).toBeVisible();
    expect(
      screen.getByText(
        "This is desired-state publication, not rollout health.",
      ),
    ).toBeVisible();
    expect(await screen.findByText("Deployment · payments")).toBeVisible();
    expect(screen.getByText(/1 resource\(s\)/)).toBeVisible();
    expect(screen.queryByLabelText(/renderer/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/credential/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/release name/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/namespace/i)).not.toBeInTheDocument();
  });

  it("explains the fail-closed recovery for an already-absent Application path", async () => {
    vi.mocked(api.helmRelease).mockResolvedValue({
      ...status,
      revision: { ...revision, action: "disable", desiredEnabled: false },
      phase: "failed",
      cascadeState: "failed",
      failureCode: "cascade-path-absent-recovery-required",
    });
    renderPanel();
    expect(
      await screen.findByText("Disable recovery requires an explicit rollback"),
    ).toBeVisible();
    expect(
      screen.getByText(/wait until it is published, then disable it again/i),
    ).toBeVisible();
  });

  it("keeps service-account sessions read-only despite mutation permissions", async () => {
    renderPanel({ humanSession: false });
    expect(
      await screen.findByText("Read-only automation identity"),
    ).toBeVisible();
    expect(
      screen.queryByRole("button", { name: "Create update revision" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Retry as new revision" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Disable desired release/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Roll back to this revision/i }),
    ).not.toBeInTheDocument();
  });

  it("renders nothing without exact helm.read permission", () => {
    renderPanel({ grants: [] });
    expect(api.helmApprovals).not.toHaveBeenCalled();
    expect(
      screen.queryByText("Approved external Helm"),
    ).not.toBeInTheDocument();
  });
});
