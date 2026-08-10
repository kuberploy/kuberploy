import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Capability, HelmReleaseStatus } from "../api/types";
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
