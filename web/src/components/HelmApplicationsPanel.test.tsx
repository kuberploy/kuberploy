import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Capability, HelmReleaseRevision } from "../api/types";
import { HelmApplicationsPanel } from "./HelmApplicationsPanel";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const application = { id: "application-payments", projectId: "project-payments", name: "Valkey", sourceKind: "helm" as const };
const environment = { id: "environment-production", projectId: "project-payments", name: "Production", namespace: "payments-production" };
const project = { id: "project-payments", name: "Payments", teamId: "team-commerce" };
const capabilities: Capability[] = [{ scopeType: "environment", scopeId: environment.id, actions: ["helm.read", "helm.deploy", "helm.retry", "helm.rollback"] }];
const revision: HelmReleaseRevision = {
  id: "22222222-2222-4222-8222-222222222222",
  generation: 1,
  releaseName: "valkey",
  action: "deploy",
  desiredEnabled: true,
  source: { kind: "git", repositoryUrl: "https://github.com/valkey-io/valkey-helm.git", targetRevision: "main", path: "valkey" },
  valuesYaml: "replicaCount: 1\n",
  valuesDigest: `sha256:${"a".repeat(64)}`,
  state: "applied",
  requestId: "request",
  createdAt: "2026-08-25T00:00:00Z",
  updatedAt: "2026-08-25T00:00:01Z",
};

function renderPanel() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <HelmApplicationsPanel application={application} environment={environment} project={project} capabilities={capabilities} featureEnabled rollbackFeatureEnabled humanSession />
    </QueryClientProvider>,
  );
}

describe("direct Helm App panel", () => {
  it("edits source and values without an approval step", async () => {
    vi.spyOn(api, "helmRelease").mockResolvedValue(revision);
    vi.spyOn(api, "helmReleaseHistory").mockResolvedValue({ items: [revision] });
    const save = vi.spyOn(api, "upsertHelmRelease").mockResolvedValue({ revision, replayed: false });
    renderPanel();
    expect(await screen.findByText("Helm source")).toBeVisible();
    expect(screen.queryByText(/approval/i)).not.toBeInTheDocument();
    const editor = screen.getByLabelText("Helm values YAML");
    await userEvent.clear(editor);
    await userEvent.type(editor, "replicaCount: 2");
    await userEvent.click(screen.getByRole("button", { name: "Update App" }));
    await waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save.mock.calls[0]?.[2]).toMatchObject({ source: revision.source, valuesYaml: "replicaCount: 2" });
  });

  it("uses a shadcn confirmation dialog for disable", async () => {
    vi.spyOn(api, "helmRelease").mockResolvedValue(revision);
    vi.spyOn(api, "helmReleaseHistory").mockResolvedValue({ items: [revision] });
    renderPanel();
    await userEvent.click(await screen.findByRole("button", { name: "Disable App" }));
    expect(screen.getByRole("alertdialog")).toBeVisible();
  });
});
