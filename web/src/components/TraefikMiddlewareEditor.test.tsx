import { openSelect, selectOption } from "../test/selectOption";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api/client";
import {
  defaultGuidedTraefikMiddleware,
  traefikMiddlewareKinds,
  type GuidedTraefikMiddleware,
} from "../lib/traefikMiddleware";
import { TraefikMiddlewareEditor } from "./TraefikMiddlewareEditor";

afterEach(cleanup);

function Harness({
  initialDefinitions = [],
  initialRefs = [],
  issue = "",
  readOnly = false,
  editingUnavailableReason,
  applicationId,
  environmentId,
  reusableProfilesEnabled = false,
}: {
  initialDefinitions?: GuidedTraefikMiddleware[];
  initialRefs?: string[];
  issue?: string;
  readOnly?: boolean;
  editingUnavailableReason?: string;
  applicationId?: string;
  environmentId?: string;
  reusableProfilesEnabled?: boolean;
}) {
  const [state, setState] = useState({
    definitions: initialDefinitions,
    refs: initialRefs,
  });
  return (
    <TraefikMiddlewareEditor
      definitions={state.definitions}
      refs={state.refs}
      issue={issue}
      routeEnabled
      readOnly={readOnly}
      editingUnavailableReason={editingUnavailableReason}
      applicationId={applicationId}
      environmentId={environmentId}
      reusableProfilesEnabled={reusableProfilesEnabled}
      onChange={setState}
    />
  );
}

describe("Traefik middleware Guided editor", () => {
  afterEach(() => vi.restoreAllMocks());

  it("surfaces reusable profile failures and retries the profile query", async () => {
    const user = userEvent.setup();
    const profiles = vi
      .spyOn(api, "assignedMiddlewareProfiles")
      .mockRejectedValueOnce(new Error("profile catalog unavailable"))
      .mockResolvedValue({ items: [] });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <Harness
          applicationId="application-1"
          environmentId="environment-1"
          reusableProfilesEnabled
        />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByText("Could not load reusable profiles"),
    ).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(profiles).toHaveBeenCalledTimes(2));
    expect(
      screen.queryByText("Could not load reusable profiles"),
    ).not.toBeInTheDocument();
  });

  it("offers every allowlisted family as bounded controls without a JSON input", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    const family = screen.getByLabelText("New middleware family");
    await openSelect(family);
    expect(screen.getAllByRole("option")).toHaveLength(
      traefikMiddlewareKinds.length,
    );
    await selectOption(family, "redirectRegex");
    await user.click(screen.getByRole("button", { name: /Add middleware/i }));

    expect(
      screen.getByLabelText("redirect-regex redirect regex"),
    ).toBeVisible();
    expect(
      screen.getByLabelText("redirect-regex redirect replacement"),
    ).toBeVisible();
    expect(screen.queryByLabelText(/middleware JSON/i)).toBeNull();
    expect(document.querySelector('input[type="password"]')).toBeNull();
  });

  it("reorders the exact route chain and reports duplicate names and refs", async () => {
    const user = userEvent.setup();
    render(
      <Harness
        initialDefinitions={[
          defaultGuidedTraefikMiddleware("headers", "security"),
          defaultGuidedTraefikMiddleware("retry", "retry-upstream"),
        ]}
        initialRefs={["security", "retry-upstream"]}
      />,
    );

    await user.click(
      screen.getByRole("button", {
        name: "Move route middleware retry-upstream up",
      }),
    );
    const routeSelectors = screen.getAllByRole("combobox", {
      name: /Route middleware [0-9]+/,
    });
    expect(routeSelectors[0]).toHaveValue("retry-upstream");
    expect(routeSelectors[1]).toHaveValue("security");

    const secondName = screen.getByLabelText("Middleware 2 name");
    await user.clear(secondName);
    await user.type(secondName, "security");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      /Middleware name security is duplicated/i,
    );
  });

  it("keeps focus on the middleware that was moved, not on the row position", async () => {
    const user = userEvent.setup();
    render(
      <Harness
        initialDefinitions={[
          defaultGuidedTraefikMiddleware("headers", "security"),
          defaultGuidedTraefikMiddleware("retry", "retry-upstream"),
          defaultGuidedTraefikMiddleware("compress", "compress-responses"),
        ]}
        initialRefs={["security", "retry-upstream", "compress-responses"]}
      />,
    );

    // Chain rows are keyed by position, so a reorder rewrites the row in place
    // instead of moving it. Without an explicit hand-off, focus would stay on
    // the position and the next Enter would move whichever entry landed there.
    await user.click(
      screen.getByRole("button", {
        name: "Move route middleware compress-responses up",
      }),
    );
    expect(document.activeElement).toBe(
      screen.getByRole("button", {
        name: "Move route middleware compress-responses up",
      }),
    );

    await user.keyboard("{Enter}");
    const routeSelectors = screen.getAllByRole("combobox", {
      name: /Route middleware [0-9]+/,
    });
    expect(routeSelectors[0]).toHaveValue("compress-responses");
  });

  it("preserves Advanced-only state and capability-gated state as disabled inspection", () => {
    const { rerender } = render(
      <Harness issue="The original YAML is preserved; use Advanced YAML." />,
    );
    expect(
      screen.getByText("The original YAML is preserved; use Advanced YAML."),
    ).toBeVisible();
    expect(
      screen.getByRole("button", { name: /Add middleware/i }),
    ).toBeDisabled();

    rerender(
      <Harness
        key="capability-gated"
        initialDefinitions={[
          defaultGuidedTraefikMiddleware("headers", "security"),
        ]}
        editingUnavailableReason="Runtime capability is unavailable."
      />,
    );
    expect(screen.getByLabelText("Middleware 1 name")).toHaveValue("security");
    expect(screen.getByLabelText("Middleware 1 name")).toBeDisabled();
  });
});
