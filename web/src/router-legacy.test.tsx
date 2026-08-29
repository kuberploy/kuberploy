import { render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const { navigateComponent } = vi.hoisted(() => ({
  navigateComponent: vi.fn(() => null),
}));

vi.mock("@tanstack/react-router", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-router")>()),
  Navigate: navigateComponent,
}));

import { LegacyBuildsRedirect, LegacySettingsRedirect } from "./router";

afterEach(() => vi.clearAllMocks());

describe("legacy settings routes", () => {
  it("replaces stale settings URLs with the current settings dashboard", () => {
    render(<LegacySettingsRedirect />);

    expect(navigateComponent).toHaveBeenCalledWith(
      expect.objectContaining({ to: "/setup", replace: true }),
      undefined,
    );
  });

  it("moves the former global builds workspace to Git providers", () => {
    render(<LegacyBuildsRedirect />);

    expect(navigateComponent).toHaveBeenCalledWith(
      expect.objectContaining({ to: "/git", replace: true }),
      undefined,
    );
  });
});
