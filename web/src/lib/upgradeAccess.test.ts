import { describe, expect, it } from "vitest";
import { hasPlatformUpgradeCapability } from "./upgradeAccess";

describe("platform upgrade access", () => {
  it("requires an exact effective platform capability", () => {
    const action = "platform-releases:read" as const;
    expect(
      hasPlatformUpgradeCapability(
        [
          {
            scopeType: "platform",
            scopeId: "platform",
            actions: [action],
          },
        ],
        action,
      ),
    ).toBe(true);
    expect(
      hasPlatformUpgradeCapability(
        [
          {
            scopeType: "project",
            scopeId: "project-1",
            actions: [action],
          },
        ],
        action,
      ),
    ).toBe(false);
    expect(hasPlatformUpgradeCapability([], action)).toBe(false);
  });
});
