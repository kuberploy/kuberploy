import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Operation, VariableSetSnapshot } from "../api/types";
import { VariableSetsView } from "./VariableSetsPage";

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("@tanstack/react-router")>();
  return {
    ...actual,
    Link: ({ children }: PropsWithChildren) => <a href="/test">{children}</a>,
  };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderView() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <VariableSetsView environmentId="environment-safe" />
    </QueryClientProvider>,
  );
}

const environment = {
  id: "environment-safe",
  projectId: "project-safe",
  name: "Backoffice",
  namespace: "backoffice",
  protectionPolicy: "protected" as const,
};

describe("VariableSet management", () => {
  it("keeps raw sources lossless and uses the exact preview without publication controls", async () => {
    const rawYaml =
      'apiVersion: variables.kuberploy.io/v1alpha1\nkind: VariableSet\n# keep ordering and comments\nvalues:\n  FEATURE_FLAG: "true"\n  REGION: "ap-southeast-1"\n';
    const project: VariableSetSnapshot = {
      scope: "project",
      bindingId: "binding-safe",
      projectId: "project-safe",
      environmentId: "environment-safe",
      path: "tenants/project-safe/variables.yaml",
      present: true,
      etag: '"sha256:' + "a".repeat(64) + '"',
      rawYaml,
      document: { kind: "VariableSet" },
      indexedRevision: "b".repeat(40),
    };
    const environmentSource: VariableSetSnapshot = {
      ...project,
      scope: "environment",
      path: "tenants/project-safe/environments/environment-safe/variables.yaml",
      present: false,
      etag: undefined,
      rawYaml: undefined,
    };
    vi.spyOn(api, "environment").mockResolvedValue(environment);
    vi.spyOn(api, "variableSets").mockResolvedValue({
      items: [project, environmentSource],
    });
    vi.spyOn(api, "me").mockResolvedValue({
      id: "admin-safe",
      displayName: "Admin",
      role: "project-admin",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          scopeType: "project",
          scopeId: "project-safe",
          actions: ["deployment-config:write"],
        },
      ],
    });
    const projects = vi.spyOn(api, "projects");
    const preview = vi.spyOn(api, "previewVariableSet").mockResolvedValue({
      previewToken: "p".repeat(43),
      scope: "project",
      path: project.path,
      gitDiff: '+  FEATURE_FLAG: "true"',
      document: { kind: "VariableSet" },
      diagnostics: [],
      expiresAt: "2026-08-09T01:00:00Z",
    });
    const operation: Operation = {
      id: "operation-variable-save",
      kind: "variable-set.git-write",
      status: "queued",
      state: "queued",
      targetType: "project",
      targetId: "project-safe",
      requestId: "request-safe",
      generation: 1,
      progress: [{ name: "git-write", status: "pending" }],
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:00:00Z",
    };
    const save = vi.spyOn(api, "saveVariableSet").mockResolvedValue(operation);
    renderView();

    const heading = await screen.findByRole("heading", {
      name: "Project variables",
    });
    const card = heading.closest("section");
    if (!card) throw new Error("project variable card missing");
    const projectEditor = within(card);
    expect(
      projectEditor.getByRole("textbox", { name: "Project variables YAML" }),
    ).toHaveValue(rawYaml);
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.getAllByText(/fixed by environment policy/i)).toHaveLength(2);
    expect(screen.getByText(/may affect every environment/i)).toBeVisible();

    await userEvent.click(
      projectEditor.getByRole("button", { name: /Preview Git diff/i }),
    );
    await waitFor(() => expect(preview).toHaveBeenCalledOnce());
    expect(preview).toHaveBeenCalledWith(
      "environment-safe",
      "project",
      rawYaml,
      project.etag,
      project.path,
    );
    expect(
      await projectEditor.findByLabelText("Git diff preview"),
    ).toHaveTextContent(/FEATURE_FLAG: "true"/);

    await userEvent.click(
      projectEditor.getByRole("button", { name: /Save through Git/i }),
    );
    await waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledWith(
      "environment-safe",
      "project",
      rawYaml,
      "p".repeat(43),
      expect.any(String),
    );
    expect(
      await projectEditor.findByText("Git operation accepted"),
    ).toBeVisible();
    expect(projects).not.toHaveBeenCalled();
  });

  it("keeps environment-scoped readers inspect-only without loading the parent project", async () => {
    const source = {
      bindingId: "binding-safe",
      projectId: "project-safe",
      environmentId: "environment-safe",
      present: false,
      indexedRevision: "b".repeat(40),
    };
    vi.spyOn(api, "environment").mockResolvedValue(environment);
    vi.spyOn(api, "variableSets").mockResolvedValue({
      items: [
        {
          ...source,
          scope: "project",
          path: "tenants/project-safe/variables.yaml",
        },
        {
          ...source,
          scope: "environment",
          path: "tenants/project-safe/environments/environment-safe/variables.yaml",
        },
      ],
    });
    vi.spyOn(api, "me").mockResolvedValue({
      id: "reader-safe",
      displayName: "Reader",
      role: "viewer",
      authentication: { kind: "session" },
    });
    vi.spyOn(api, "capabilities").mockResolvedValue({
      capabilities: [
        {
          scopeType: "environment",
          scopeId: "environment-safe",
          actions: ["deployment-config:read"],
        },
      ],
    });
    const projects = vi.spyOn(api, "projects");
    const preview = vi.spyOn(api, "previewVariableSet");
    const save = vi.spyOn(api, "saveVariableSet");
    renderView();

    expect(await screen.findAllByText(/Read-only:/i)).toHaveLength(2);
    expect(screen.getAllByRole("textbox")).toHaveLength(2);
    expect(screen.getAllByRole("textbox")[0]).toHaveAttribute("readonly");
    expect(
      screen.queryByRole("button", { name: /Preview Git diff/i }),
    ).toBeNull();
    expect(
      screen.queryByRole("button", { name: /Save through Git/i }),
    ).toBeNull();
    expect(projects).not.toHaveBeenCalled();
    expect(preview).not.toHaveBeenCalled();
    expect(save).not.toHaveBeenCalled();
  });
});
