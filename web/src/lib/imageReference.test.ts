import { describe, expect, it } from "vitest";
import {
  isCanonicalImmutableImage,
  isCanonicalTaggedImage,
} from "./imageReference";

describe("existing-image reference grammar", () => {
  it("accepts canonical registries, repositories, tags, and digests", () => {
    expect(
      isCanonicalTaggedImage(
        "registry.example.test:5443/team/api_server:release-2026.08",
      ),
    ).toBe(true);
    expect(
      isCanonicalImmutableImage(
        `registry.example.test/team/api@sha256:${"a".repeat(64)}`,
      ),
    ).toBe(true);
  });

  it.each([
    "Registry.example.test/team/api:release",
    "registry.example.test:0/team/api:release",
    "registry.example.test:65536/team/api:release",
    "registry.example.test/team//api:release",
    "registry.example.test/team/../api:release",
    "registry.example.test/team/api:",
    "registry.example.test/team/api@sha256:not-a-digest",
  ])("rejects non-canonical or ambiguous input %s", (reference) => {
    expect(isCanonicalTaggedImage(reference)).toBe(false);
    expect(isCanonicalImmutableImage(reference)).toBe(false);
  });
});
