import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { defaultConfigYaml, guidedConfigFromYaml } from "../lib/configDraft";
import { GuidedConfigForm } from "./GuidedConfigForm";

afterEach(cleanup);

describe("guided runtime controls", () => {
  it("labels environment values as runtime-only input", () => {
    const initial = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    render(<GuidedConfigForm initial={initial} onChange={vi.fn()} />);

    expect(
      screen.getByRole("heading", { name: "Runtime environment values" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/never passed to image builds/i),
    ).toBeInTheDocument();
  });

  it("edits literal container argv and termination grace without shell parsing", async () => {
    const user = userEvent.setup();
    const initial = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    const onChange = vi.fn();
    render(<GuidedConfigForm initial={initial} onChange={onChange} />);

    const command = screen.getByRole("textbox", {
      name: "Container command (YAML list)",
    });
    const args = screen.getByRole("textbox", {
      name: "Container arguments (YAML list)",
    });
    expect(command).toHaveValue("[]");
    expect(args).toHaveValue("[]");

    await user.clear(command);
    await user.type(command, '- /bin/server\n- "argument with spaces"');
    await user.clear(args);
    await user.type(args, '- --literal\n- "semi; $(id)"');
    await user.type(
      screen.getByRole("spinbutton", { name: "Termination grace period" }),
      "30",
    );

    await waitFor(() => {
      const latest = onChange.mock.lastCall?.[0];
      expect(latest).toMatchObject({
        commandYaml: '- /bin/server\n- "argument with spaces"',
        argsYaml: '- --literal\n- "semi; $(id)"',
        terminationGracePeriodSeconds: 30,
      });
    });
  });

  it("shows unsafe scalar command input and invalid grace in the form", async () => {
    const user = userEvent.setup();
    const initial = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    render(<GuidedConfigForm initial={initial} onChange={vi.fn()} />);
    const command = screen.getByRole("textbox", {
      name: "Container command (YAML list)",
    });

    await user.clear(command);
    await user.type(command, "/bin/sh -c 'echo owned'");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      /YAML list, never a shell string/i,
    );

    await user.clear(command);
    fireEvent.change(command, { target: { value: "[]" } });
    await user.type(
      screen.getByRole("spinbutton", { name: "Termination grace period" }),
      "0",
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(
      /integer from 1 to 3600/i,
    );
  });

  it("starts with no probes and edits a typed HTTP readiness check", async () => {
    const user = userEvent.setup();
    const initial = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    const onChange = vi.fn();
    render(<GuidedConfigForm initial={initial} onChange={onChange} />);

    expect(screen.getByRole("combobox", { name: "Startup check" })).toHaveValue(
      "disabled",
    );
    expect(
      screen.getByRole("combobox", { name: "Readiness check" }),
    ).toHaveValue("disabled");
    expect(
      screen.getByRole("combobox", { name: "Liveness check" }),
    ).toHaveValue("disabled");

    await user.selectOptions(
      screen.getByRole("combobox", { name: "Readiness check" }),
      "httpGet",
    );
    const path = screen.getByRole("textbox", {
      name: "Readiness HTTP path",
    });
    expect(path).toHaveValue("/healthz");
    expect(screen.getByRole("textbox", { name: "Readiness port" })).toHaveValue(
      "http",
    );

    await user.clear(path);
    await user.type(path, "/ready");
    await user.type(
      screen.getByRole("spinbutton", { name: "Readiness period" }),
      "5",
    );

    await waitFor(() => {
      const latest = onChange.mock.lastCall?.[0];
      expect(latest?.probes.readiness).toMatchObject({
        mode: "httpGet",
        httpPath: "/ready",
        port: "http",
        periodSeconds: 5,
      });
    });
  });

  it("shows malformed exec command YAML while it is still in the form", async () => {
    const user = userEvent.setup();
    const initial = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    render(<GuidedConfigForm initial={initial} onChange={vi.fn()} />);

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
  });
});

describe("guided External DNS catalog", () => {
  const catalog = {
    items: [
      {
        id: "integration-1",
        slug: "public-dns",
        name: "Public DNS",
        mode: "managed" as const,
        providerKind: "cloudflare" as const,
        allowedDomainSuffixes: ["example.com"],
        runtimeAvailable: false,
      },
    ],
    truncated: false,
    configurationState: "configured" as const,
    controllerReadiness: "unobserved" as const,
    runtimeAvailable: false as const,
  };

  it("preserves an existing selection while stale and reports exact runtime unavailability", async () => {
    const initial = {
      ...guidedConfigFromYaml(defaultConfigYaml({ name: "api" })),
      host: "api.example.com",
      dnsMode: "externalDns" as const,
      dnsIntegrationRef: "public-dns",
    };
    render(
      <GuidedConfigForm
        initial={initial}
        onChange={vi.fn()}
        externalDNSCatalog={catalog}
        externalDNSRuntimeEnabled={false}
      />,
    );

    expect(screen.getByLabelText(/^Hostname/)).toHaveValue("api.example.com");
    expect(screen.getByLabelText(/^DNS integration/)).toHaveValue("public-dns");
    expect(screen.queryByPlaceholderText("cloudflare-primary")).toBeNull();
    expect(screen.getByLabelText(/^DNS integration/)).toBeDisabled();
    expect(
      screen.getByText(/External DNS runtime is not ready/i),
    ).toBeVisible();
    expect(
      screen.getByText(
        /selected External DNS integration revision is not freshly observed ready/i,
      ),
    ).toBeVisible();
  });

  it("requires an explicit integration before reporting an exactly ready runtime", async () => {
    const initial = {
      ...guidedConfigFromYaml(defaultConfigYaml({ name: "api" })),
      host: "api.example.com",
    };
    render(
      <GuidedConfigForm
        initial={initial}
        onChange={vi.fn()}
        externalDNSCatalog={{
          ...catalog,
          items: catalog.items.map((item) => ({
            ...item,
            runtimeAvailable: true,
          })),
          controllerReadiness: "ready",
          runtimeAvailable: true,
        }}
        externalDNSRuntimeEnabled
      />,
    );

    expect(screen.getByRole("radio", { name: /Automatic DNS/i })).toBeEnabled();
    await userEvent.click(
      screen.getByRole("radio", { name: /Automatic DNS/i }),
    );
    expect(
      screen.getByText("Select an External DNS integration"),
    ).toBeVisible();
    expect(
      screen.queryByText("External DNS revision is ready"),
    ).not.toBeInTheDocument();

    await userEvent.selectOptions(
      screen.getByLabelText(/^DNS integration/),
      "public-dns",
    );
    expect(screen.getByText("External DNS revision is ready")).toBeVisible();
  });

  it("keeps a new automatic-DNS selection disabled when the operational capability is off", () => {
    const initial = {
      ...guidedConfigFromYaml(defaultConfigYaml({ name: "api" })),
      host: "api.example.com",
    };
    render(
      <GuidedConfigForm
        initial={initial}
        onChange={vi.fn()}
        externalDNSCatalog={{
          ...catalog,
          items: catalog.items.map((item) => ({
            ...item,
            runtimeAvailable: true,
          })),
          controllerReadiness: "ready",
          runtimeAvailable: true,
        }}
        externalDNSRuntimeEnabled={false}
      />,
    );

    expect(
      screen.getByRole("radio", { name: /Automatic DNS/i }),
    ).toBeDisabled();
  });

  it("preserves an unavailable existing slug and reports catalog errors safely", () => {
    const initial = {
      ...guidedConfigFromYaml(defaultConfigYaml({ name: "api" })),
      host: "api.example.com",
      dnsMode: "externalDns" as const,
      dnsIntegrationRef: "removed-profile",
    };
    render(
      <GuidedConfigForm
        initial={initial}
        onChange={vi.fn()}
        externalDNSCatalog={{ ...catalog, items: [] }}
        externalDNSCatalogError="The authorized catalog could not be loaded."
        externalDNSRuntimeEnabled={false}
      />,
    );
    const integration = screen.getByLabelText(/^DNS integration/);
    expect(integration).toHaveValue("removed-profile");
    expect(integration).toBeDisabled();
    expect(
      screen.getByRole("option", { name: "removed-profile (unavailable)" }),
    ).toBeDisabled();
    expect(
      screen.getByText("The authorized catalog could not be loaded."),
    ).toBeVisible();
  });
});

describe("guided sslip.io hostname", () => {
  const preview = {
    mode: "sslip" as const,
    hostname: "api-203-0-113-10.sslip.io",
    source: "service-ip" as const,
    observedAt: "2026-08-09T10:00:00Z",
  };

  it("selects only the exact server-derived hostname and removes External DNS", async () => {
    const user = userEvent.setup();
    const initial = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    const onChange = vi.fn();
    render(
      <GuidedConfigForm
        initial={initial}
        onChange={onChange}
        sslipHostnameEnabled
        sslipHostnamePreview={preview}
      />,
    );

    const option = screen.getByRole("radio", {
      name: /Free sslip\.io hostname/i,
    });
    expect(option).toBeEnabled();
    await user.click(option);

    const hostname = screen.getByRole("textbox", { name: "Hostname" });
    await waitFor(() => {
      expect(hostname).toHaveValue(preview.hostname);
      expect(hostname).toHaveAttribute("readonly");
      expect(onChange.mock.lastCall?.[0]).toMatchObject({
        dnsMode: "sslip",
        host: preview.hostname,
        dnsIntegrationRef: "",
      });
    });
    expect(screen.queryByLabelText(/^DNS integration/)).toBeNull();
    const sslipStatus = screen
      .getByText(preview.hostname)
      .closest('[role="status"]');
    expect(sslipStatus).toHaveTextContent(
      /public ingress IP and exact fresh edge runtime readiness/i,
    );
    expect(sslipStatus).toHaveTextContent(
      /Dynamic ALB\/load-balancer hostnames are not eligible/i,
    );
  });

  it("fails closed for a new route but preserves an existing sslip route for inspection", () => {
    const newRoute = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    const { unmount } = render(
      <GuidedConfigForm
        initial={newRoute}
        onChange={vi.fn()}
        sslipHostnameEnabled
        sslipHostnameError="No fresh public ingress observation is available."
      />,
    );
    expect(
      screen.getByRole("radio", { name: /Free sslip\.io hostname/i }),
    ).toBeDisabled();

    unmount();
    render(
      <GuidedConfigForm
        initial={{
          ...newRoute,
          dnsMode: "sslip",
          host: "existing-203-0-113-20.sslip.io",
        }}
        onChange={vi.fn()}
        sslipHostnameEnabled
        sslipHostnameError="No fresh public ingress observation is available."
      />,
    );

    expect(
      screen.getByRole("radio", { name: /Free sslip\.io hostname/i }),
    ).toBeEnabled();
    expect(screen.getByRole("textbox", { name: "Hostname" })).toHaveValue(
      "existing-203-0-113-20.sslip.io",
    );
    expect(screen.getByRole("textbox", { name: "Hostname" })).toHaveAttribute(
      "readonly",
    );
    expect(
      screen
        .getByText("sslip.io hostname unavailable")
        .closest('[role="status"]'),
    ).toHaveTextContent("No fresh public ingress observation is available.");
  });

  it("edits separate collapsed Kubernetes resource overrides", async () => {
    const user = userEvent.setup();
    const initial = guidedConfigFromYaml(defaultConfigYaml({ name: "api" }));
    const onChange = vi.fn();
    render(<GuidedConfigForm initial={initial} onChange={onChange} />);

    expect(
      screen.getByText("Advanced Kubernetes YAML overrides").closest("details"),
    ).not.toHaveAttribute("open");
    await user.click(screen.getByText("Advanced Kubernetes YAML overrides"));
    expect(
      screen.getByText(/Advanced YAML wins over matching Guided fields/i),
    ).toBeInTheDocument();

    const serviceAccount = screen.getByRole("textbox", {
      name: "ServiceAccount override YAML",
    });
    await user.clear(serviceAccount);
    fireEvent.change(serviceAccount, {
      target: {
        value:
          "metadata:\n  annotations:\n    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/app",
      },
    });

    await waitFor(() =>
      expect(onChange.mock.lastCall?.[0].resourceOverrides).toMatchObject({
        deploymentYaml: "{}",
        serviceYaml: "{}",
        ingressYaml: "{}",
        serviceAccountYaml:
          "metadata:\n  annotations:\n    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/app",
      }),
    );
  });
});
