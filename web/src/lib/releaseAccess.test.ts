import { describe, expect, it } from "vitest";
import { hasPlatformReleaseCapability } from "./releaseAccess";

const action = "platform-releases:read" as const;

describe("platform release access", () => {
  it("accepts only the exact platform capability", () => {
    expect(
      hasPlatformReleaseCapability(
        [
          {
            role: "platform-admin",
            scopeType: "platform",
            scopeId: "platform",
            actions: [action],
          },
        ],
        action,
      ),
    ).toBe(true);
    expect(
      hasPlatformReleaseCapability(
        [
          {
            role: "project-admin",
            scopeType: "project",
            scopeId: "project-1",
            actions: [action],
          },
        ],
        action,
      ),
    ).toBe(false);
    expect(hasPlatformReleaseCapability([], action)).toBe(false);
  });
});
