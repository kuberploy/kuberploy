import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import { NewDeploymentPage } from "./NewDeploymentPage";

const router = vi.hoisted(() => ({ navigate: vi.fn() }));

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
}));

function wrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
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
  vi.spyOn(api, "capabilities").mockResolvedValue({
    features: { secretBindings: false, git: true, argo: true },
    capabilities: [],
  });
  router.navigate.mockReset();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("new deployment runtime controls", () => {
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
    await user.click(screen.getByRole("button", { name: /commit & deploy/i }));

    await waitFor(() => expect(createDeployment).toHaveBeenCalledOnce());
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
