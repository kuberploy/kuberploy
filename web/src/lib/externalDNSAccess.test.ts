import { describe, expect, it } from "vitest";
import type { Capability } from "../api/types";
import {
  externalDNSHostnameAllowed,
  hasExternalDNSPlatformCapability,
} from "./externalDNSAccess";

describe("External DNS access and suffix matching", () => {
  it("requires an exact platform-admin capability, not broad actions", () => {
    const exact: Capability = {
      role: "platform-admin",
      scopeType: "platform",
      scopeId: "platform",
      actions: ["external-dns-integrations:read"],
    };
    expect(
      hasExternalDNSPlatformCapability(
        [exact],
        "external-dns-integrations:read",
      ),
    ).toBe(true);
    for (const capability of [
      { ...exact, role: "project-admin" as const },
      { ...exact, scopeType: "project" as const, scopeId: "project-a" },
      { ...exact, scopeId: "not-platform" },
      { ...exact, actions: ["platform-upgrades:create"] },
    ]) {
      expect(
        hasExternalDNSPlatformCapability(
          [capability],
          "external-dns-integrations:read",
        ),
      ).toBe(false);
    }
  });

  it("matches DNS suffixes only at a label boundary", () => {
    expect(
      externalDNSHostnameAllowed("API.Example.com.", ["example.com"]),
    ).toBe(true);
    expect(externalDNSHostnameAllowed("example.com", ["example.com"])).toBe(
      true,
    );
    expect(
      externalDNSHostnameAllowed("evil-example.com", ["example.com"]),
    ).toBe(false);
    expect(
      externalDNSHostnameAllowed("example.com.evil.test", ["example.com"]),
    ).toBe(false);
  });
});
