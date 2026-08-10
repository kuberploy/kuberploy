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
  featureEnabled = true,
  humanSession = true,
}: {
  capabilities?: Capability[];
  environments?: Environment[];
  featureEnabled?: boolean;
  humanSession?: boolean;
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const Wrapper = ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  render(
    <RuntimeSecretsPanel
      application={application}
      environments={environments}
      project={project}
      capabilities={capabilities}
      featureEnabled={featureEnabled}
      humanSession={humanSession}
    />,
    { wrapper: Wrapper },
  );
  return queryClient;
}

async function fillCreate(
  user: ReturnType<typeof userEvent.setup>,
  value: string,
) {
  await user.type(
    screen.getByRole("textbox", { name: "Runtime secret binding name" }),
    "database-credentials",
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
    "DATABASE_PASSWORD",
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
    const queryClient = renderPanel();

    await user.click(
      await screen.findByRole("button", { name: "New binding" }),
    );
    await fillCreate(user, secretValue);
    await user.click(
      screen.getByRole("button", { name: "Ingest write-only values" }),
    );

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(receivedExactPayload).toBe(true);
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
    const queryClient = renderPanel();

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
    await screen.findByText("Immutable versions");
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
