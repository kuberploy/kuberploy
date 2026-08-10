import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ComponentProps } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type { Application, Deployment } from "../api/types";
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
  vi.restoreAllMocks();
});

describe("deployment ConfigEditor preview binding", () => {
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
    expect(await screen.findByLabelText(/^DNS integration/)).toHaveTextContent(
      "Public DNS",
    );
    expect(screen.getByText(/External DNS revision is ready/i)).toBeVisible();
  });

  it("keeps Guided and Advanced YAML inspectable but immutable without scoped config write", async () => {
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
});
