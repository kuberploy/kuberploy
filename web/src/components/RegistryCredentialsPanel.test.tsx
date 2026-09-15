import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { PropsWithChildren } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import type {
  Application,
  Capability,
  RegistryCredentialBindingDetail,
  Environment,
  Project,
} from "../api/types";
import { RegistryCredentialsPanel } from "./RegistryCredentialsPanel";

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
const detail: RegistryCredentialBindingDetail = {
  id: "registry-credential-private-registry",
  applicationId: application.id,
  environmentId: production.id,
  name: "private-registry",
  state: "ready",
  activeVersion: 1,
  createdBy: "user-admin",
  createdAt: "2026-09-15T00:00:00Z",
  updatedAt: "2026-09-15T00:05:00Z",
  versions: [
    {
      number: 1,
      host: "registry.example.test",
      username: "ci-bot",
      createdBy: "user-admin",
      createdAt: "2026-09-15T00:00:00Z",
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
  featureEnabled = true,
  humanSession = true,
  environments = [production],
  capabilities = [
    capability("registry-credential-bindings:read"),
    capability("registry-credential-bindings:create"),
    capability("registry-credential-bindings:rotate"),
    capability("registry-credential-bindings:delete"),
  ],
}: {
  featureEnabled?: boolean;
  humanSession?: boolean;
  environments?: Environment[];
  capabilities?: Capability[];
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const Wrapper = ({ children }: PropsWithChildren) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  const rendered = render(
    <RegistryCredentialsPanel
      application={application}
      environments={environments}
      project={project}
      capabilities={capabilities}
      featureEnabled={featureEnabled}
      humanSession={humanSession}
    />,
    { wrapper: Wrapper },
  );
  return { queryClient, rerender: rendered.rerender };
}

describe("registry credentials panel", () => {
  it("stays hidden when the feature is not advertised", async () => {
    const list = vi.spyOn(api, "registryCredentialBindings");
    renderPanel({ featureEnabled: false });
    expect(
      screen.queryByText("Registry pull credentials"),
    ).not.toBeInTheDocument();
    await Promise.resolve();
    expect(list).not.toHaveBeenCalled();
  });

  it("requires an interactive session even when scoped actions are present", () => {
    const list = vi.spyOn(api, "registryCredentialBindings");
    renderPanel({ humanSession: false });
    expect(
      screen.getByText("Interactive session required"),
    ).toBeInTheDocument();
    expect(list).not.toHaveBeenCalled();
  });

  it("clears and uncaches the write-only password immediately after success", async () => {
    const user = userEvent.setup();
    const password = "request-only-secret-token";
    vi.spyOn(api, "registryCredentialBindings").mockResolvedValue({
      items: [],
    });
    let receivedExactPayload = false;
    const create = vi
      .spyOn(api, "createRegistryCredentialBinding")
      .mockImplementation(async (_applicationID, input) => {
        receivedExactPayload =
          input.environmentId === production.id &&
          input.name === detail.name &&
          input.host === "registry.example.test" &&
          input.username === "ci-bot" &&
          input.password === password;
        return detail;
      });
    const { queryClient } = renderPanel();

    await user.click(
      await screen.findByRole("button", { name: "New credential" }),
    );
    expect(
      screen.queryByText("No registry pull credentials"),
    ).not.toBeInTheDocument();
    await user.type(
      screen.getByRole("textbox", { name: "Registry credential binding name" }),
      detail.name,
    );
    await user.type(
      screen.getByRole("textbox", {
        name: "Create credential registry host",
      }),
      "registry.example.test",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Create credential username" }),
      "ci-bot",
    );
    await user.type(
      screen.getByLabelText("Create credential password"),
      password,
    );
    await user.click(
      screen.getByRole("button", { name: "Validate and seal" }),
    );

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(receivedExactPayload).toBe(true);
    expect(screen.queryByDisplayValue(password)).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(password);
    expect(JSON.stringify(create.mock.calls)).not.toContain(password);
    expect(JSON.stringify(queryClient.getQueryCache().getAll())).not.toContain(
      password,
    );
    expect(queryClient.getMutationCache().getAll()).toEqual([]);
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(window.location.href).not.toContain(password);
  });

  it("does not retain a hostile echoed password and reuses only the stable idempotency key", async () => {
    const user = userEvent.setup();
    const password = "hostile-error-secret";
    vi.spyOn(api, "registryCredentialBindings").mockResolvedValue({
      items: [],
    });
    const keys: string[] = [];
    const create = vi
      .spyOn(api, "createRegistryCredentialBinding")
      .mockImplementation(async (_applicationID, _input, key) => {
        keys.push(key);
        throw new Error(`hostile transport echoed ${password}`);
      });
    renderPanel();
    await user.click(
      await screen.findByRole("button", { name: "New credential" }),
    );
    await user.type(
      screen.getByRole("textbox", { name: "Registry credential binding name" }),
      detail.name,
    );

    for (let attempt = 0; attempt < 2; attempt += 1) {
      await user.type(
        screen.getByRole("textbox", {
          name: "Create credential registry host",
        }),
        "registry.example.test",
      );
      await user.type(
        screen.getByRole("textbox", { name: "Create credential username" }),
        "ci-bot",
      );
      await user.type(screen.getByLabelText("Create credential password"), password);
      await user.click(
        screen.getByRole("button", { name: "Validate and seal" }),
      );
      await waitFor(() => expect(create).toHaveBeenCalledTimes(attempt + 1));
    }

    expect(keys[0]).toBeTruthy();
    expect(keys[1]).toBe(keys[0]);
    expect(document.body).not.toHaveTextContent(password);
    expect(JSON.stringify(create.mock.calls)).not.toContain(password);
    expect(
      screen.getByText(/write-only registry credential request failed/i),
    ).toBeInTheDocument();
  });

  it("uses observed active version for rotation and exact delete confirmation", async () => {
    const user = userEvent.setup();
    vi.spyOn(api, "registryCredentialBindings").mockResolvedValue({
      items: [detail],
    });
    vi.spyOn(api, "registryCredentialBinding").mockResolvedValue(detail);
    let expectedVersion = 0;
    const rotate = vi
      .spyOn(api, "rotateRegistryCredentialBinding")
      .mockImplementation(async (_bindingID, input) => {
        expectedVersion = input.expectedActiveVersion;
        return detail;
      });
    const remove = vi
      .spyOn(api, "deleteRegistryCredentialBinding")
      .mockResolvedValue(undefined);
    renderPanel();

    await user.click(
      await screen.findByRole("button", { name: /private-registry/i }),
    );
    await screen.findByText("Public attestations");
    await user.type(
      screen.getByRole("textbox", { name: "Rotate credential registry host" }),
      "registry.example.test",
    );
    await user.type(
      screen.getByRole("textbox", { name: "Rotate credential username" }),
      "ci-bot",
    );
    await user.type(
      screen.getByLabelText("Rotate credential password"),
      "new-token",
    );
    await user.click(
      screen.getByRole("button", { name: "Validate and rotate" }),
    );
    await waitFor(() => expect(rotate).toHaveBeenCalledOnce());
    expect(expectedVersion).toBe(1);
    expect(JSON.stringify(rotate.mock.calls)).not.toContain("new-token");

    const confirmation = screen.getByRole("textbox", {
      name: "Exact registry credential binding name confirmation",
    });
    await user.type(confirmation, "wrong-name");
    await user.click(
      screen.getByRole("button", { name: "Delete credential" }),
    );
    expect(remove).not.toHaveBeenCalled();
    await user.clear(confirmation);
    await user.type(confirmation, detail.name);
    await user.click(
      screen.getByRole("button", { name: "Delete credential" }),
    );
    await waitFor(() => expect(remove).toHaveBeenCalledOnce());
    expect(remove.mock.calls[0]?.[0]).toBe(detail.id);
    expect(remove.mock.calls[0]?.[1]).toMatch(/^[A-Za-z0-9._:-]{16,128}$/);
  });
});
