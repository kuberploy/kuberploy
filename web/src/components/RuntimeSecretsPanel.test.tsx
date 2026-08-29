import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  Capability,
  Environment,
  Project,
  RuntimeSecretBindingDetail,
} from "../api/types";
import { RuntimeSecretsPanel } from "./RuntimeSecretsPanel";

function memoryStorage(): Storage {
  const values = new Map<string, string>();
  return {
    get length() {
      return values.size;
    },
    clear: () => values.clear(),
    getItem: (key) => values.get(key) ?? null,
    key: (index) => Array.from(values.keys())[index] ?? null,
    removeItem: (key) => values.delete(key),
    setItem: (key, value) => values.set(key, value),
  };
}

beforeEach(() => {
  vi.stubGlobal("localStorage", memoryStorage());
  vi.stubGlobal("sessionStorage", memoryStorage());
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const application: Application = {
  id: "application-payments",
  projectId: "project-payments",
  name: "Payments API",
};
const project: Project = {
  id: "project-payments",
  name: "Payments",
  teamId: "team-commerce",
};
const production: Environment = {
  id: "environment-production",
  projectId: project.id,
  name: "Production",
  namespace: "payments-production",
};
const staging: Environment = {
  id: "environment-staging",
  projectId: project.id,
  name: "Staging",
  namespace: "payments-staging",
};
const unrelated: Environment = {
  id: "environment-unrelated",
  projectId: "project-unrelated",
  name: "Unrelated",
  namespace: "unrelated-production",
};

const detail: RuntimeSecretBindingDetail = {
  id: "binding-database",
  applicationId: application.id,
  environmentId: production.id,
  name: "database-credentials",
  provider: "external-secrets",
  state: "ready",
  activeVersion: 3,
  createdBy: "user-admin",
  createdAt: "2026-08-09T00:00:00Z",
  updatedAt: "2026-08-09T00:05:00Z",
  versions: [
    {
      id: "version-3",
      number: 3,
      state: "active",
      deliveries: [
        {
          sourceKey: "password",
          kind: "environment",
          environmentName: "DATABASE_PASSWORD",
        },
      ],
      createdAt: "2026-08-09T00:00:00Z",
      updatedAt: "2026-08-09T00:05:00Z",
    },
  ],
};
const stagingDetail: RuntimeSecretBindingDetail = {
  ...detail,
  id: "binding-staging",
  environmentId: staging.id,
  name: "staging-credentials",
};

function capability(
  action: string,
  scopeId = production.id,
  scopeType: Capability["scopeType"] = "environment",
): Capability {
  return { role: "project-admin", scopeType, scopeId, actions: [action] };
}

function renderPanel({
  capabilities = [
    capability("secret-bindings:read"),
    capability("secret-bindings:create"),
    capability("secret-bindings:rotate"),
    capability("secret-bindings:delete"),
  ],
  environments = [production],
  preferredEnvironmentId,
  featureEnabled = true,
  humanSession = true,
}: {
  capabilities?: Capability[];
  environments?: Environment[];
  preferredEnvironmentId?: string;
  featureEnabled?: boolean;
  humanSession?: boolean;
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const Wrapper = ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  const rendered = render(
    <RuntimeSecretsPanel
      application={application}
      environments={environments}
      preferredEnvironmentId={preferredEnvironmentId}
      project={project}
      capabilities={capabilities}
      featureEnabled={featureEnabled}
      humanSession={humanSession}
    />,
    { wrapper: Wrapper },
  );
  return { queryClient, rerender: rendered.rerender };
}

it("opens on the deployment Environment when more than one is readable", async () => {
  vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({ items: [] });
  renderPanel({
    environments: [production, staging],
    preferredEnvironmentId: staging.id,
    capabilities: [
      capability("secret-bindings:read", project.id, "project"),
      capability("secret-bindings:create", project.id, "project"),
      capability("secret-bindings:rotate", project.id, "project"),
      capability("secret-bindings:delete", project.id, "project"),
    ],
  });

  expect(
    await screen.findByRole("combobox", {
      name: "Runtime secret environment",
    }),
  ).toHaveValue(staging.id);
  expect(api.runtimeSecretBindings).toHaveBeenCalledWith(
    application.id,
    staging.id,
  );
});

async function fillCreate(
  user: ReturnType<typeof userEvent.setup>,
  value: string,
  options: {
    name?: string;
    environmentName?: string;
  } = {},
) {
  const name = options.name ?? "database-credentials";
  const environmentName = options.environmentName ?? "DATABASE_PASSWORD";
  await user.type(
    screen.getByRole("textbox", { name: "Runtime secret binding name" }),
    name,
  );
  await user.type(
    screen.getByRole("textbox", { name: "Create secret key 1" }),
    "password",
  );
  await user.type(screen.getByLabelText("Create write-only value 1"), value);
  await user.type(
    screen.getByRole("textbox", { name: "Create delivery source 1" }),
    "password",
  );
  await user.type(
    screen.getByRole("textbox", { name: "Create environment variable 1" }),
    environmentName,
  );
}

describe("runtime-secret management panel", () => {
  it("stays hidden when the runtime-secret feature is false", async () => {
    const list = vi.spyOn(api, "runtimeSecretBindings");

    renderPanel({ featureEnabled: false });

    expect(screen.queryByText("Variables & secrets")).not.toBeInTheDocument();
    await Promise.resolve();
    expect(list).not.toHaveBeenCalled();
  });

  it("does not infer metadata access from broad actions or wrong scopes", async () => {
    const list = vi.spyOn(api, "runtimeSecretBindings");
    const capabilities = [
      capability("secret-bindings:read", "other-environment"),
      capability("secret-bindings:create"),
    ];

    renderPanel({ capabilities });

    expect(
      screen.getByText("Secret metadata access not granted"),
    ).toBeInTheDocument();
    expect(list).not.toHaveBeenCalled();
  });

  it("lists only environments covered by the exact read capability", async () => {
    const list = vi
      .spyOn(api, "runtimeSecretBindings")
      .mockResolvedValue({ items: [] });

    renderPanel({
      capabilities: [capability("secret-bindings:read", staging.id)],
      environments: [production, staging, unrelated],
    });

    await openSelect(screen.getByRole("combobox", { name: /environment/i }));
    expect(
      await screen.findByRole("option", {
        name: "Staging · payments-staging",
      }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: /Production/ }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: /Unrelated/ }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "New binding" }),
    ).not.toBeInTheDocument();
    expect(list).toHaveBeenCalledWith(application.id, staging.id);
  });

  it("clears and uncaches write-only values immediately after success", async () => {
    const user = userEvent.setup();
    const secretValue = "s3cr3t-success-$(never-log)";
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({ items: [] });
    let receivedExactPayload = false;
    const create = vi
      .spyOn(api, "createRuntimeSecretBinding")
      .mockImplementation(async (_applicationId, input) => {
        receivedExactPayload =
          input.environmentId === production.id &&
          input.values.password === secretValue &&
          input.deliveries[0]?.kind === "environment" &&
          input.deliveries[0].environmentName === "DATABASE_PASSWORD";
        return detail;
      });
    const { queryClient } = renderPanel();

    await user.click(
      await screen.findByRole("button", { name: "New binding" }),
    );
    await fillCreate(user, secretValue);
    await user.click(
      screen.getByRole("button", { name: "Ingest write-only values" }),
    );

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(receivedExactPayload).toBe(true);
    expect(create.mock.calls[0]?.[1].provider).toBe("sealed-secrets");
    expect(screen.queryByDisplayValue(secretValue)).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(secretValue);
    expect(JSON.stringify(create.mock.calls)).not.toContain(secretValue);
    expect(JSON.stringify(queryClient.getQueryCache().getAll())).not.toContain(
      secretValue,
    );
    expect(queryClient.getMutationCache().getAll()).toEqual([]);
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(window.location.href).not.toContain(secretValue);
  });

  it("does not advertise the unavailable External Secrets provider", async () => {
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({ items: [] });
    const user = userEvent.setup();
    renderPanel();

    await user.click(
      await screen.findByRole("button", { name: "New binding" }),
    );

    expect(
      screen.getByRole("textbox", { name: "Runtime secret provider" }),
    ).toHaveValue("Sealed Secrets");
    expect(screen.queryByText("External Secrets")).not.toBeInTheDocument();
  });

  it("polls asynchronous create readiness and pending rotation activation", async () => {
    const user = userEvent.setup();
    const versionOne = detail.versions[0]!;
    const versionTwo = {
      ...versionOne,
      id: "version-5",
      number: 5,
      state: "awaiting-readiness" as const,
    };
    const provisioning = {
      ...detail,
      state: "provisioning" as const,
      activeVersion: undefined,
    };
    const pendingRotation = {
      ...detail,
      state: "ready" as const,
      activeVersion: 3,
      versions: [versionOne, versionTwo],
    };
    const activatedRotation = {
      ...pendingRotation,
      activeVersion: 5,
      versions: [versionOne, { ...versionTwo, state: "active" as const }],
    };
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({
      items: [
        {
          ...detail,
          state: "provisioning",
          activeVersion: undefined,
        },
      ],
    });
    const getDetail = vi
      .spyOn(api, "runtimeSecretBinding")
      .mockResolvedValueOnce(provisioning)
      .mockResolvedValueOnce(pendingRotation)
      .mockResolvedValue(activatedRotation);
    renderPanel();

    await user.click(
      await screen.findByRole("button", { name: /database-credentials/i }),
    );
    await screen.findByText("Not active");
    await waitFor(
      () => expect(getDetail.mock.calls.length).toBeGreaterThanOrEqual(3),
      { timeout: 3_000 },
    );
    expect(
      screen.getByText("Active version").nextElementSibling,
    ).toHaveTextContent("5");
  });

  it("clears values and sanitizes errors while retaining only a stable retry key", async () => {
    const user = userEvent.setup();
    const secretValue = "s3cr3t-error-should-disappear";
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({ items: [] });
    const idempotencyKeys: string[] = [];
    const create = vi
      .spyOn(api, "createRuntimeSecretBinding")
      .mockImplementation(async (_applicationId, _input, idempotencyKey) => {
        idempotencyKeys.push(idempotencyKey);
        throw new Error(`hostile transport echoed ${secretValue}`);
      });
    const { queryClient } = renderPanel();

    await user.click(
      await screen.findByRole("button", { name: "New binding" }),
    );
    await fillCreate(user, secretValue);
    await user.click(
      screen.getByRole("button", { name: "Ingest write-only values" }),
    );

    expect(
      await screen.findByText(/write-only request failed/i),
    ).toBeInTheDocument();
    expect(screen.queryByDisplayValue(secretValue)).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(secretValue);
    expect(JSON.stringify(create.mock.calls)).not.toContain(secretValue);
    expect(JSON.stringify(queryClient.getQueryCache().getAll())).not.toContain(
      secretValue,
    );
    expect(queryClient.getMutationCache().getAll()).toEqual([]);

    await fillCreate(user, secretValue);
    await user.click(
      screen.getByRole("button", { name: "Ingest write-only values" }),
    );
    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
    expect(idempotencyKeys[0]).toBeTruthy();
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);

    await fillCreate(user, secretValue, {
      name: "other-credentials",
      environmentName: "OTHER_PASSWORD",
    });
    await user.click(
      screen.getByRole("button", { name: "Ingest write-only values" }),
    );
    await waitFor(() => expect(create).toHaveBeenCalledTimes(3));
    expect(idempotencyKeys[2]).not.toBe(idempotencyKeys[1]);
  });

  it("uses the observed active version for rotation and exact deletion confirmation", async () => {
    const user = userEvent.setup();
    const rotatedValue = "rotation-material-never-retained";
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({
      items: [detail],
    });
    vi.spyOn(api, "runtimeSecretBinding").mockResolvedValue(detail);
    let receivedExpectedVersion = 0;
    const rotate = vi
      .spyOn(api, "rotateRuntimeSecretBinding")
      .mockImplementation(async (_bindingId, input) => {
        receivedExpectedVersion = input.expectedActiveVersion;
        return detail;
      });
    const remove = vi
      .spyOn(api, "deleteRuntimeSecretBinding")
      .mockResolvedValue(undefined);
    renderPanel();

    await user.click(
      await screen.findByRole("button", { name: /database-credentials/i }),
    );
    await screen.findByText("Versions");
    await user.click(screen.getByRole("button", { name: "Rotate" }));
    await user.type(
      screen.getByRole("textbox", { name: "Rotate secret key 1" }),
      "password",
    );
    await user.type(
      screen.getByLabelText("Rotate write-only value 1"),
      rotatedValue,
    );
    await user.type(
      screen.getByRole("textbox", { name: "Rotate delivery source 1" }),
      "password",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Rotate environment variable 1" }),
      "DATABASE_PASSWORD",
    );
    await user.click(
      screen.getByRole("button", { name: "Ingest new version" }),
    );

    await waitFor(() => expect(rotate).toHaveBeenCalledOnce());
    expect(receivedExpectedVersion).toBe(3);
    expect(JSON.stringify(rotate.mock.calls)).not.toContain(rotatedValue);
    expect(screen.queryByDisplayValue(rotatedValue)).not.toBeInTheDocument();

    const confirmation = screen.getByRole("textbox", {
      name: "Exact runtime secret binding name confirmation",
    });
    await user.type(confirmation, "wrong-name");
    await user.click(screen.getByRole("button", { name: "Delete binding" }));
    expect(remove).not.toHaveBeenCalled();
    await user.clear(confirmation);
    await user.type(confirmation, detail.name);
    await user.click(screen.getByRole("button", { name: "Delete binding" }));
    await waitFor(() => expect(remove).toHaveBeenCalledOnce());
    expect(remove.mock.calls[0]?.[0]).toBe(detail.id);
    expect(remove.mock.calls[0]?.[1]).toMatch(/^[A-Za-z0-9._:-]{16,128}$/);
  });

  it("does not retry a failed rotation after the observed active version changes", async () => {
    const user = userEvent.setup();
    const firstValue = "rotation-first-value";
    const secondValue = "rotation-second-value";
    const updatedDetail = {
      ...detail,
      activeVersion: 4,
      versions: [
        ...detail.versions,
        { ...detail.versions[0]!, id: "version-4", number: 4 },
      ],
    };
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({
      items: [detail],
    });
    vi.spyOn(api, "runtimeSecretBinding")
      .mockResolvedValueOnce(detail)
      .mockResolvedValue(updatedDetail);
    const rotate = vi
      .spyOn(api, "rotateRuntimeSecretBinding")
      .mockRejectedValueOnce(new Error("conflict"))
      .mockResolvedValue(updatedDetail);
    const { queryClient } = renderPanel();

    await user.click(
      await screen.findByRole("button", { name: /database-credentials/i }),
    );
    await screen.findByText("Versions");
    await user.click(screen.getByRole("button", { name: "Rotate" }));
    await user.type(
      screen.getByRole("textbox", { name: "Rotate secret key 1" }),
      "password",
    );
    await user.type(
      screen.getByLabelText("Rotate write-only value 1"),
      firstValue,
    );
    await user.type(
      screen.getByRole("textbox", { name: "Rotate delivery source 1" }),
      "password",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Rotate environment variable 1" }),
      "DATABASE_PASSWORD",
    );
    await user.click(
      screen.getByRole("button", { name: "Ingest new version" }),
    );
    await screen.findByText(/write-only rotation failed/i);
    const firstKey = rotate.mock.calls[0]?.[2];

    queryClient.setQueryData(
      ["runtime-secret-bindings", application.id, production.id],
      { items: [{ ...detail, activeVersion: 4 }] },
    );
    await waitFor(() =>
      expect(screen.getByText("Version 4")).toBeInTheDocument(),
    );
    await user.click(screen.getByRole("button", { name: "Rotate" }));

    await user.type(
      screen.getByRole("textbox", { name: "Rotate secret key 1" }),
      "password",
    );
    await user.type(
      screen.getByLabelText("Rotate write-only value 1"),
      secondValue,
    );
    await user.type(
      screen.getByRole("textbox", { name: "Rotate delivery source 1" }),
      "password",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Rotate environment variable 1" }),
      "DATABASE_PASSWORD",
    );
    await user.click(
      screen.getByRole("button", { name: "Ingest new version" }),
    );
    await waitFor(() => expect(rotate).toHaveBeenCalledTimes(2));
    expect(rotate.mock.calls[1]?.[1].expectedActiveVersion).toBe(4);
    expect(rotate.mock.calls[1]?.[2]).not.toBe(firstKey);
  });

  it("ignores a pending delete completion after the environment changes", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "runtimeSecretBindings").mockImplementation(
      async (_applicationId, environmentId) => ({
        items: environmentId === production.id ? [detail] : [stagingDetail],
      }),
    );
    vi.spyOn(api, "runtimeSecretBinding").mockImplementation(
      async (bindingId) => (bindingId === detail.id ? detail : stagingDetail),
    );
    let resolveDelete: (() => void) | undefined;
    const remove = vi
      .spyOn(api, "deleteRuntimeSecretBinding")
      .mockImplementation(
        () =>
          new Promise<void>((resolve) => {
            resolveDelete = resolve;
          }),
      );
    renderPanel({
      environments: [production, staging],
      capabilities: [
        capability("secret-bindings:read", project.id, "project"),
        capability("secret-bindings:create", project.id, "project"),
        capability("secret-bindings:rotate", project.id, "project"),
        capability("secret-bindings:delete", project.id, "project"),
      ],
    });

    await user.click(
      await screen.findByRole("button", { name: /database-credentials/i }),
    );
    await screen.findByText("Versions");
    const confirmation = screen.getByRole("textbox", {
      name: "Exact runtime secret binding name confirmation",
    });
    await user.type(confirmation, detail.name);
    await user.click(screen.getByRole("button", { name: "Delete binding" }));
    await waitFor(() => expect(remove).toHaveBeenCalledOnce());

    await selectOption(
      screen.getByRole("combobox", { name: "Runtime secret environment" }),
      staging.id,
    );
    await user.click(
      await screen.findByRole("button", { name: /staging-credentials/i }),
    );
    await screen.findByText("Versions");

    resolveDelete?.();
    await waitFor(() =>
      expect(screen.getByText("Versions")).toBeInTheDocument(),
    );
    expect(
      screen.getByRole("button", { name: /staging-credentials/i }),
    ).toHaveClass("runtime-secret-binding--active");
  });

  it("clears stale environment and binding selection after access changes", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "runtimeSecretBindings").mockImplementation(
      async (_applicationId, environmentId) => ({
        items: environmentId === staging.id ? [stagingDetail] : [],
      }),
    );
    vi.spyOn(api, "runtimeSecretBinding").mockResolvedValue(stagingDetail);
    const { rerender } = renderPanel({
      environments: [production, staging],
      capabilities: [
        capability("secret-bindings:read", project.id, "project"),
        capability("secret-bindings:delete", project.id, "project"),
      ],
    });

    await selectOption(
      await screen.findByRole("combobox", {
        name: "Runtime secret environment",
      }),
      staging.id,
    );
    await user.click(
      await screen.findByRole("button", { name: /staging-credentials/i }),
    );
    await screen.findByText("Versions");

    rerender(
      <RuntimeSecretsPanel
        application={application}
        environments={[production]}
        project={project}
        capabilities={[
          capability("secret-bindings:read", project.id, "project"),
          capability("secret-bindings:delete", project.id, "project"),
        ]}
        featureEnabled
        humanSession
      />,
    );

    await waitFor(() =>
      expect(
        screen.getByRole("combobox", { name: "Runtime secret environment" }),
      ).toHaveValue(production.id),
    );
    expect(
      screen.queryByRole("button", { name: /staging-credentials/i }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("Versions")).not.toBeInTheDocument();
  });

  it("keeps a service-account session metadata-only despite mutation actions", async () => {
    vi.spyOn(api, "runtimeSecretBindings").mockResolvedValue({ items: [] });
    renderPanel({ humanSession: false });

    expect(
      await screen.findByText("Metadata-only automation session"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "New binding" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Create write-only binding" }),
    ).not.toBeInTheDocument();
  });
});
