import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { NewDeploymentPage } from "./NewDeploymentPage";

const router = vi.hoisted(() => ({
  navigate: vi.fn(),
  search: {
    projectId: undefined as string | undefined,
    environmentId: undefined as string | undefined,
    applicationId: undefined as string | undefined,
  },
}));

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    className,
  }: PropsWithChildren<{ to: string; className?: string }>) => (
    <a href={to} className={className}>
      {children}
    </a>
  ),
  useNavigate: () => router.navigate,
  useSearch: () => router.search,
}));

function wrapper(
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  }),
) {
  return ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

beforeEach(() => {
  vi.spyOn(api, "projects").mockResolvedValue({
    items: [{ id: "project-1", name: "Payments" }],
  });
  vi.spyOn(api, "environments").mockResolvedValue({
    items: [
      {
        id: "environment-1",
        projectId: "project-1",
        name: "Production",
        namespace: "payments-production",
      },
    ],
  });
  vi.spyOn(api, "applications").mockResolvedValue({
    items: [
      { id: "application-1", projectId: "project-1", name: "Payments API" },
    ],
  });
  vi.spyOn(api, "deployments").mockResolvedValue({ items: [] });
  vi.spyOn(api, "capabilities").mockResolvedValue({
    features: { secretBindings: false, git: true, argo: true },
    capabilities: [],
  });
  router.navigate.mockReset();
  router.search.projectId = undefined;
  router.search.environmentId = undefined;
  router.search.applicationId = undefined;
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("new deployment runtime controls", () => {
  it("starts in the exact project environment selected by Add App", async () => {
    router.search.projectId = "project-1";
    router.search.environmentId = "environment-1";

    render(<NewDeploymentPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("heading", { name: "Add App from OCI image" }),
    ).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByRole("combobox", { name: "Project" })).toHaveValue(
        "project-1",
      );
      expect(
        screen.getByRole("combobox", { name: /^Environment/ }),
      ).toHaveValue("environment-1");
    });
  });

  it("loads the current Git bundle ETag before updating an existing deployment", async () => {
    const user = userEvent.setup();
    const etag = `"sha256:${"a".repeat(64)}"`;
    vi.mocked(api.deployments).mockResolvedValue({
      items: [
        {
          id: "deployment-1",
          applicationId: "application-1",
          environmentId: "environment-1",
          runtime: {
            replicas: 1,
            ports: [{ name: "http", containerPort: 3000, protocol: "TCP" }],
            resources: { requests: { cpu: "50m", memory: "100Mi" } },
          },
        },
      ],
    });
    const deploymentConfig = vi
      .spyOn(api, "deploymentConfig")
      .mockResolvedValue({
        kind: "ConfigBundle",
        etag,
        targetHeadRevision: "a".repeat(40),
        indexedRevision: "b".repeat(40),
        configRevision: "c".repeat(40),
        freshness: "fresh",
        documents: [],
      });
    const createDeployment = vi
      .spyOn(api, "createDeployment")
      .mockResolvedValue({
        id: "operation-1",
        kind: "deployment.create",
        status: "queued",
        state: "queued",
        targetType: "deployment",
        targetId: "deployment-2",
        requestId: "request-1",
        generation: 1,
        progress: [],
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    await user.type(
      screen.getByRole("textbox", { name: /^Image digest/ }),
      `ghcr.io/acme/payments@sha256:${"d".repeat(64)}`,
    );

    await waitFor(() =>
      expect(deploymentConfig).toHaveBeenCalledWith("deployment-1"),
    );
    const submit = screen.getByRole("button", { name: /commit & deploy/i });
    await waitFor(() => expect(submit).toBeEnabled());
    await user.click(submit);

    await waitFor(() => expect(createDeployment).toHaveBeenCalledOnce());
    expect(createDeployment.mock.calls[0]?.[2]).toBe(etag);
  });

  it("reserves a durable application before enabling first-deployment scope", async () => {
    const user = userEvent.setup();
    const createApplication = vi
      .spyOn(api, "createApplication")
      .mockResolvedValue({
        id: "application-new",
        projectId: "project-1",
        name: "New API",
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.type(
      screen.getByRole("textbox", { name: /^Application name/ }),
      "New API",
    );
    await user.click(
      screen.getByRole("button", { name: "Create application identity" }),
    );

    await screen.findByText("Application identity created");
    expect(createApplication).toHaveBeenCalledWith(
      { projectId: "project-1", name: "New API" },
      expect.any(String),
    );
    expect(
      screen.getByRole("link", { name: "Source options" }),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("radio", { name: "New application" }));
    await user.click(
      screen.getByRole("button", { name: "Create application identity" }),
    );
    await waitFor(() => expect(createApplication).toHaveBeenCalledTimes(2));
    expect(createApplication.mock.calls[1]?.[1]).not.toBe(
      createApplication.mock.calls[0]?.[1],
    );
  });

  it("ignores a stale application reservation after the draft changes", async () => {
    const user = userEvent.setup();
    let resolveApplication:
      | ((value: { id: string; projectId: string; name: string }) => void)
      | undefined;
    const createApplication = vi
      .spyOn(api, "createApplication")
      .mockImplementation(
        () =>
          new Promise((resolve) => {
            resolveApplication = resolve;
          }),
      );
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    const name = screen.getByRole("textbox", { name: /^Application name/ });
    await user.type(name, "New API");
    await user.click(
      screen.getByRole("button", { name: "Create application identity" }),
    );
    await waitFor(() => expect(createApplication).toHaveBeenCalledOnce());

    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    resolveApplication?.({
      id: "application-stale",
      projectId: "project-1",
      name: "New API",
    });

    await waitFor(() => expect(createApplication).toHaveReturned());
    expect(
      screen.getByRole("radio", { name: "Existing application" }),
    ).toBeChecked();
    expect(screen.getByRole("combobox", { name: "Application" })).toHaveValue(
      "",
    );
    expect(
      screen.queryByText("Application identity created"),
    ).not.toBeInTheDocument();
  });

  it("clears an environment removed by an authorization refresh", async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    render(<NewDeploymentPage />, { wrapper: wrapper(queryClient) });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    const environment = screen.getByRole("combobox", {
      name: /^Environment/,
    });
    await user.selectOptions(environment, "environment-1");

    vi.mocked(api.environments).mockResolvedValueOnce({ items: [] });
    await queryClient.invalidateQueries({ queryKey: ["environments"] });

    await waitFor(() => expect(environment).toHaveValue(""));
  });

  it("requires a current safe tag-to-digest preview and still submits only the caller tag", async () => {
    const user = userEvent.setup();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        secretBindings: false,
        git: true,
        argo: true,
        imageTagResolution: true,
      },
      capabilities: [],
    });
    const immutableImage = `registry.example.test/payments/api@sha256:${"c".repeat(64)}`;
    const previewImageResolution = vi
      .spyOn(api, "previewImageResolution")
      .mockResolvedValue({
        requestedImage: "registry.example.test/payments/api:release",
        immutableImage,
        resolved: true,
      });
    const createDeployment = vi
      .spyOn(api, "createDeployment")
      .mockResolvedValue({
        id: "operation-tag",
        kind: "deployment.create",
        status: "queued",
        state: "queued",
        targetType: "deployment",
        targetId: "deployment-tag",
        requestId: "request-tag",
        generation: 1,
        progress: [],
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    const image = screen.getByRole("textbox", { name: /^Image digest/ });
    await user.type(image, "registry.example.test/payments/api:release");

    const submit = screen.getByRole("button", { name: /commit & deploy/i });
    expect(submit).toBeDisabled();
    await user.click(
      screen.getByRole("button", { name: "Resolve tag to digest" }),
    );
    await waitFor(() =>
      expect(previewImageResolution).toHaveBeenCalledWith(
        "environment-1",
        "application-1",
        "registry.example.test/payments/api:release",
      ),
    );
    expect(await screen.findByText(immutableImage)).toBeVisible();
    expect(submit).toBeEnabled();

    await user.click(submit);
    await waitFor(() => expect(createDeployment).toHaveBeenCalledOnce());
    expect(createDeployment.mock.calls[0]?.[0].image).toBe(
      "registry.example.test/payments/api:release",
    );
    expect(createDeployment.mock.calls[0]?.[0].expectedImmutableImage).toBe(
      immutableImage,
    );
    expect(createDeployment.mock.calls[0]?.[0]).not.toHaveProperty("targetId");
    expect(createDeployment.mock.calls[0]?.[0]).not.toHaveProperty(
      "credentialRef",
    );
  });

  it("fails closed when a resolved tag preview becomes stale", async () => {
    const user = userEvent.setup();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        secretBindings: false,
        git: true,
        argo: true,
        imageTagResolution: true,
      },
      capabilities: [],
    });
    vi.spyOn(api, "previewImageResolution").mockResolvedValue({
      requestedImage: "registry.example.test/payments/api:release",
      immutableImage: `registry.example.test/payments/api@sha256:${"d".repeat(64)}`,
      resolved: true,
    });
    const createDeployment = vi.spyOn(api, "createDeployment");
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    const image = screen.getByRole("textbox", { name: /^Image digest/ });
    await user.type(image, "registry.example.test/payments/api:release");
    await user.click(
      screen.getByRole("button", { name: "Resolve tag to digest" }),
    );
    const submit = screen.getByRole("button", { name: /commit & deploy/i });
    await waitFor(() => expect(submit).toBeEnabled());

    await user.type(image, "-changed");
    expect(submit).toBeDisabled();
    await user.click(submit);
    expect(createDeployment).not.toHaveBeenCalled();
  });

  it("does not reuse a tag preview after changing deployment environment", async () => {
    const user = userEvent.setup();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        secretBindings: false,
        git: true,
        argo: true,
        imageTagResolution: true,
      },
      capabilities: [],
    });
    vi.mocked(api.environments).mockResolvedValue({
      items: [
        {
          id: "environment-1",
          projectId: "project-1",
          name: "Production",
          namespace: "payments-production",
        },
        {
          id: "environment-2",
          projectId: "project-1",
          name: "Staging",
          namespace: "payments-staging",
        },
      ],
    });
    const previewImageResolution = vi
      .spyOn(api, "previewImageResolution")
      .mockResolvedValue({
        requestedImage: "registry.example.test/payments/api:release",
        immutableImage: `registry.example.test/payments/api@sha256:${"e".repeat(64)}`,
        resolved: true,
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    const environment = screen.getByRole("combobox", {
      name: /^Environment/,
    });
    await user.selectOptions(environment, "environment-1");
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    const image = screen.getByRole("textbox", { name: /^Image digest/ });
    await user.type(image, "registry.example.test/payments/api:release");
    await user.click(
      screen.getByRole("button", { name: "Resolve tag to digest" }),
    );
    await waitFor(() => expect(previewImageResolution).toHaveBeenCalledOnce());
    expect(
      screen.getByRole("button", { name: /commit & deploy/i }),
    ).toBeEnabled();

    await user.selectOptions(environment, "environment-2");
    await user.selectOptions(environment, "environment-1");
    expect(
      screen.getByRole("button", { name: /commit & deploy/i }),
    ).toBeDisabled();
    expect(
      screen.queryByText(/Immutable image resolved/),
    ).not.toBeInTheDocument();
  });

  it("hides a tag-resolution error after the image changes", async () => {
    const user = userEvent.setup();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        secretBindings: false,
        git: true,
        argo: true,
        imageTagResolution: true,
      },
      capabilities: [],
    });
    vi.spyOn(api, "previewImageResolution").mockRejectedValueOnce(
      new Error("old image resolution failed"),
    );
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    const image = screen.getByRole("textbox", { name: /^Image digest/ });
    await user.type(image, "registry.example.test/payments/api:release");
    await user.click(
      screen.getByRole("button", { name: "Resolve tag to digest" }),
    );
    await screen.findByText("Tag could not be resolved");

    await user.type(image, "-changed");
    expect(
      screen.queryByText("Tag could not be resolved"),
    ).not.toBeInTheDocument();
  });

  it("does not promise or submit deployment while protected GitOps is unavailable", async () => {
    vi.mocked(api.capabilities).mockResolvedValue({
      features: { secretBindings: false, git: true, argo: false },
      capabilities: [],
    });
    const createApplication = vi.spyOn(api, "createApplication");
    const createDeployment = vi.spyOn(api, "createDeployment");
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    expect(
      await screen.findByText("Protected GitOps is not ready"),
    ).toBeVisible();
    const submit = screen.getByRole("button", { name: /commit & deploy/i });
    expect(submit).toBeDisabled();
    await userEvent.click(submit);
    expect(createApplication).not.toHaveBeenCalled();
    expect(createDeployment).not.toHaveBeenCalled();
  });

  it("blocks scalar container commands before any deployment side effect", async () => {
    const user = userEvent.setup();
    const createApplication = vi.spyOn(api, "createApplication");
    const createDeployment = vi.spyOn(api, "createDeployment");
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    const command = await screen.findByRole("textbox", {
      name: "Container command (YAML list)",
    });
    await user.clear(command);
    await user.type(command, "/bin/sh -c 'echo owned'");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      /YAML list, never a shell string/i,
    );
    const submit = screen.getByRole("button", { name: /commit & deploy/i });
    expect(submit).toBeDisabled();
    await user.click(submit);
    expect(createApplication).not.toHaveBeenCalled();
    expect(createDeployment).not.toHaveBeenCalled();
  });

  it("keeps probes optional and blocks malformed exec YAML locally", async () => {
    const user = userEvent.setup();
    const createDeployment = vi.spyOn(api, "createDeployment");
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    expect(
      await screen.findByRole("combobox", { name: "Readiness check" }),
    ).toHaveValue("disabled");
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Liveness check" }),
      "exec",
    );
    const command = screen.getByRole("textbox", {
      name: "Liveness exec arguments (YAML list)",
    });
    await user.clear(command);
    await user.type(command, "command: /bin/check");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      /exec command must be a YAML array/i,
    );
    const submit = screen.getByRole("button", { name: /commit & deploy/i });
    expect(submit).toBeDisabled();
    await user.click(submit);
    expect(createDeployment).not.toHaveBeenCalled();
  });

  it("sends a typed readiness probe with the existing-image deployment", async () => {
    const user = userEvent.setup();
    const createDeployment = vi
      .spyOn(api, "createDeployment")
      .mockRejectedValueOnce(new Error("response lost"))
      .mockResolvedValue({
        id: "operation-1",
        kind: "deployment.create",
        status: "queued",
        state: "queued",
        targetType: "deployment",
        targetId: "deployment-1",
        requestId: "request-1",
        generation: 1,
        progress: [],
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    await user.type(
      screen.getByRole("textbox", { name: /^Image digest/ }),
      `ghcr.io/acme/payments@sha256:${"a".repeat(64)}`,
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Readiness check" }),
      "httpGet",
    );
    const path = screen.getByRole("textbox", {
      name: "Readiness HTTP path",
    });
    await user.clear(path);
    await user.type(path, "/ready");
    await user.type(
      screen.getByRole("spinbutton", { name: "Readiness timeout" }),
      "2",
    );
    const command = screen.getByRole("textbox", {
      name: "Container command (YAML list)",
    });
    const args = screen.getByRole("textbox", {
      name: "Container arguments (YAML list)",
    });
    await user.clear(command);
    await user.type(command, "- /bin/server\n- argument with spaces");
    await user.clear(args);
    await user.type(args, '- --literal\n- "semi; $(id)"');
    await user.type(
      screen.getByRole("textbox", { name: "Container working directory" }),
      "/srv/payments",
    );
    await user.type(
      screen.getByRole("spinbutton", { name: "Termination grace period" }),
      "45",
    );
    await user.click(screen.getByRole("radio", { name: "Manual hostname" }));
    await user.type(
      screen.getByRole("textbox", { name: /^Hostname/ }),
      "payments.example.test",
    );
    const submit = screen.getByRole("button", { name: /commit & deploy/i });
    await user.click(submit);
    await screen.findByText("Deployment was not accepted");
    await user.click(submit);

    await waitFor(() => expect(createDeployment).toHaveBeenCalledTimes(2));
    expect(createDeployment.mock.calls[0]?.[1]).toBe(
      createDeployment.mock.calls[1]?.[1],
    );
    expect(createDeployment.mock.calls[0]?.[0].runtime).toMatchObject({
      command: ["/bin/server", "argument with spaces"],
      args: ["--literal", "semi; $(id)"],
      workingDirectory: "/srv/payments",
      terminationGracePeriodSeconds: 45,
    });
    expect(createDeployment.mock.calls[0]?.[0].runtime.probes).toEqual({
      readiness: {
        httpGet: { path: "/ready", port: "http" },
        timeoutSeconds: 2,
      },
    });
    expect(
      createDeployment.mock.calls[0]?.[0].runtime.resources.requests,
    ).toEqual({ cpu: "50m", memory: "100Mi" });
    expect(createDeployment.mock.calls[0]?.[0].route).toEqual({
      hostname: "payments.example.test",
      dnsMode: "manual",
      pathPrefix: "/",
      tlsMode: "httpOnly",
    });
  });

  it("does not navigate from a late success after the deployment draft changes", async () => {
    const user = userEvent.setup();
    let resolveDeployment:
      | ((value: Awaited<ReturnType<typeof api.createDeployment>>) => void)
      | undefined;
    const createDeployment = vi
      .spyOn(api, "createDeployment")
      .mockImplementation(
        () =>
          new Promise((resolve) => {
            resolveDeployment = resolve;
          }),
      );
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    const image = screen.getByRole("textbox", { name: /^Image digest/ });
    const firstImage = `ghcr.io/acme/payments@sha256:${"a".repeat(64)}`;
    const secondImage = `ghcr.io/acme/payments@sha256:${"b".repeat(64)}`;
    await user.type(image, firstImage);
    await user.click(screen.getByRole("button", { name: /commit & deploy/i }));
    await waitFor(() => expect(createDeployment).toHaveBeenCalledOnce());

    await user.clear(image);
    await user.type(image, secondImage);
    resolveDeployment?.({
      id: "operation-stale-draft",
      kind: "deployment.create",
      status: "queued",
      state: "queued",
      targetType: "deployment",
      targetId: "deployment-stale-draft",
      requestId: "request-stale-draft",
      generation: 1,
      progress: [],
      createdAt: "2026-08-16T00:00:00Z",
      updatedAt: "2026-08-16T00:00:00Z",
    });

    await waitFor(() => expect(router.navigate).not.toHaveBeenCalled());
    expect(image).toHaveValue(secondImage);
  });

  it("exposes scheduling in the first-deployment wizard and submits it", async () => {
    const user = userEvent.setup();
    const createDeployment = vi
      .spyOn(api, "createDeployment")
      .mockResolvedValue({
        id: "operation-scheduling",
        kind: "deployment.create",
        status: "queued",
        state: "queued",
        targetType: "deployment",
        targetId: "deployment-scheduling",
        requestId: "request-scheduling",
        generation: 1,
        progress: [],
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    await user.click(screen.getByRole("button", { name: "Add label" }));
    await user.type(
      screen.getByRole("textbox", { name: "Node selector 1 key" }),
      "workload",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Node selector 1 value" }),
      "high",
    );
    await user.type(
      screen.getByRole("textbox", { name: /^Image digest/ }),
      `ghcr.io/acme/payments@sha256:${"a".repeat(64)}`,
    );

    await user.click(screen.getByRole("button", { name: /commit & deploy/i }));
    await waitFor(() => expect(createDeployment).toHaveBeenCalledOnce());
    expect(createDeployment.mock.calls[0]?.[0].runtime.nodeSelector).toEqual({
      workload: "high",
    });
  });

  it("submits workload-specific StatefulSet controls from the first-deployment wizard", async () => {
    const user = userEvent.setup();
    const createDeployment = vi
      .spyOn(api, "createDeployment")
      .mockResolvedValue({
        id: "operation-stateful",
        kind: "deployment.create",
        status: "queued",
        state: "queued",
        targetType: "deployment",
        targetId: "deployment-stateful",
        requestId: "request-stateful",
        generation: 1,
        progress: [],
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Workload type" }),
      "StatefulSet",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^StatefulSet strategy/ }),
      "OnDelete",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Pod management policy" }),
      "Parallel",
    );
    await user.type(
      screen.getByRole("textbox", { name: /^Image digest/ }),
      `ghcr.io/acme/payments@sha256:${"e".repeat(64)}`,
    );

    await user.click(screen.getByRole("button", { name: /commit & deploy/i }));
    await waitFor(() => expect(createDeployment).toHaveBeenCalledOnce());
    expect(createDeployment.mock.calls[0]?.[0].runtime).toMatchObject({
      workloadType: "StatefulSet",
      strategy: { type: "OnDelete" },
      podManagementPolicy: "Parallel",
    });
  });

  it("clears app-scoped placement when the selected application changes", async () => {
    const user = userEvent.setup();
    vi.mocked(api.applications).mockResolvedValue({
      items: [
        { id: "application-1", projectId: "project-1", name: "Payments API" },
        { id: "application-2", projectId: "project-1", name: "Billing API" },
      ],
    });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    const application = screen.getByRole("combobox", { name: "Application" });
    await user.selectOptions(application, "application-1");
    await user.click(screen.getByRole("button", { name: "Add label" }));
    await user.type(
      screen.getByRole("textbox", { name: "Node selector 1 key" }),
      "workload",
    );
    await user.click(screen.getByRole("button", { name: "Add constraint" }));
    expect(
      screen.queryByText("No topology constraints."),
    ).not.toBeInTheDocument();

    await user.selectOptions(application, "application-2");
    expect(
      screen.queryByRole("textbox", { name: "Node selector 1 key" }),
    ).not.toBeInTheDocument();
    expect(await screen.findByText("No topology constraints.")).toBeVisible();
  });

  it("clears app-scoped values and route when the selected application changes", async () => {
    const user = userEvent.setup();
    vi.mocked(api.applications).mockResolvedValue({
      items: [
        { id: "application-1", projectId: "project-1", name: "Payments API" },
        { id: "application-2", projectId: "project-1", name: "Billing API" },
      ],
    });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    const application = screen.getByRole("combobox", { name: "Application" });
    await user.selectOptions(application, "application-1");
    await user.click(screen.getByRole("button", { name: "Add value" }));
    await user.type(
      screen.getByRole("textbox", { name: "Variable 1 name" }),
      "OLD_VALUE",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Variable 1 value" }),
      "old",
    );
    await user.click(screen.getByRole("radio", { name: "Manual hostname" }));
    await user.type(
      screen.getByRole("textbox", { name: /^Hostname/ }),
      "old.example.com",
    );

    await user.selectOptions(application, "application-2");

    await waitFor(() => {
      expect(
        screen.queryByRole("textbox", { name: "Variable 1 name" }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole("textbox", { name: /^Hostname/ }),
      ).not.toBeInTheDocument();
      expect(
        screen.getByRole("radio", { name: "Internal only" }),
      ).toBeChecked();
    });
  });

  it("clears the previous application draft when switching to a new identity", async () => {
    const user = userEvent.setup();
    vi.mocked(api.applications).mockResolvedValue({
      items: [
        { id: "application-1", projectId: "project-1", name: "Payments API" },
      ],
    });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );
    await user.click(screen.getByRole("button", { name: "Add label" }));
    await user.type(
      screen.getByRole("textbox", { name: "Node selector 1 key" }),
      "workload",
    );
    await user.click(screen.getByRole("button", { name: "Add value" }));
    await user.type(
      screen.getByRole("textbox", { name: "Variable 1 name" }),
      "OLD_VALUE",
    );
    await user.click(screen.getByRole("radio", { name: "Manual hostname" }));
    await user.type(
      screen.getByRole("textbox", { name: /^Hostname/ }),
      "old.example.com",
    );

    await user.click(screen.getByRole("radio", { name: "New application" }));

    await waitFor(() => {
      expect(
        screen.queryByRole("textbox", { name: "Node selector 1 key" }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole("textbox", { name: "Variable 1 name" }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole("textbox", { name: /^Hostname/ }),
      ).not.toBeInTheDocument();
      expect(
        screen.getByRole("radio", { name: "Internal only" }),
      ).toBeChecked();
    });

    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    expect(screen.getByRole("combobox", { name: "Application" })).toHaveValue(
      "",
    );
  });

  it("clears stale environment and application scope when the project changes", async () => {
    const user = userEvent.setup();
    vi.mocked(api.projects).mockResolvedValue({
      items: [
        { id: "project-1", name: "Payments" },
        { id: "project-2", name: "Billing" },
      ],
    });
    vi.mocked(api.environments).mockResolvedValue({
      items: [
        {
          id: "environment-1",
          projectId: "project-1",
          name: "Production",
          namespace: "payments-production",
        },
        {
          id: "environment-2",
          projectId: "project-2",
          name: "Production",
          namespace: "billing-production",
        },
      ],
    });
    vi.mocked(api.applications).mockResolvedValue({
      items: [
        { id: "application-1", projectId: "project-1", name: "Payments API" },
        { id: "application-2", projectId: "project-2", name: "Billing API" },
      ],
    });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    const environment = screen.getByRole("combobox", {
      name: /^Environment/,
    });
    await user.selectOptions(environment, "environment-1");
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    const application = screen.getByRole("combobox", { name: "Application" });
    await user.selectOptions(application, "application-1");
    await user.click(screen.getByRole("button", { name: "Add label" }));
    expect(
      screen.getByRole("textbox", { name: "Node selector 1 key" }),
    ).toBeInTheDocument();

    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-2",
    );

    await waitFor(() => {
      expect(environment).toHaveValue("");
      expect(application).toHaveValue("");
    });
    expect(
      screen.queryByRole("textbox", { name: "Node selector 1 key" }),
    ).not.toBeInTheDocument();
  });

  it("previews sslip server-side and submits only the closed route intent", async () => {
    const user = userEvent.setup();
    vi.mocked(api.capabilities).mockResolvedValue({
      features: {
        secretBindings: false,
        sslip: true,
        git: true,
        argo: true,
      },
      capabilities: [],
    });
    const applicationSSLIPHostname = vi
      .spyOn(api, "applicationSSLIPHostname")
      .mockResolvedValue({
        mode: "sslip",
        hostname: "kp-32f2a03ad59c10f96a1a.8-8-8-8.sslip.io",
        source: "service-ip",
        observedAt: "2026-08-09T10:00:00Z",
      });
    const createApplication = vi.spyOn(api, "createApplication");
    const createDeployment = vi
      .spyOn(api, "createDeployment")
      .mockResolvedValue({
        id: "operation-sslip",
        kind: "deployment.create",
        status: "queued",
        state: "queued",
        targetType: "deployment",
        targetId: "deployment-sslip",
        requestId: "request-sslip",
        generation: 1,
        progress: [],
        createdAt: "2026-08-09T00:00:00Z",
        updatedAt: "2026-08-09T00:00:00Z",
      });
    render(<NewDeploymentPage />, { wrapper: wrapper() });

    await screen.findByRole("option", { name: "Payments" });
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Project" }),
      "project-1",
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: /^Environment/ }),
      "environment-1",
    );
    await user.click(
      screen.getByRole("radio", { name: "Existing application" }),
    );
    await user.selectOptions(
      screen.getByRole("combobox", { name: "Application" }),
      "application-1",
    );

    await waitFor(() =>
      expect(applicationSSLIPHostname).toHaveBeenCalledWith(
        "application-1",
        "environment-1",
      ),
    );
    const sslipMode = screen.getByRole("radio", {
      name: "Free sslip.io hostname",
    });
    await waitFor(() => expect(sslipMode).toBeEnabled());
    await user.click(sslipMode);
    const hostname = screen.getByRole("textbox", {
      name: "sslip.io hostname",
    });
    expect(hostname).toHaveValue("kp-32f2a03ad59c10f96a1a.8-8-8-8.sslip.io");
    expect(hostname).toHaveAttribute("readonly");

    await user.type(
      screen.getByRole("textbox", { name: /^Image digest/ }),
      `ghcr.io/acme/payments@sha256:${"a".repeat(64)}`,
    );
    await user.click(screen.getByRole("button", { name: /commit & deploy/i }));

    await waitFor(() => expect(createDeployment).toHaveBeenCalledOnce());
    expect(createApplication).not.toHaveBeenCalled();
    const route = createDeployment.mock.calls[0]?.[0].route;
    expect(route).toEqual({
      dnsMode: "sslip",
      pathPrefix: "/",
      tlsMode: "httpOnly",
    });
    expect(route).not.toHaveProperty("hostname");
    expect(route).not.toHaveProperty("ip");
  });
});
